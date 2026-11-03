package link

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"

	"github.com/dest1256/d256-node/internal/state"
)

type pki struct {
	caPEM, certPEM, keyPEM []byte
	serverCert             tls.Certificate
	pool                   *x509.CertPool
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	caKey := mustKey(t)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	issue := func(serial int64, usage x509.ExtKeyUsage, mod func(*x509.Certificate)) (*ecdsa.PrivateKey, []byte) {
		k := mustKey(t)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "leaf"},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature,
		}
		mod(tmpl)
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return k, der
	}
	sk, sder := issue(2, x509.ExtKeyUsageServerAuth, func(c *x509.Certificate) { c.DNSNames = []string{"localhost"} })
	ck, cder := issue(3, x509.ExtKeyUsageClientAuth, func(*x509.Certificate) {})
	keyDER, _ := x509.MarshalECPrivateKey(ck)
	p := &pki{
		caPEM:      pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		certPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cder}),
		keyPEM:     pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		serverCert: tls.Certificate{Certificate: [][]byte{sder}, PrivateKey: sk},
		pool:       x509.NewCertPool(),
	}
	p.pool.AddCert(ca)
	return p
}

// fakePanel plays the panel's side of the link and applies traffic batches the way the real one does.
type fakePanel struct {
	pb.UnimplementedNodeLinkServer
	mu       sync.Mutex
	hellos   []*pb.Hello
	metrics  int
	failOnce bool // drop the first connection right after the handshake
	deny     bool

	desired *pb.DesiredState // sent after the handshake when set
	limits  *pb.LimitsSnapshot
	acks    []*pb.StateAck
	batches []*pb.TrafficBatch // every batch received, duplicates included
	applied []*pb.TrafficBatch // batches accepted exactly once
	acked   uint64
	// ignoreTraffic makes the panel receive batches without ever applying or acknowledging them.
	ignoreTraffic bool
}

func (f *fakePanel) Connect(st pb.NodeLink_ConnectServer) error {
	if f.deny {
		return status.Error(codes.PermissionDenied, "certificate not accepted")
	}
	first, err := st.Recv()
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.hellos = append(f.hellos, first.GetHello())
	drop := f.failOnce && len(f.hellos) == 1
	f.mu.Unlock()
	if err := st.Send(&pb.PanelToNode{Msg: &pb.PanelToNode_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, MinProtocolVersion: 1}}}); err != nil {
		return err
	}
	if drop {
		return status.Error(codes.Unavailable, "test drop")
	}
	f.mu.Lock()
	acked := f.acked
	f.mu.Unlock()
	_ = st.Send(&pb.PanelToNode{Msg: &pb.PanelToNode_JournalAck{JournalAck: &pb.JournalAck{AckedSeq: acked}}})
	if f.desired != nil {
		_ = st.Send(&pb.PanelToNode{Msg: &pb.PanelToNode_DesiredState{DesiredState: f.desired}})
	}
	if f.limits != nil {
		_ = st.Send(&pb.PanelToNode{Msg: &pb.PanelToNode_Limits{Limits: f.limits}})
	}
	for {
		m, err := st.Recv()
		if err != nil {
			return nil
		}
		switch x := m.Msg.(type) {
		case *pb.NodeToPanel_Heartbeat:
			_ = st.Send(&pb.PanelToNode{Msg: &pb.PanelToNode_Heartbeat{Heartbeat: x.Heartbeat}})
		case *pb.NodeToPanel_SystemMetrics:
			f.mu.Lock()
			f.metrics++
			f.mu.Unlock()
		case *pb.NodeToPanel_StateAck:
			f.mu.Lock()
			f.acks = append(f.acks, x.StateAck)
			f.mu.Unlock()
		case *pb.NodeToPanel_Traffic:
			f.mu.Lock()
			f.batches = append(f.batches, x.Traffic)
			if !f.ignoreTraffic {
				if x.Traffic.FromSeq == f.acked+1 {
					f.applied = append(f.applied, x.Traffic)
					f.acked = x.Traffic.ToSeq
				}
				level := f.acked
				f.mu.Unlock()
				_ = st.Send(&pb.PanelToNode{Msg: &pb.PanelToNode_JournalAck{JournalAck: &pb.JournalAck{AckedSeq: level}}})
			} else {
				f.mu.Unlock()
			}
		}
	}
}

type testNode struct {
	d      *Daemon
	dir    state.Dir
	cancel context.CancelFunc
	done   chan struct{}
}

type harness struct {
	t    *testing.T
	pki  *pki
	dir  state.Dir
	addr string
}

// newHarness stores an enrolled identity pointing at a fixed local port, so a fake panel can be started,
// stopped and restarted on the same address.
func newHarness(t *testing.T) *harness {
	t.Helper()
	p := newPKI(t)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	h := &harness{t: t, pki: p, dir: state.Dir(t.TempDir()), addr: net.JoinHostPort("127.0.0.1", port)}
	err = h.dir.Save(&state.Identity{
		Info:   state.Info{NodeID: 1, PanelURL: "https://localhost", GRPCAddr: net.JoinHostPort("localhost", port)},
		KeyPEM: p.keyPEM, CertPEM: p.certPEM, CAPEM: p.caPEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// servePanel starts the fake panel on the harness address and returns a stop function.
func (h *harness) servePanel(fp *fakePanel) (stop func()) {
	h.t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", h.addr)
	if err != nil {
		h.t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{h.pki.serverCert},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: h.pki.pool,
	})))
	pb.RegisterNodeLinkServer(gs, fp)
	go func() { _ = gs.Serve(ln) }()
	var once sync.Once
	stop = func() { once.Do(gs.Stop) }
	h.t.Cleanup(stop)
	return stop
}

// startNode runs a daemon on the harness directory. closeAtEnd=false leaves Close to the test.
func (h *harness) startNode(opts Options) *testNode {
	h.t.Helper()
	d, err := New(h.dir, slog.New(slog.DiscardHandler), opts)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &testNode{d: d, dir: h.dir, cancel: cancel, done: make(chan struct{})}
	go func() { _ = d.Run(ctx); close(n.done) }()
	return n
}

// stop ends the run loop and closes the daemon cleanly.
func (n *testNode) stop() {
	n.cancel()
	<-n.done
	n.d.Close()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fakePanel) snapshot() (hellos int, batches, applied []*pb.TrafficBatch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hellos), append([]*pb.TrafficBatch(nil), f.batches...), append([]*pb.TrafficBatch(nil), f.applied...)
}

// setCollector swaps the usage source while the daemon may be ticking.
func (n *testNode) setCollector(fn func() []*pb.TrafficEntry) {
	n.d.tickMu.Lock()
	n.d.collectUsage = fn
	n.d.tickMu.Unlock()
}

func scripted(entries ...[]*pb.TrafficEntry) func() []*pb.TrafficEntry {
	var mu sync.Mutex
	i := 0
	return func() []*pb.TrafficEntry {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(entries) {
			return nil
		}
		i++
		return entries[i-1]
	}
}

func usage(name string, up, down uint64) []*pb.TrafficEntry {
	return []*pb.TrafficEntry{{Name: name, Up: up, Down: down}}
}

func listenPort(t *testing.T) int {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func vlessConfig(port int, clients ...string) []byte {
	var cl []string
	for _, e := range clients {
		cl = append(cl, fmt.Sprintf(`{"id":"6b0b0e3e-41b2-4f1c-9a16-0d5c1c1f3a11","email":%q}`, e))
	}
	return []byte(`{"log":{"loglevel":"none"},"stats":{},"inbounds":[{"tag":"in1","listen":"127.0.0.1","port":` + strconv.Itoa(port) +
		`,"protocol":"vless","settings":{"clients":[` + strings.Join(cl, ",") + `],"decryption":"none"}}],"outbounds":[{"protocol":"freedom","tag":"direct"}]}`)
}

func init() { tick = 150 * time.Millisecond }

func TestDaemonConnectsAndReportsMetrics(t *testing.T) {
	h := newHarness(t)
	fp := &fakePanel{}
	h.servePanel(fp)
	n := h.startNode(Options{})
	defer n.stop()
	waitFor(t, "hello and metrics", func() bool {
		hellos, _, _ := fp.snapshot()
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return hellos == 1 && fp.metrics >= 1
	})
	if hello := fp.hellos[0]; hello.ProtocolVersion != 1 || hello.Os == "" || hello.JournalEpoch == 0 || hello.JournalEpoch != n.d.jr.Epoch() {
		t.Fatalf("hello = %+v", hello)
	}
	waitFor(t, "status connected", func() bool {
		s, err := n.dir.ReadStatus()
		return err == nil && s.Connected
	})
}

func TestDaemonReconnectsAfterDrop(t *testing.T) {
	h := newHarness(t)
	fp := &fakePanel{failOnce: true}
	h.servePanel(fp)
	n := h.startNode(Options{})
	defer n.stop()
	waitFor(t, "second connection", func() bool {
		hellos, _, _ := fp.snapshot()
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return hellos >= 2 && fp.metrics >= 1
	})
}

func TestDaemonReportsRejection(t *testing.T) {
	h := newHarness(t)
	h.servePanel(&fakePanel{deny: true})
	n := h.startNode(Options{})
	defer n.stop()
	waitFor(t, "rejection in status", func() bool {
		s, err := n.dir.ReadStatus()
		return err == nil && !s.Connected && s.LastError != ""
	})
}

func TestDaemonRejectsUntrustedServer(t *testing.T) {
	h := newHarness(t)
	other := newPKI(t) // a different CA: the node must not talk to a server it cannot verify
	fp := &fakePanel{}
	h.pki = &pki{caPEM: other.caPEM, certPEM: h.pki.certPEM, keyPEM: h.pki.keyPEM, serverCert: h.pki.serverCert, pool: h.pki.pool}
	_ = h.dir.Save(&state.Identity{
		Info:   state.Info{NodeID: 1, PanelURL: "https://localhost", GRPCAddr: strings.Replace(h.addr, "127.0.0.1", "localhost", 1)},
		KeyPEM: h.pki.keyPEM, CertPEM: h.pki.certPEM, CAPEM: other.caPEM,
	})
	h.servePanel(fp)
	n := h.startNode(Options{})
	defer n.stop()
	time.Sleep(1500 * time.Millisecond)
	if hellos, _, _ := fp.snapshot(); hellos != 0 {
		t.Fatal("node connected to a server not signed by the panel CA it pinned")
	}
}

func TestDaemonAppliesDesiredStateCachesItAndAcks(t *testing.T) {
	h := newHarness(t)
	port := listenPort(t)
	fp := &fakePanel{desired: &pb.DesiredState{Version: 5, XrayConfigJson: vlessConfig(port, "a@x")}}
	h.servePanel(fp)
	n := h.startNode(Options{})
	waitFor(t, "state ack", func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return len(fp.acks) == 1 && fp.acks[0].Ok && fp.acks[0].Version == 5
	})
	waitFor(t, "xray status", func() bool {
		s, err := n.dir.ReadStatus()
		return err == nil && s.XrayState == "running" && s.ConfigVersion == 5
	})
	n.stop()

	// A restart without any panel: Xray comes up from the sealed cache.
	h2 := &harness{t: t, pki: h.pki, dir: h.dir, addr: h.addr}
	n2 := h2.startNode(Options{})
	defer n2.stop()
	if cfg := n2.d.Xray.Config(); !strings.Contains(string(cfg), "a@x") {
		t.Fatalf("xray was not started from the cache: %s", cfg)
	}
	if st, _, v := n2.d.Xray.Status(); st != "running" || v != 5 {
		t.Fatalf("status after restart: %s v%d", st, v)
	}
}

func TestOfflineUsageIsJournaledThenDeliveredExactlyOnce(t *testing.T) {
	h := newHarness(t)
	n := h.startNode(Options{})
	defer n.stop()
	n.setCollector(scripted(usage("alice", 1, 10), usage("alice", 2, 20), usage("bob", 3, 30)))

	// The panel is unreachable: every tick lands on disk.
	for i := 0; i < 3; i++ {
		if err := n.d.tick(nil); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := n.d.jr.Batches()
	if err != nil || len(stored) != 3 {
		t.Fatalf("journal = %+v %v", stored, err)
	}
	if n.d.journalBytes() == 0 {
		t.Fatal("journal size must be reported")
	}

	fp := &fakePanel{}
	h.servePanel(fp)
	waitFor(t, "all batches applied", func() bool {
		_, _, applied := fp.snapshot()
		return len(applied) == 3
	})
	_, _, applied := fp.snapshot()
	for i, b := range applied {
		if b.FromSeq != uint64(i+1) || b.ToSeq != uint64(i+1) {
			t.Fatalf("applied out of order: %+v", applied)
		}
	}
	waitFor(t, "journal drained by the acknowledgement", func() bool {
		left, _ := n.d.jr.Batches()
		return len(left) == 0
	})
}

func TestUnacknowledgedUsageSurvivesARestartAndIsNotLostOrDoubled(t *testing.T) {
	h := newHarness(t)
	fp := &fakePanel{ignoreTraffic: true}
	stopFirst := h.servePanel(fp)
	n := h.startNode(Options{})
	waitFor(t, "connected", func() bool { s, err := n.dir.ReadStatus(); return err == nil && s.Connected })
	n.setCollector(scripted(usage("alice", 5, 50), usage("alice", 6, 60)))
	waitFor(t, "both batches sent but never acknowledged", func() bool {
		_, batches, _ := fp.snapshot()
		return len(batches) >= 2
	})
	n.stop() // clean shutdown: the unacknowledged queue is written to the journal
	stopFirst()

	// A new daemon (same data) talks to a panel that has applied nothing yet.
	fp2 := &fakePanel{}
	h.servePanel(fp2)
	n2 := h.startNode(Options{})
	defer n2.stop()
	waitFor(t, "both batches applied once", func() bool {
		_, _, applied := fp2.snapshot()
		return len(applied) == 2
	})
	_, _, applied := fp2.snapshot()
	var up uint64
	for _, b := range applied {
		for _, e := range b.Entries {
			up += e.Up
		}
	}
	if up != 11 {
		t.Fatalf("usage seen by the panel = %d, want 11", up)
	}
	if n2.d.jr.Epoch() == 0 {
		t.Fatal("epoch missing")
	}
}

func TestLimitsAreEnforcedOfflineAndRestoredByThePanel(t *testing.T) {
	h := newHarness(t)
	port := listenPort(t)
	cfg := vlessConfig(port, "alice@x", "bob@x")
	fp := &fakePanel{
		desired: &pb.DesiredState{Version: 3, XrayConfigJson: cfg},
		limits:  &pb.LimitsSnapshot{Clients: []*pb.ClientLimit{{Email: "alice@x", QuotaBytes: 1000, UsedBytes: 900}, {Email: "bob@x"}}},
	}
	stop := h.servePanel(fp)
	n := h.startNode(Options{})
	defer n.stop()
	waitFor(t, "state and limits cached", func() bool {
		return strings.Contains(string(n.d.Xray.Config()), "alice@x")
	})
	time.Sleep(300 * time.Millisecond) // let the limits snapshot reach the enforcer
	stop()
	waitFor(t, "link down", func() bool { s, err := n.dir.ReadStatus(); return err == nil && !s.Connected })

	// Offline: alice crosses her quota.
	n.setCollector(scripted(usage("alice@x", 60, 60)))
	if err := n.d.tick(nil); err != nil {
		t.Fatal(err)
	}
	if got := string(n.d.Xray.Config()); strings.Contains(got, "alice@x") || !strings.Contains(got, "bob@x") {
		t.Fatalf("offline enforcement: alice must be gone and bob kept: %s", got)
	}

	// The panel is back and still allows her: its state replaces the node's.
	h.servePanel(fp)
	waitFor(t, "panel state restored", func() bool {
		return strings.Contains(string(n.d.Xray.Config()), "alice@x")
	})
}
