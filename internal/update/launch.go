package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// maxAttempts is how many starts a new binary gets to prove itself (reach the panel) before the node
	// falls back to the binary in the image.
	maxAttempts = 3
	// reexecEnv marks a process that was started by a hand-over, so it does not hand over again.
	reexecEnv = "D256_REEXEC"
)

// Manifest records the installed update. It is written atomically next to the binaries.
type Manifest struct {
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Path      string `json:"path"`
	Previous  string `json:"previous,omitempty"`
	Attempts  int    `json:"attempts"`
	Confirmed bool   `json:"confirmed"`
}

func manifestPath(binDir string) string { return filepath.Join(binDir, "current.json") }

func readManifest(binDir string) (*Manifest, error) {
	raw, err := os.ReadFile(manifestPath(binDir))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func writeManifest(binDir string, m *Manifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := manifestPath(binDir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, manifestPath(binDir))
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// execFn replaces the process image; tests swap it.
var execFn = syscall.Exec

// MaybeExec is called at the very start of `run`. If an update is installed and valid, the process is
// replaced by it and this function does not return. In every other case it returns nil and the
// embedded binary carries on: no update, the update is not newer than the image, the update failed to
// prove itself, or the installed file no longer matches its recorded hash.
func (u *Updater) MaybeExec(args, env []string) error {
	for _, e := range env {
		if e == reexecEnv+"=1" {
			return nil
		}
	}
	m, err := readManifest(u.binDir())
	if err != nil {
		return nil
	}
	switch {
	case !ValidVersion(m.Version) || (ValidVersion(u.Current) && Compare(u.Current, m.Version) >= 0):
		// The image was rebuilt with a release at least as new: the installed copy is obsolete.
		u.forget()
		return nil
	case !m.Confirmed && m.Attempts >= maxAttempts:
		u.forget()
		return nil
	}
	if sum, err := fileSHA256(m.Path); err != nil || sum != m.SHA256 || filepath.Dir(filepath.Dir(m.Path)) != u.binDir() {
		u.forget()
		return nil
	}
	return u.handOver(m, args, env)
}

// ExecCurrent hands over to the installed update right away (used after a successful Install).
func (u *Updater) ExecCurrent(args, env []string) error {
	m, err := readManifest(u.binDir())
	if err != nil {
		return err
	}
	return u.handOver(m, args, env)
}

func (u *Updater) handOver(m *Manifest, args, env []string) error {
	if !m.Confirmed {
		m.Attempts++
		if err := writeManifest(u.binDir(), m); err != nil {
			return err
		}
	}
	env = append(append([]string(nil), env...), reexecEnv+"=1")
	return execFn(m.Path, append([]string{m.Path}, args...), env)
}

// Confirm is called once the running binary has reached the panel. A binary that was installed by an
// update stops being on probation.
func (u *Updater) Confirm() {
	m, err := readManifest(u.binDir())
	if err != nil || m.Version != u.Current || m.Confirmed {
		return
	}
	m.Confirmed, m.Attempts = true, 0
	_ = writeManifest(u.binDir(), m)
}

// forget drops the installed update so the node runs the binary of the image again.
func (u *Updater) forget() {
	m, err := readManifest(u.binDir())
	_ = os.Remove(manifestPath(u.binDir()))
	if err == nil && ValidVersion(m.Version) {
		_ = os.RemoveAll(filepath.Join(u.binDir(), m.Version))
	}
}

// Installed returns the version recorded as installed ("" when none).
func (u *Updater) Installed() string {
	m, err := readManifest(u.binDir())
	if err != nil {
		return ""
	}
	return m.Version
}
