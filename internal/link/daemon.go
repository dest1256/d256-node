// Package link maintains the node's outbound gRPC (mTLS) session with the panel and everything the node
// needs to keep working when that session is down: the sealed state cache, the traffic journal and the
// local enforcement of client limits.
package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"

	"github.com/dest1256/d256-node/internal/cache"
	"github.com/dest1256/d256-node/internal/journal"
	"github.com/dest1256/d256-node/internal/limits"
	"github.com/dest1256/d256-node/internal/state"
	"github.com/dest1256/d256-node/internal/sysmetrics"
	"github.com/dest1256/d256-node/internal/vault"
	"github.com/dest1256/d256-node/internal/version"
	"github.com/dest1256/d256-node/internal/xrayrun"
)

// tick is how often usage is collected and the link is serviced; tests shorten it.
var tick = 5 * time.Second

const (
	echoDeadline    = 15 * time.Second
	minBackoff      = time.Second
	maxBackoff      = time.Minute
	stableAfter     = 30 * time.Second
	rejectedWait    = time.Minute
	helloAckTimeout = 15 * time.Second
)

// Options tune the daemon; the zero value is the production setting.
type Options struct {
	// JournalMaxBytes caps the disk journal (default journal.DefaultMaxBytes).
	JournalMaxBytes int64
	// Secret overrides the key material the local data is sealed with (protected mode); by default it
	// is derived from the node's private key.
	Secret []byte
}

// Daemon owns the reconnecting session and the node's offline state.
type Daemon struct {
	Dir     state.Dir
	Log     *slog.Logger
	Metrics *sysmetrics.Collector
	Xray    *xrayrun.Runner

	id     *state.Identity
	sealer *vault.Sealer
	jr     *journal.Journal
	enf    *limits.Enforcer

	// collectUsage reads the usage measured since the last call; tests replace it.
	collectUsage func() []*pb.TrafficEntry

	reconnect chan struct{}

	tickMu   sync.Mutex // serialises usage collection
	lastTick int64

	mu           sync.Mutex // everything below
	st           state.Status
	pending      []journal.Batch // unacknowledged usage while the link is up, in sequence order
	sentTo       uint64
	online       bool
	limits       *pb.LimitsSnapshot
	cacheVersion uint64
	cacheConfig  []byte
}

// New opens the node's local data and, if a state cache exists, starts Xray from it right away so the
// node serves clients before (and without) the panel.
func New(dir state.Dir, log *slog.Logger, opts Options) (*Daemon, error) {
	id, err := dir.Load()
	if err != nil {
		return nil, err
	}
	secret := opts.Secret
	if len(secret) == 0 {
		secret = id.KeyPEM
	}
	sealer, err := vault.FromSecret(secret, "node-local-data")
	if err != nil {
		return nil, err
	}
	jr, err := journal.Open(dir.JournalPath(), sealer, opts.JournalMaxBytes)
	if err != nil {
		return nil, err
	}
	d := &Daemon{
		Dir: dir, Log: log, Metrics: sysmetrics.New(), Xray: xrayrun.New(log),
		id: id, sealer: sealer, jr: jr, enf: limits.New(),
		reconnect: make(chan struct{}, 1),
		lastTick:  time.Now().Unix(),
		st:        state.Status{XrayState: xrayrun.StateNotConfigured},
	}
	d.collectUsage = d.Xray.Collect
	d.restoreFromCache()
	return d, nil
}

// restoreFromCache applies the last known state and rebuilds the limit bookkeeping from it plus the
// usage the journal still holds (measured offline, not yet acknowledged).
func (d *Daemon) restoreFromCache() {
	st, err := cache.Load(d.Dir.CachePath(), d.sealer)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return
	case err != nil:
		d.Log.Warn("ignoring an unreadable state cache", "err", err)
		return
	}
	if err := d.Xray.Apply(&pb.DesiredState{Version: st.Version, XrayConfigJson: st.ConfigJSON}); err != nil {
		d.Log.Error("the cached state could not be applied", "err", err)
		return
	}
	d.cacheVersion, d.cacheConfig, d.limits = st.Version, st.ConfigJSON, st.Limits
	d.Log.Info("started from the cached state", "version", st.Version, "age", time.Since(time.Unix(st.SavedUnix, 0)).Round(time.Second))
	if st.Limits != nil {
		d.enf.SetSnapshot(st.Limits)
		if stored, err := d.jr.Batches(); err == nil {
			for _, b := range stored {
				d.enf.AddUsage(b.Entries)
			}
		}
	}
}

// Close writes the in-memory queue to disk, closes the journal and stops Xray.
func (d *Daemon) Close() {
	d.goOffline()
	if err := d.jr.Close(); err != nil {
		d.Log.Warn("closing the journal", "err", err)
	}
	d.Xray.Stop()
}

// Reconnect drops the current session; the loop dials again immediately.
func (d *Daemon) Reconnect() {
	select {
	case d.reconnect <- struct{}{}:
	default:
	}
}

func (d *Daemon) setStatus(fn func(*state.Status)) {
	d.mu.Lock()
	fn(&d.st)
	s := d.st
	d.mu.Unlock()
	if err := d.Dir.WriteStatus(s); err != nil {
		d.Log.Debug("write status", "err", err)
	}
}

// Run keeps the link up until ctx is cancelled, backing off between failures. While the link is down
// it still measures usage, journals it and enforces limits.
func (d *Daemon) Run(ctx context.Context) error {
	tlsCfg, err := clientTLS(d.id)
	if err != nil {
		return err
	}
	go d.offlineLoop(ctx)
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := d.session(ctx, d.id, tlsCfg)
		d.goOffline()
		d.setStatus(func(s *state.Status) {
			s.Connected = false
			if err != nil {
				s.LastError = err.Error()
			}
		})
		if ctx.Err() != nil {
			return nil
		}
		wait := backoff
		if isRejected(err) {
			d.Log.Error("panel rejected this node's certificate; it was deleted or revoked - re-enroll with a new token", "err", err)
			wait = rejectedWait
		} else {
			d.Log.Warn("link down", "err", err, "retry_in", backoff.Round(time.Millisecond))
		}
		if time.Since(started) > stableAfter {
			backoff = minBackoff
		} else if backoff < maxBackoff {
			backoff = min(backoff*2, maxBackoff)
		}
		select {
		case <-time.After(wait + time.Duration(rand.Int64N(int64(wait/4)+1))):
		case <-d.reconnect:
		case <-ctx.Done():
		}
	}
	return nil
}

// offlineLoop does the periodic work for as long as the link is down; while it is up the session loop
// drives the same tick together with its sends.
func (d *Daemon) offlineLoop(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.mu.Lock()
			online := d.online
			d.mu.Unlock()
			if !online {
				_ = d.tick(nil)
				xs, _, xver := d.Xray.Status()
				jb := d.journalBytes()
				d.setStatus(func(s *state.Status) { s.XrayState, s.ConfigVersion, s.JournalBytes = xs, xver, jb })
			}
		}
	}
}

func isRejected(err error) bool {
	c := status.Code(err)
	return c == codes.PermissionDenied || c == codes.Unauthenticated
}

func clientTLS(id *state.Identity) (*tls.Config, error) {
	pair, err := tls.X509KeyPair(id.CertPEM, id.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(id.CAPEM) {
		return nil, errors.New("panel CA certificate is invalid")
	}
	host, _, err := net.SplitHostPort(id.Info.GRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("bad panel address %q: %w", id.Info.GRPCAddr, err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: pool, ServerName: host}, nil
}

func (d *Daemon) session(parent context.Context, id *state.Identity, tlsCfg *tls.Config) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	conn, err := grpc.NewClient(id.Info.GRPCAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	stream, err := pb.NewNodeLinkClient(conn).Connect(ctx)
	if err != nil {
		return err
	}
	var sendMu sync.Mutex
	send := func(m *pb.NodeToPanel) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}

	if err := send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_Hello{Hello: &pb.Hello{
		ProtocolVersion:     uint32(pb.ProtocolVersion_PROTOCOL_VERSION_1),
		NodeVersion:         version.Version,
		XrayVersion:         xrayrun.Version(),
		Os:                  runtime.GOOS,
		Arch:                runtime.GOARCH,
		JournalEpoch:        d.jr.Epoch(),
		AppliedStateVersion: d.appliedVersion(),
	}}}); err != nil {
		return err
	}

	var rtt atomic.Uint32
	var lastEcho atomic.Int64
	lastEcho.Store(time.Now().UnixNano())
	desiredCh := make(chan *pb.DesiredState, 1)
	ackCh := make(chan *pb.HelloAck, 1)
	errCh := make(chan error, 1)
	go func() {
		first := true
		for {
			m, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
			if first {
				first = false
				ackCh <- m.GetHelloAck()
				continue
			}
			switch {
			case m.GetHeartbeat() != nil:
				hb := m.GetHeartbeat()
				ms := uint32(max(time.Since(time.Unix(0, hb.UnixNanos)).Milliseconds(), 0))
				rtt.Store(ms)
				lastEcho.Store(time.Now().UnixNano())
				d.setStatus(func(s *state.Status) { s.LatencyMs = ms })
			case m.GetDesiredState() != nil:
				enqueue(desiredCh, m.GetDesiredState())
			case m.GetJournalAck() != nil:
				d.onJournalAck(m.GetJournalAck().AckedSeq)
			case m.GetLimits() != nil:
				d.onLimits(m.GetLimits())
			}
		}
	}()
	select {
	case ack := <-ackCh:
		if ack == nil || !ack.Accepted {
			reason := "no acknowledgement"
			if ack != nil {
				reason = ack.Reason
			}
			return fmt.Errorf("panel refused this node: %s", reason)
		}
	case err := <-errCh:
		return err
	case <-time.After(helloAckTimeout):
		return errors.New("timed out waiting for the panel's hello acknowledgement")
	case <-d.reconnect:
		return errors.New("reconnect requested")
	case <-ctx.Done():
		return nil
	}
	d.Log.Info("connected to panel", "addr", id.Info.GRPCAddr, "node_id", id.Info.NodeID)
	d.goOnline()
	d.setStatus(func(s *state.Status) { s.Connected, s.LastConnected, s.LastError = true, time.Now().Unix(), "" })
	go d.applyLoop(ctx, desiredCh, send)

	sendRound := func() error {
		hb := &pb.Heartbeat{UnixNanos: time.Now().UnixNano(), LastRttMs: rtt.Load()}
		if err := send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_Heartbeat{Heartbeat: hb}}); err != nil {
			return err
		}
		d.Metrics.OnlineClients = uint32(d.Xray.OnlineUsers())
		d.Metrics.JournalBytes = d.journalBytes()
		if err := send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_SystemMetrics{SystemMetrics: d.Metrics.Sample()}}); err != nil {
			return err
		}
		xs, xerr, xver := d.Xray.Status()
		jb := d.Metrics.JournalBytes
		d.setStatus(func(s *state.Status) { s.XrayState, s.ConfigVersion, s.JournalBytes = xs, xver, jb })
		if err := send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_XrayStatus{XrayStatus: &pb.XrayStatus{
			State: xs, Error: xerr, Version: xrayrun.Version(), AppliedStateVersion: xver,
			OnlineUsers: d.Xray.OnlineEmails(5000),
		}}}); err != nil {
			return err
		}
		return d.tick(send)
	}
	if err := sendRound(); err != nil {
		return err
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if time.Since(time.Unix(0, lastEcho.Load())) > echoDeadline {
				return errors.New("panel stopped answering heartbeats")
			}
			if err := sendRound(); err != nil {
				return err
			}
		case err := <-errCh:
			return err
		case <-d.reconnect:
			return errors.New("reconnect requested")
		case <-ctx.Done():
			return nil
		}
	}
}

func (d *Daemon) appliedVersion() uint64 {
	_, _, v := d.Xray.Status()
	return v
}

// onLimits adopts a fresh limits snapshot from the panel and remembers it for offline starts.
func (d *Daemon) onLimits(s *pb.LimitsSnapshot) {
	d.enf.SetSnapshot(s)
	d.mu.Lock()
	d.limits = s
	d.mu.Unlock()
	d.saveCache(0, nil)
}
