// Package state is the node's on-disk identity: enrollment info, client key/certificate and the panel CA.
// Stage 7 wraps these files in authenticated encryption; paths and the API stay the same.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	infoFile   = "node.json"
	keyFile    = "client.key"
	certFile   = "client.crt"
	caFile     = "ca.crt"
	statusFile = "status.json"
	pidFile    = "d256-node.pid"
	logFile    = "node.log"
)

// ErrNotEnrolled is returned when the node has no stored identity.
var ErrNotEnrolled = errors.New("node is not enrolled; run `d256-node enroll <panel-url> <token>`")

// Info is the non-secret enrollment record.
type Info struct {
	NodeID     int    `json:"nodeId"`
	PanelURL   string `json:"panelUrl"`
	GRPCAddr   string `json:"grpcAddr"`
	EnrolledAt int64  `json:"enrolledAt"`
}

// Identity is everything needed to dial the panel.
type Identity struct {
	Info    Info
	KeyPEM  []byte
	CertPEM []byte
	CAPEM   []byte
}

// Dir is a state directory.
type Dir string

// DefaultDir is $D256_STATE_DIR or /var/lib/d256-node.
func DefaultDir() Dir {
	if v := os.Getenv("D256_STATE_DIR"); v != "" {
		return Dir(v)
	}
	return Dir("/var/lib/d256-node")
}

func (d Dir) path(name string) string { return filepath.Join(string(d), name) }

// LogPath, PIDPath and StatusPath locate the runtime files.
func (d Dir) LogPath() string    { return d.path(logFile) }
func (d Dir) PIDPath() string    { return d.path(pidFile) }
func (d Dir) StatusPath() string { return d.path(statusFile) }

// Enrolled reports whether an identity is stored.
func (d Dir) Enrolled() bool {
	_, err := os.Stat(d.path(infoFile))
	return err == nil
}

// Save stores the identity atomically. The private key is created with mode 0600.
func (d Dir) Save(id *Identity) error {
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return err
	}
	info, err := json.MarshalIndent(id.Info, "", "  ")
	if err != nil {
		return err
	}
	// Key first, node.json last: node.json marks the enrollment as complete.
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{keyFile, id.KeyPEM, 0o600},
		{certFile, id.CertPEM, 0o644},
		{caFile, id.CAPEM, 0o644},
		{infoFile, info, 0o644},
	} {
		if err := writeAtomic(d.path(f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

// Load reads the stored identity.
func (d Dir) Load() (*Identity, error) {
	if !d.Enrolled() {
		return nil, ErrNotEnrolled
	}
	id := &Identity{}
	raw, err := os.ReadFile(d.path(infoFile))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &id.Info); err != nil {
		return nil, fmt.Errorf("read %s: %w", infoFile, err)
	}
	for name, dst := range map[string]*[]byte{keyFile: &id.KeyPEM, certFile: &id.CertPEM, caFile: &id.CAPEM} {
		if *dst, err = os.ReadFile(d.path(name)); err != nil {
			return nil, err
		}
	}
	return id, nil
}

// Reset removes keys, certificates and runtime files.
func (d Dir) Reset() error {
	var firstErr error
	for _, n := range []string{infoFile, keyFile, certFile, caFile, statusFile, pidFile} {
		if err := os.Remove(d.path(n)); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Status is what the daemon publishes for `d256-node status`.
type Status struct {
	Connected     bool   `json:"connected"`
	LastConnected int64  `json:"lastConnected"`
	LastError     string `json:"lastError"`
	LatencyMs     uint32 `json:"latencyMs"`
	ConfigVersion uint64 `json:"configVersion"`
	XrayState     string `json:"xrayState"`
	JournalBytes  uint64 `json:"journalBytes"`
	UpdatedAt     int64  `json:"updatedAt"`
	PID           int    `json:"pid"`
}

// WriteStatus publishes the daemon status.
func (d Dir) WriteStatus(s Status) error {
	s.UpdatedAt = time.Now().Unix()
	s.PID = os.Getpid()
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(d.StatusPath(), raw, 0o644)
}

// ReadStatus returns the last published status.
func (d Dir) ReadStatus() (*Status, error) {
	raw, err := os.ReadFile(d.StatusPath())
	if err != nil {
		return nil, err
	}
	var s Status
	return &s, json.Unmarshal(raw, &s)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
