package cache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"

	"github.com/dest1256/d256-node/internal/vault"
)

func TestSaveLoadRoundTripAndEncryption(t *testing.T) {
	s, _ := vault.FromSecret([]byte("k"), "cache-test")
	path := filepath.Join(t.TempDir(), "cache.bin")
	if _, err := Load(path, s); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing cache: %v", err)
	}
	in := State{
		Version: 9, ConfigJSON: []byte(`{"inbounds":[],"secret":"reality-private-key"}`),
		Limits: &pb.LimitsSnapshot{TakenUnix: 5, Clients: []*pb.ClientLimit{{Email: "a@x", QuotaBytes: 10, UsedBytes: 3, ExpiryUnix: 99}}},
	}
	if err := Save(path, s, in); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("reality-private-key")) {
		t.Fatal("config visible in plaintext")
	}
	out, err := Load(path, s)
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != 9 || !bytes.Equal(out.ConfigJSON, in.ConfigJSON) || len(out.Limits.Clients) != 1 || out.Limits.Clients[0].UsedBytes != 3 {
		t.Fatalf("loaded %+v", out)
	}
	other, _ := vault.FromSecret([]byte("other"), "cache-test")
	if _, err := Load(path, other); !errors.Is(err, vault.ErrOpen) {
		t.Fatalf("wrong key: %v", err)
	}
	raw[len(raw)-1] ^= 1
	_ = os.WriteFile(path, raw, 0o600)
	if _, err := Load(path, s); !errors.Is(err, vault.ErrOpen) {
		t.Fatalf("tampered cache: %v", err)
	}
}
