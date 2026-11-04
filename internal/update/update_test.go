package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// release is a fake release host: it serves binaries and signatures under the paths the updater builds.
type release struct {
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	mu     sync.Mutex
	assets map[string][]byte
	hits   []string
}

func newRelease(t *testing.T) (*release, *httptest.Server) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &release{pub: pub, priv: priv, assets: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.hits = append(r.hits, req.URL.Path)
		body, ok := r.assets[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return r, srv
}

// script is a stand-in binary: the sanity check runs `<file> version`.
func script(version string) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\necho \"d256-node %s (linux/amd64, go test, protocol 1)\"\n", version))
}

func (r *release) publish(version string, bin []byte, goos, goarch string, signWith ed25519.PrivateKey) {
	sum := sha256.Sum256(bin)
	sig := ed25519.Sign(signWith, SignedMessage(version, goos, goarch, hex.EncodeToString(sum[:])))
	base := fmt.Sprintf("/dest1256/d256-node/releases/download/%s/d256-node_%s_%s", version, goos, goarch)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assets[base] = bin
	r.assets[base+".sig"] = []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

func newUpdater(t *testing.T, r *release, srv *httptest.Server, current string) *Updater {
	t.Helper()
	return &Updater{
		Dir: t.TempDir(), Current: current, Repo: DefaultRepo, BaseURL: srv.URL,
		PubKey: r.pub, Client: srv.Client(), GOOS: "linux", GOARCH: "amd64",
	}
}

func TestInstallVerifiesAndInstallsARelease(t *testing.T) {
	r, srv := newRelease(t)
	r.publish("v1.2.0", script("v1.2.0"), "linux", "amd64", r.priv)
	u := newUpdater(t, r, srv, "v1.1.0")

	if err := u.Install(context.Background(), "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(u.binDir())
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v1.2.0" || m.Confirmed || m.Attempts != 0 {
		t.Fatalf("manifest = %+v", m)
	}
	if fi, err := os.Stat(m.Path); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("binary not installed or not executable: %v %v", fi, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(m.Path)); len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestInstallRejectsWhatIsNotSignedByTheNodesKey(t *testing.T) {
	r, srv := newRelease(t)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	r.publish("v1.2.0", script("v1.2.0"), "linux", "amd64", otherPriv)
	u := newUpdater(t, r, srv, "v1.1.0")
	if err := u.Install(context.Background(), "v1.2.0"); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("a foreign signature must be refused, got %v", err)
	}
	if _, err := readManifest(u.binDir()); err == nil {
		t.Fatal("nothing may be recorded for a refused update")
	}
	if _, err := os.Stat(filepath.Join(u.binDir(), "v1.2.0", "d256-node")); err == nil {
		t.Fatal("a refused binary was left in place")
	}
}

func TestSignatureBindsVersionAndPlatform(t *testing.T) {
	r, srv := newRelease(t)
	// A genuinely signed v1.0.0 binary is offered under the name of v1.3.0, and an arm64 signature for amd64.
	old := script("v1.3.0")
	sum := sha256.Sum256(old)
	sig := ed25519.Sign(r.priv, SignedMessage("v1.0.0", "linux", "amd64", hex.EncodeToString(sum[:])))
	base := "/dest1256/d256-node/releases/download/v1.3.0/d256-node_linux_amd64"
	r.assets[base] = old
	r.assets[base+".sig"] = []byte(base64.StdEncoding.EncodeToString(sig))
	u := newUpdater(t, r, srv, "v1.1.0")
	if err := u.Install(context.Background(), "v1.3.0"); err == nil {
		t.Fatal("a signature for another version must not validate")
	}

	sig = ed25519.Sign(r.priv, SignedMessage("v1.3.0", "linux", "arm64", hex.EncodeToString(sum[:])))
	r.assets[base+".sig"] = []byte(base64.StdEncoding.EncodeToString(sig))
	if err := u.Install(context.Background(), "v1.3.0"); err == nil {
		t.Fatal("a signature for another platform must not validate")
	}
}

func TestInstallRefusesBadInputBeforeAnyNetworkAccess(t *testing.T) {
	r, srv := newRelease(t)
	u := newUpdater(t, r, srv, "v1.1.0")
	for _, v := range []string{"", "latest", "v1", "1.2.3", "v1.2.3/../../x", "v1.2.3?x=1", "v1.2.3 ", "../v1.2.3", "v01.2.3", strings.Repeat("v1.1.1-a", 20)} {
		if err := u.Install(context.Background(), v); err == nil {
			t.Errorf("version %q was accepted", v)
		}
	}
	if err := u.Install(context.Background(), "v1.1.0"); err == nil {
		t.Error("installing the running version must be refused")
	}
	if len(r.hits) != 0 {
		t.Fatalf("bad input reached the network: %v", r.hits)
	}
}

func TestInstallIsDisabledWithoutASigningKey(t *testing.T) {
	r, srv := newRelease(t)
	u := newUpdater(t, r, srv, "v1.1.0")
	u.PubKey = nil
	if u.Enabled() {
		t.Fatal("no key means disabled")
	}
	if err := u.Install(context.Background(), "v1.2.0"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("got %v", err)
	}
	if PublicKey() != nil && len(PublicKey()) != ed25519.PublicKeySize {
		t.Fatal("malformed embedded key must read as no key")
	}
}

func TestInstallChecksTheBinaryRunsAndReportsTheVersion(t *testing.T) {
	r, srv := newRelease(t)
	r.publish("v1.2.0", script("v9.9.9"), "linux", "amd64", r.priv) // signed, but it is not v1.2.0
	r.publish("v1.2.1", []byte("not an executable"), "linux", "amd64", r.priv)
	u := newUpdater(t, r, srv, "v1.1.0")
	if err := u.Install(context.Background(), "v1.2.0"); err == nil || !strings.Contains(err.Error(), "expected v1.2.0") {
		t.Fatalf("a binary reporting another version must be refused, got %v", err)
	}
	if err := u.Install(context.Background(), "v1.2.1"); err == nil {
		t.Fatal("a binary that cannot run must be refused")
	}
}

func TestInstallLimitsDownloadAndHandlesErrors(t *testing.T) {
	r, srv := newRelease(t)
	u := newUpdater(t, r, srv, "v1.1.0")
	if err := u.Install(context.Background(), "v2.0.0"); err == nil {
		t.Fatal("a missing release must fail")
	}
	r.assets["/dest1256/d256-node/releases/download/v2.0.1/d256-node_linux_amd64"] = nil
	r.assets["/dest1256/d256-node/releases/download/v2.0.1/d256-node_linux_amd64.sig"] = []byte("x")
	if err := u.Install(context.Background(), "v2.0.1"); err == nil {
		t.Fatal("an empty download must fail")
	}
	r.publish("v2.0.2", script("v2.0.2"), "linux", "amd64", r.priv)
	r.assets["/dest1256/d256-node/releases/download/v2.0.2/d256-node_linux_amd64.sig"] = []byte("!!!not base64!!!")
	if err := u.Install(context.Background(), "v2.0.2"); err == nil {
		t.Fatal("a malformed signature must fail")
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.3", "v1.2.4", -1},
		{"v1.10.0", "v1.9.0", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.2.3-rc.1", "v1.2.3", -1},
		{"v1.2.3", "v1.2.3-rc.1", 1},
		{"dev", "v0.0.1", -1},
		{"v0.0.1", "dev", 1},
		{"dev", "dev", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// handoverEnv installs a release and returns the updater plus a record of what would be exec'd.
func handoverEnv(t *testing.T, current string) (*Updater, *[]string) {
	t.Helper()
	r, srv := newRelease(t)
	r.publish("v1.2.0", script("v1.2.0"), "linux", "amd64", r.priv)
	u := newUpdater(t, r, srv, current)
	if err := u.Install(context.Background(), "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	var execd []string
	old := execFn
	execFn = func(path string, argv, env []string) error {
		execd = append([]string{path}, append(argv, env...)...)
		return nil
	}
	t.Cleanup(func() { execFn = old })
	return u, &execd
}

func TestMaybeExecHandsOverToTheInstalledUpdate(t *testing.T) {
	u, execd := handoverEnv(t, "dev")
	if err := u.MaybeExec([]string{"run", "--x"}, []string{"A=1"}); err != nil {
		t.Fatal(err)
	}
	if len(*execd) == 0 || !strings.HasSuffix((*execd)[0], filepath.Join("v1.2.0", "d256-node")) {
		t.Fatalf("exec = %v", *execd)
	}
	joined := strings.Join(*execd, " ")
	if !strings.Contains(joined, "run --x") || !strings.Contains(joined, "D256_REEXEC=1") {
		t.Fatalf("arguments or marker missing: %v", *execd)
	}
	if m, _ := readManifest(u.binDir()); m.Attempts != 1 {
		t.Fatalf("a hand-over must count as an attempt: %+v", m)
	}
}

func TestMaybeExecDoesNotLoopAndConfirmEndsProbation(t *testing.T) {
	u, execd := handoverEnv(t, "dev")
	if err := u.MaybeExec(nil, []string{"D256_REEXEC=1"}); err != nil || len(*execd) != 0 {
		t.Fatalf("the new binary must not hand over again: %v %v", err, *execd)
	}
	// The new binary reaches the panel.
	nu := *u
	nu.Current = "v1.2.0"
	nu.Confirm()
	if m, _ := readManifest(u.binDir()); !m.Confirmed || m.Attempts != 0 {
		t.Fatalf("not confirmed: %+v", m)
	}
	// A confirmed update is not counted against on later starts.
	for i := 0; i < 5; i++ {
		*execd = nil
		if err := u.MaybeExec(nil, nil); err != nil || len(*execd) == 0 {
			t.Fatalf("start %d: %v %v", i, err, *execd)
		}
	}
	if m, _ := readManifest(u.binDir()); m.Attempts != 0 {
		t.Fatalf("attempts grew for a confirmed update: %+v", m)
	}
}

func TestAnUpdateThatNeverReachesThePanelIsRolledBack(t *testing.T) {
	u, execd := handoverEnv(t, "dev")
	for i := 0; i < maxAttempts; i++ {
		if err := u.MaybeExec(nil, nil); err != nil || len(*execd) == 0 {
			t.Fatalf("attempt %d: %v %v", i, err, *execd)
		}
		*execd = nil
	}
	// The fourth start finds a binary that never confirmed: back to the image's binary.
	if err := u.MaybeExec(nil, nil); err != nil || len(*execd) != 0 {
		t.Fatalf("rollback expected: %v %v", err, *execd)
	}
	if u.Installed() != "" {
		t.Fatal("the failed update must be forgotten")
	}
	if _, err := os.Stat(filepath.Join(u.binDir(), "v1.2.0")); err == nil {
		t.Fatal("the failed binary must be removed")
	}
}

func TestMaybeExecIgnoresObsoleteOrTamperedUpdates(t *testing.T) {
	// The image was rebuilt with a release at least as new as the installed one.
	u, execd := handoverEnv(t, "v1.3.0")
	if err := u.MaybeExec(nil, nil); err != nil || len(*execd) != 0 || u.Installed() != "" {
		t.Fatalf("obsolete update must be dropped: %v %v %q", err, *execd, u.Installed())
	}

	// The installed file no longer matches the recorded hash.
	u, execd = handoverEnv(t, "dev")
	m, _ := readManifest(u.binDir())
	if err := os.WriteFile(m.Path, []byte("#!/bin/sh\necho evil\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := u.MaybeExec(nil, nil); err != nil || len(*execd) != 0 || u.Installed() != "" {
		t.Fatalf("a modified binary must not be run: %v %v", err, *execd)
	}
}

func TestMaybeExecRefusesAManifestPointingElsewhere(t *testing.T) {
	u, execd := handoverEnv(t, "dev")
	m, _ := readManifest(u.binDir())
	outside := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, _ := fileSHA256(outside)
	m.Path, m.SHA256 = outside, sum
	if err := writeManifest(u.binDir(), m); err != nil {
		t.Fatal(err)
	}
	if err := u.MaybeExec(nil, nil); err != nil || len(*execd) != 0 {
		t.Fatalf("a path outside bin/ must not be executed: %v %v", err, *execd)
	}
}

func TestOnlyTheCurrentAndPreviousReleaseAreKept(t *testing.T) {
	r, srv := newRelease(t)
	u := newUpdater(t, r, srv, "dev")
	for _, v := range []string{"v1.0.0", "v1.1.0", "v1.2.0"} {
		r.publish(v, script(v), "linux", "amd64", r.priv)
		if err := u.Install(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(u.binDir())
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 2 || dirs[0] != "v1.1.0" || dirs[1] != "v1.2.0" {
		t.Fatalf("kept %v", dirs)
	}
}

func TestSourceRepoComesFromTheBuildAndIsValid(t *testing.T) {
	if got := SourceRepo(); !ValidRepo(got) || got != "dest1256/d256-node" {
		t.Fatalf("SourceRepo() = %q", got)
	}
	old := repoFile
	defer func() { repoFile = old }()
	for in, want := range map[string]string{
		"someone/their-fork\n": "someone/their-fork",
		"":                     DefaultRepo,
		"../../evil":           DefaultRepo,
		"a/b/c":                DefaultRepo,
		"has space/x":          DefaultRepo,
	} {
		repoFile = in
		if got := SourceRepo(); got != want {
			t.Errorf("repo.txt %q -> %q, want %q", in, got, want)
		}
	}
}
