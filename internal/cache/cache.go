// Package cache stores the last desired state the panel sent, sealed on disk, so the node can start
// Xray and keep enforcing limits when the panel cannot be reached.
package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"

	"github.com/dest1256/d256-node/internal/vault"
)

const purpose = "cache/state"

// State is everything the node needs to run without the panel.
type State struct {
	Version    uint64
	ConfigJSON []byte
	Limits     *pb.LimitsSnapshot
	SavedUnix  int64
}

type wire struct {
	Version   uint64 `json:"version"`
	Config    []byte `json:"config"`
	Limits    []byte `json:"limits,omitempty"`
	SavedUnix int64  `json:"savedUnix"`
}

// Save replaces the cache atomically.
func Save(path string, s *vault.Sealer, st State) error {
	w := wire{Version: st.Version, Config: st.ConfigJSON, SavedUnix: time.Now().Unix()}
	if st.Limits != nil {
		raw, err := proto.Marshal(st.Limits)
		if err != nil {
			return err
		}
		w.Limits = raw
	}
	plain, err := json.Marshal(w)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cache-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(s.Seal(plain, purpose)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Load returns os.ErrNotExist when there is no cache and vault.ErrOpen when it cannot be read.
func Load(path string, s *vault.Sealer) (*State, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	plain, err := s.Open(blob, purpose)
	if err != nil {
		return nil, err
	}
	var w wire
	if err := json.Unmarshal(plain, &w); err != nil {
		return nil, vault.ErrOpen
	}
	st := &State{Version: w.Version, ConfigJSON: w.Config, SavedUnix: w.SavedUnix}
	if len(w.Limits) > 0 {
		var l pb.LimitsSnapshot
		if err := proto.Unmarshal(w.Limits, &l); err == nil {
			st.Limits = &l
		}
	}
	return st, nil
}
