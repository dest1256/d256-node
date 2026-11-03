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
	"log/slog"
	"math/big"
	"net"
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

type fakePanel struct {
	pb.UnimplementedNodeLinkServer
	mu       sync.Mutex
	hellos   []*pb.Hello
	metrics  int
	failOnce bool // drop the first connection right after the handshake
	deny     bool
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
		}
	}
}

func startDaemon(t *testing.T, p *pki, fp *fakePanel) (*Daemon, context.CancelFunc) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{p.serverCert},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: p.pool,
	})))
	pb.RegisterNodeLinkServer(gs, fp)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)

	dir := state.Dir(t.TempDir())
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	err = dir.Save(&state.Identity{
		Info:   state.Info{NodeID: 1, PanelURL: "https://localhost", GRPCAddr: net.JoinHostPort("localhost", port)},
		KeyPEM: p.keyPEM, CertPEM: p.certPEM, CAPEM: p.caPEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := New(dir, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return d, cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDaemonConnectsAndReportsMetrics(t *testing.T) {
	p := newPKI(t)
	fp := &fakePanel{}
	d, _ := startDaemon(t, p, fp)
	waitFor(t, "hello and metrics", func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return len(fp.hellos) == 1 && fp.metrics >= 1
	})
	if h := fp.hellos[0]; h.ProtocolVersion != 1 || h.Os == "" {
		t.Fatalf("hello = %+v", h)
	}
	waitFor(t, "status connected", func() bool {
		s, err := d.Dir.ReadStatus()
		return err == nil && s.Connected && s.XrayState == "not_configured"
	})
}

func TestDaemonReconnectsAfterDrop(t *testing.T) {
	p := newPKI(t)
	fp := &fakePanel{failOnce: true}
	startDaemon(t, p, fp)
	waitFor(t, "second connection", func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return len(fp.hellos) >= 2 && fp.metrics >= 1
	})
}

func TestDaemonReportsRejection(t *testing.T) {
	p := newPKI(t)
	fp := &fakePanel{deny: true}
	d, _ := startDaemon(t, p, fp)
	waitFor(t, "rejection in status", func() bool {
		s, err := d.Dir.ReadStatus()
		return err == nil && !s.Connected && s.LastError != ""
	})
}

func TestDaemonRejectsUntrustedServer(t *testing.T) {
	p := newPKI(t)
	other := newPKI(t) // a different CA: the node must not talk to a server it cannot verify
	fp := &fakePanel{}
	mixed := &pki{caPEM: other.caPEM, certPEM: p.certPEM, keyPEM: p.keyPEM, serverCert: p.serverCert, pool: p.pool}
	d, _ := startDaemon(t, mixed, fp)
	time.Sleep(1500 * time.Millisecond)
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.hellos) != 0 {
		t.Fatal("node connected to a server not signed by the panel CA it pinned")
	}
	if s, err := d.Dir.ReadStatus(); err == nil && s.Connected {
		t.Fatal("status claims connected")
	}
}
