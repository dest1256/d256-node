// Package connstats records, for every client connection, who connected from where to which destination
// and how many bytes moved. The core only counts bytes per user; this adds the destination by wrapping
// its dispatcher (see tracker.go), without patching xray-core.
package connstats

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

const (
	maxDestLen = 253
	// ipRetention is how long a source IP stays on a client's device list after its last connection.
	ipRetention = 30 * time.Minute
	// MaxActive caps the live snapshot sent to the panel.
	MaxActive = 5000
	// maxIPsPerClient bounds the device list of one client.
	maxIPsPerClient = 50
)

type aggKey struct {
	email, inbound, dest, network string
	hour                          int64
}

type aggVal struct {
	up, down uint64
	conns    uint32
}

// conn is one live connection. The byte counters are written from the data path, everything else is
// fixed at creation (dest is read through a callback because sniffing resolves it later).
type conn struct {
	id       uint64
	email    string
	inbound  string
	srcIP    string
	network  string
	start    time.Time
	destFn   func() (dest string, port uint32)
	up, down atomic.Uint64

	drainedUp, drainedDown uint64 // guarded by Store.mu
	counted                bool   // guarded by Store.mu
}

// Store aggregates connection usage and tracks who is connected right now. It outlives core restarts.
type Store struct {
	mu     sync.Mutex
	nextID uint64
	active map[uint64]*conn
	agg    map[aggKey]*aggVal
	ips    map[string]map[string]time.Time // email -> ip -> last seen
	limits map[string]uint32               // email -> max distinct IPs
	now    func() time.Time
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		active: map[uint64]*conn{}, agg: map[aggKey]*aggVal{},
		ips: map[string]map[string]time.Time{}, limits: map[string]uint32{}, now: time.Now,
	}
}

// SetDeviceLimits replaces the per-client device limits (0 or absent = unlimited).
func (s *Store) SetDeviceLimits(limits map[string]uint32) {
	s.mu.Lock()
	s.limits = limits
	s.mu.Unlock()
}

// admit reports whether a new connection from ip may start: a client at its device limit may only
// connect from addresses that are already connected.
func (s *Store) admit(email, ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := s.limits[email]
	if limit == 0 {
		return true
	}
	distinct := map[string]struct{}{}
	for _, c := range s.active {
		if c.email == email {
			distinct[c.srcIP] = struct{}{}
		}
	}
	if _, known := distinct[ip]; known {
		return true
	}
	return uint32(len(distinct)) < limit
}

func (s *Store) open(email, inbound, srcIP, network string, destFn func() (string, uint32)) *conn {
	c := &conn{email: email, inbound: inbound, srcIP: srcIP, network: network, start: s.now(), destFn: destFn}
	s.mu.Lock()
	s.nextID++
	c.id = s.nextID
	s.active[c.id] = c
	if srcIP != "" {
		m := s.ips[email]
		if m == nil {
			m = map[string]time.Time{}
			s.ips[email] = m
		}
		m[srcIP] = c.start
	}
	s.mu.Unlock()
	return c
}

func truncate(v string) string {
	if len(v) > maxDestLen {
		return v[:maxDestLen]
	}
	return v
}

// addLocked accumulates a delta into the hourly aggregate. The caller holds s.mu.
func (s *Store) addLocked(c *conn, dest string, up, down uint64, newConn bool) {
	if up == 0 && down == 0 && !newConn {
		return
	}
	k := aggKey{c.email, c.inbound, truncate(dest), c.network, s.now().Truncate(time.Hour).Unix()}
	v := s.agg[k]
	if v == nil {
		v = &aggVal{}
		s.agg[k] = v
	}
	v.up += up
	v.down += down
	if newConn {
		v.conns++
	}
}

func (s *Store) drainConnLocked(c *conn) {
	dest, _ := c.destFn()
	up, down := c.up.Load(), c.down.Load()
	s.addLocked(c, dest, up-c.drainedUp, down-c.drainedDown, !c.counted)
	c.drainedUp, c.drainedDown, c.counted = up, down, true
}

// close finishes a connection and books its remaining usage.
func (s *Store) close(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.active[c.id]; !ok {
		return
	}
	s.drainConnLocked(c)
	delete(s.active, c.id)
	if m := s.ips[c.email]; m != nil && c.srcIP != "" {
		m[c.srcIP] = s.now()
	}
}

// Drain moves everything measured so far (closed connections and the progress of long ones) out of
// the store as per-hour entries.
func (s *Store) Drain() []*pb.ConnEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.active {
		s.drainConnLocked(c)
	}
	out := make([]*pb.ConnEntry, 0, len(s.agg))
	for k, v := range s.agg {
		out = append(out, &pb.ConnEntry{
			Email: k.email, InboundTag: k.inbound, Dest: k.dest, Network: k.network,
			HourUnix: k.hour, Up: v.up, Down: v.down, Conns: v.conns,
		})
	}
	s.agg = map[aggKey]*aggVal{}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HourUnix != out[j].HourUnix {
			return out[i].HourUnix < out[j].HourUnix
		}
		if out[i].Email != out[j].Email {
			return out[i].Email < out[j].Email
		}
		return out[i].Dest < out[j].Dest
	})
	return out
}

// Snapshot returns the live connections (capped) and the device list.
func (s *Store) Snapshot() *pb.ActiveConnections {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &pb.ActiveConnections{}
	conns := make([]*conn, 0, len(s.active))
	for _, c := range s.active {
		conns = append(conns, c)
	}
	sort.Slice(conns, func(i, j int) bool { return conns[i].start.After(conns[j].start) })
	if len(conns) > MaxActive {
		conns, res.Truncated = conns[:MaxActive], true
	}
	for _, c := range conns {
		dest, port := c.destFn()
		res.Conns = append(res.Conns, &pb.ActiveConn{
			Email: c.email, InboundTag: c.inbound, SourceIp: c.srcIP, Dest: truncate(dest), Port: port,
			Network: c.network, StartUnix: c.start.Unix(), Up: c.up.Load(), Down: c.down.Load(),
		})
	}
	cutoff := s.now().Add(-ipRetention)
	for email, m := range s.ips {
		type seen struct {
			ip string
			at time.Time
		}
		var list []seen
		for ip, at := range m {
			if at.Before(cutoff) {
				delete(m, ip)
				continue
			}
			list = append(list, seen{ip, at})
		}
		if len(m) == 0 {
			delete(s.ips, email)
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].at.After(list[j].at) })
		if len(list) > maxIPsPerClient {
			list = list[:maxIPsPerClient]
		}
		for _, l := range list {
			res.Ips = append(res.Ips, &pb.ClientIp{Email: email, Ip: l.ip, LastSeenUnix: l.at.Unix()})
		}
	}
	sort.Slice(res.Ips, func(i, j int) bool {
		if res.Ips[i].Email != res.Ips[j].Email {
			return res.Ips[i].Email < res.Ips[j].Email
		}
		return res.Ips[i].LastSeenUnix > res.Ips[j].LastSeenUnix
	})
	return res
}

// ActiveCount is the number of live connections.
func (s *Store) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// cleanDest normalises a destination for storage and display.
func cleanDest(d string) string { return strings.ToLower(strings.TrimSuffix(d, ".")) }
