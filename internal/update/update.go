// Package update lets a node replace its own binary (which includes the embedded Xray) with a signed
// release. The panel only names a version; the node builds the download address itself, checks an
// Ed25519 signature against the key compiled into it, runs the new binary once to make sure it works
// on this host, and only then installs it. The next start of the node hands over to the installed
// binary; a new binary that never reaches the panel is rolled back to the one in the image.
package update

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed signing.pub
var signingPub string

const (
	// DefaultRepo is where releases are downloaded from.
	DefaultRepo = "dest1256/d256-node"
	maxBinary   = 256 << 20
	maxSig      = 4 << 10
	sanityRun   = 15 * time.Second
)

// DefaultBaseURL is the host that serves release assets. It is a variable only so that the end-to-end test
// can point a test build at a local server (-ldflags "-X .../internal/update.DefaultBaseURL=http://..."); no
// configuration or panel message can change it.
var DefaultBaseURL = "https://github.com"

var (
	// ErrDisabled means the build has no signing key, so no update can be verified.
	ErrDisabled = errors.New("updates are disabled: this build has no release signing key")

	versionRe = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$`)
	repoRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
)

// ValidVersion reports whether v is a release tag like v1.2.3 (optionally with a pre-release suffix).
// Only such a string is ever put into a download address.
func ValidVersion(v string) bool { return len(v) <= 40 && versionRe.MatchString(v) }

// ValidRepo reports whether r looks like owner/name.
func ValidRepo(r string) bool { return repoRe.MatchString(r) }

// Compare orders two release tags: -1, 0 or 1. A pre-release is lower than its release.
// Anything that is not a valid tag (like "dev") sorts below every valid tag.
func Compare(a, b string) int {
	av, aok := parse(a)
	bv, bok := parse(b)
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	}
	for i := 0; i < 3; i++ {
		if av.n[i] != bv.n[i] {
			if av.n[i] < bv.n[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case av.pre == bv.pre:
		return 0
	case av.pre == "":
		return 1
	case bv.pre == "":
		return -1
	case av.pre < bv.pre:
		return -1
	}
	return 1
}

type semver struct {
	n   [3]int
	pre string
}

func parse(v string) (semver, bool) {
	if !ValidVersion(v) {
		return semver{}, false
	}
	core, pre, _ := strings.Cut(v[1:], "-")
	var s semver
	for i, part := range strings.Split(core, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return semver{}, false
		}
		s.n[i] = n
	}
	s.pre = pre
	return s, true
}

// Updater installs releases under <dir>/bin.
type Updater struct {
	// Dir is the node's state directory; binaries live in Dir/bin.
	Dir string
	// Current is the version of the running binary.
	Current string
	// Repo and BaseURL select the release source; the defaults are the only production values.
	Repo, BaseURL string
	// PubKey verifies release signatures; nil disables updates.
	PubKey ed25519.PublicKey
	// Client downloads the assets (default: a client that only follows https redirects).
	Client *http.Client
	// GOOS and GOARCH name the asset (default: this process).
	GOOS, GOARCH string
}

// New returns an Updater for the node whose state lives in dir, with the build's signing key.
func New(dir, current string) *Updater {
	return &Updater{
		Dir: dir, Current: current, Repo: DefaultRepo, BaseURL: DefaultBaseURL,
		PubKey: PublicKey(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	}
}

// PublicKey returns the key compiled into this build, or nil when none is configured.
func PublicKey() ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signingPub))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}

// Enabled reports whether this build can verify updates.
func (u *Updater) Enabled() bool { return len(u.PubKey) == ed25519.PublicKeySize }

func (u *Updater) binDir() string { return filepath.Join(u.Dir, "bin") }

// SignedMessage is what a release signature covers: it binds the version, the platform and the content
// hash, so a validly signed old binary cannot be passed off as a new version or another architecture.
func SignedMessage(version, goos, goarch, sha256hex string) []byte {
	return []byte(fmt.Sprintf("d256-node\n%s\n%s/%s\n%s\n", version, goos, goarch, sha256hex))
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{
		Timeout: 15 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return errors.New("refusing a redirect to a non-https address")
			}
			return nil
		},
	}
}

func (u *Updater) assetURL(version, suffix string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/d256-node_%s_%s%s", strings.TrimRight(u.BaseURL, "/"), u.Repo, version, u.GOOS, u.GOARCH, suffix)
}

// Install downloads, verifies and installs the given release. It does not restart the node.
func (u *Updater) Install(ctx context.Context, version string) error {
	if !u.Enabled() {
		return ErrDisabled
	}
	if !ValidVersion(version) {
		return fmt.Errorf("%q is not a release version like v1.2.3", version)
	}
	if !ValidRepo(u.Repo) {
		return errors.New("invalid release repository")
	}
	if version == u.Current {
		return fmt.Errorf("already running %s", version)
	}
	dir := filepath.Join(u.binDir(), version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".d256-node-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	sum, err := u.fetchBinary(ctx, u.assetURL(version, ""), tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	sig, err := u.fetchSignature(ctx, u.assetURL(version, ".sig"))
	if err != nil {
		return err
	}
	if !ed25519.Verify(u.PubKey, SignedMessage(version, u.GOOS, u.GOARCH, sum), sig) {
		return errors.New("signature check failed: the release was not signed with this node's key")
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := sanityCheck(ctx, tmpPath, version); err != nil {
		return err
	}
	final := filepath.Join(dir, "d256-node")
	if err := os.Rename(tmpPath, final); err != nil {
		return err
	}

	prev, _ := readManifest(u.binDir())
	m := &Manifest{Version: version, SHA256: sum, Path: final}
	if prev != nil {
		m.Previous = prev.Version
	}
	if err := writeManifest(u.binDir(), m); err != nil {
		return err
	}
	u.prune(m)
	return nil
}

func (u *Updater) fetchBinary(ctx context.Context, url string, dst io.Writer) (string, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, maxBinary+1))
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if n == 0 {
		return "", errors.New("download: empty file")
	}
	if n > maxBinary {
		return "", errors.New("download: file is larger than the allowed size")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (u *Updater) fetchSignature(ctx context.Context, url string) ([]byte, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSig))
	if err != nil {
		return nil, err
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("the signature file is malformed")
	}
	return sig, nil
}

func (u *Updater) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "d256-node-updater")
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download: %s answered %d", url, resp.StatusCode)
	}
	return resp, nil
}

// sanityCheck runs the new binary once: it must start on this host and report the expected version.
func sanityCheck(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, sanityRun)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return fmt.Errorf("the new binary does not start on this host: %w", err)
	}
	first, _ := bufio.NewReader(strings.NewReader(string(out))).ReadString('\n')
	if !strings.HasPrefix(strings.TrimSpace(first), "d256-node "+version+" ") && strings.TrimSpace(first) != "d256-node "+version {
		return fmt.Errorf("the new binary reports %q, expected %s", strings.TrimSpace(first), version)
	}
	return nil
}

// prune keeps only the current and the previous release on disk.
func (u *Updater) prune(m *Manifest) {
	entries, err := os.ReadDir(u.binDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != m.Version && e.Name() != m.Previous {
			_ = os.RemoveAll(filepath.Join(u.binDir(), e.Name()))
		}
	}
}
