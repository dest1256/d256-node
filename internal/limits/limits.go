// Package limits keeps enforcing client quotas and expiry while the panel is unreachable.
//
// The panel stays authoritative: whenever it is connected it decides who may connect, and a new snapshot
// replaces the node's own bookkeeping. Offline, the node adds the usage it measures to the last
// snapshot and drops clients that are over quota or expired.
package limits

import (
	"sync"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

// Enforcer tracks per-client usage against the last snapshot.
type Enforcer struct {
	mu      sync.Mutex
	limits  map[string]*pb.ClientLimit
	used    map[string]uint64
	dropped map[string]bool
}

// New returns an enforcer with no snapshot (nothing is ever dropped).
func New() *Enforcer {
	return &Enforcer{limits: map[string]*pb.ClientLimit{}, used: map[string]uint64{}, dropped: map[string]bool{}}
}

// SetSnapshot replaces the limits and restarts the usage from the panel's own count.
func (e *Enforcer) SetSnapshot(s *pb.LimitsSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.limits = make(map[string]*pb.ClientLimit, len(s.GetClients()))
	e.used = make(map[string]uint64, len(s.GetClients()))
	for _, c := range s.GetClients() {
		e.limits[c.Email] = c
		e.used[c.Email] = c.UsedBytes
	}
}

// AddUsage adds locally measured usage; only user entries count.
func (e *Enforcer) AddUsage(entries []*pb.TrafficEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, en := range entries {
		if !en.Inbound {
			e.used[en.Name] += en.Up + en.Down
		}
	}
}

// Violations returns the clients that just crossed their quota or expiry and were not dropped yet.
func (e *Enforcer) Violations(nowUnix int64) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for email, l := range e.limits {
		if e.dropped[email] {
			continue
		}
		over := l.QuotaBytes > 0 && e.used[email] >= l.QuotaBytes
		expired := l.ExpiryUnix > 0 && nowUnix >= l.ExpiryUnix
		if over || expired {
			e.dropped[email] = true
			out = append(out, email)
		}
	}
	return out
}

// Reset forgets which clients were dropped (a fresh panel state was applied).
func (e *Enforcer) Reset() {
	e.mu.Lock()
	e.dropped = map[string]bool{}
	e.mu.Unlock()
}
