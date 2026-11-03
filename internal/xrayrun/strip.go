package xrayrun

import (
	"encoding/json"
	"fmt"
)

// WithoutUsers returns cfgJSON with the given users removed from every inbound's client list.
// It is how the node enforces limits offline: the stripped config is applied with a restart so live
// sessions end too, and the panel's own config (which still lists them) replaces it on reconnect.
func WithoutUsers(cfgJSON []byte, emails []string) ([]byte, error) {
	drop := make(map[string]struct{}, len(emails))
	for _, e := range emails {
		drop[e] = struct{}{}
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		return nil, err
	}
	var inbounds []map[string]json.RawMessage
	if err := json.Unmarshal(cfg["inbounds"], &inbounds); err != nil {
		return nil, fmt.Errorf("read inbounds: %w", err)
	}
	for _, ib := range inbounds {
		var settings map[string]json.RawMessage
		if json.Unmarshal(ib["settings"], &settings) != nil {
			continue
		}
		var clients []map[string]any
		if json.Unmarshal(settings["clients"], &clients) != nil {
			continue
		}
		kept := clients[:0]
		for _, c := range clients {
			if email, _ := c["email"].(string); email != "" {
				if _, gone := drop[email]; gone {
					continue
				}
			}
			kept = append(kept, c)
		}
		raw, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		settings["clients"] = raw
		if ib["settings"], err = json.Marshal(settings); err != nil {
			return nil, err
		}
	}
	var err error
	if cfg["inbounds"], err = json.Marshal(inbounds); err != nil {
		return nil, err
	}
	return json.Marshal(cfg)
}

// Drop restarts Xray without the given users. The runner then differs from the panel's config, so the
// next state from the panel is applied in full and restores whoever the panel still allows.
func (r *Runner) Drop(emails []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inst == nil || len(emails) == 0 {
		return nil
	}
	stripped, err := WithoutUsers(r.cfgJSON, emails)
	if err != nil {
		return err
	}
	return r.restartLocked(r.version, stripped)
}
