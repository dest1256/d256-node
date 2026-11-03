// Package link maintains the node's outbound gRPC (mTLS) session with the panel.
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

	"github.com/dest1256/d256-node/internal/state"
	"github.com/dest1256/d256-node/internal/sysmetrics"
	"github.com/dest1256/d256-node/internal/version"
	"github.com/dest1256/d256-node/internal/xrayrun"
)

const (
	tick            = 5 * time.Second
	echoDeadline    = 15 * time.Second
	minBackoff      = time.Second
	maxBackoff      = time.Minute
	stableAfter     = 30 * time.Second
	rejectedWait    = time.Minute
	helloAckTimeout = 15 * time.Second
)

// Daemon owns the reconnecting session.
type Daemon struct {
	Dir     state.Dir
	Log     *slog.Logger
	Metrics *sysmetrics.Collector
	Xray    *xrayrun.Runner

	// Traffic journal (in memory until the disk journal of stage 5): batches not yet acknowledged by
	// the panel, in sequence order, plus the next sequence to assign.
	epoch   uint64
	pending []trafficBatch
	nextSeq uint64
	sentTo  uint64

	reconnect chan struct{}
	mu        sync.Mutex
	st        state.Status
}

// New builds a daemon.
func New(dir state.Dir, log *slog.Logger) *Daemon {
	return &Daemon{
		Dir: dir, Log: log, Metrics: sysmetrics.New(), Xray: xrayrun.New(log),
		epoch:     randomEpoch(),
		nextSeq:   1,
		reconnect: make(chan struct{}, 1),
		st:        state.Status{XrayState: xrayrun.StateNotConfigured},
	}
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

// Run connects until ctx is cancelled, backing off between failures.
func (d *Daemon) Run(ctx context.Context) error {
	id, err := d.Dir.Load()
	if err != nil {
		return err
	}
	tlsCfg, err := clientTLS(id)
	if err != nil {
		return err
	}
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := d.session(ctx, id, tlsCfg)
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
		JournalEpoch:        d.epoch,
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
	d.setStatus(func(s *state.Status) { s.Connected, s.LastConnected, s.LastError = true, time.Now().Unix(), "" })

	d.resetSent()
	go d.applyLoop(ctx, desiredCh, send)

	sendRound := func() error {
		hb := &pb.Heartbeat{UnixNanos: time.Now().UnixNano(), LastRttMs: rtt.Load()}
		if err := send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_Heartbeat{Heartbeat: hb}}); err != nil {
			return err
		}
		d.Metrics.OnlineClients = uint32(d.Xray.OnlineUsers())
		if err := send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_SystemMetrics{SystemMetrics: d.Metrics.Sample()}}); err != nil {
			return err
		}
		xs, xerr, xver := d.Xray.Status()
		d.setStatus(func(s *state.Status) { s.XrayState, s.ConfigVersion = xs, xver })
		if err := send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_XrayStatus{XrayStatus: &pb.XrayStatus{
			State: xs, Error: xerr, Version: xrayrun.Version(), AppliedStateVersion: xver,
		}}}); err != nil {
			return err
		}
		return d.sendTraffic(send)
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
