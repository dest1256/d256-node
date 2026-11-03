package xrayrun

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"

	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/proxy/vless"
	protobuf "google.golang.org/protobuf/proto"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

const (
	uuidA = "6b0b0e3e-41b2-4f1c-9a16-0d5c1c1f3a11"
	uuidB = "0c1c4b34-7d68-4d6a-8d5e-3f7a1a1b9c22"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func config(port int, clients ...string) []byte {
	cl := ""
	for i, c := range clients {
		if i > 0 {
			cl += ","
		}
		cl += c
	}
	return fmt.Appendf(nil, `{
  "log": {"loglevel": "none"},
  "stats": {},
  "policy": {"levels": {"0": {"statsUserUplink": true, "statsUserDownlink": true}},
             "system": {"statsInboundUplink": true, "statsInboundDownlink": true}},
  "inbounds": [{"tag": "in1", "listen": "127.0.0.1", "port": %d, "protocol": "vless",
                "settings": {"clients": [%s], "decryption": "none"}}],
  "outbounds": [{"protocol": "freedom", "tag": "direct", "settings": {"finalRules": [{"action": "allow"}]}}]
}`, port, cl)
}

func client(id, email string) string {
	return fmt.Sprintf(`{"id": %q, "email": %q}`, id, email)
}

func newRunner(t *testing.T) *Runner {
	t.Helper()
	r := New(slog.New(slog.DiscardHandler))
	t.Cleanup(r.Stop)
	return r
}

func hasUser(t *testing.T, r *Runner, tag, email string) bool {
	t.Helper()
	mgr := r.inst.GetFeature(inbound.ManagerType()).(inbound.Manager)
	h, err := mgr.GetHandler(context.Background(), tag)
	if err != nil {
		t.Fatal(err)
	}
	um := h.(proxy.GetInbound).GetInbound().(proxy.UserManager)
	return um.GetUser(context.Background(), email) != nil
}

func TestApplyStartsRestartsAndRejectsBadConfig(t *testing.T) {
	r := newRunner(t)
	if st, _, _ := r.Status(); st != StateNotConfigured {
		t.Fatalf("initial state %q", st)
	}
	port := freePort(t)
	if err := r.Apply(&pb.DesiredState{Version: 1, XrayConfigJson: config(port, client(uuidA, "a@x"))}); err != nil {
		t.Fatal(err)
	}
	if st, _, v := r.Status(); st != StateRunning || v != 1 {
		t.Fatalf("after apply: %q v%d", st, v)
	}
	if !hasUser(t, r, "in1", "a@x") {
		t.Fatal("user missing after start")
	}

	// A config that cannot be parsed is rejected and the running one is untouched.
	if err := r.Apply(&pb.DesiredState{Version: 2, XrayConfigJson: []byte(`{"inbounds": [{"protocol": "nonsense"}]}`)}); err == nil {
		t.Fatal("bad config must be rejected")
	}
	if st, errText, v := r.Status(); st != StateRunning || v != 1 || errText == "" {
		t.Fatalf("after bad config: %q v%d err=%q", st, v, errText)
	}
	if !hasUser(t, r, "in1", "a@x") {
		t.Fatal("the previous config must keep running")
	}

	// A config that parses but cannot start (port already taken) restores the previous one.
	busy, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	if err := r.Apply(&pb.DesiredState{Version: 3, XrayConfigJson: config(busyPort, client(uuidB, "b@x"))}); err == nil {
		t.Fatal("start failure must be reported")
	}
	if st, _, v := r.Status(); st != StateRunning || v != 1 {
		t.Fatalf("after failed start: %q v%d", st, v)
	}
	if !hasUser(t, r, "in1", "a@x") || hasUser(t, r, "in1", "b@x") {
		t.Fatal("the previous config was not restored")
	}

	// Same config again is a no-op that only advances the version.
	if err := r.Apply(&pb.DesiredState{Version: 4, XrayConfigJson: config(port, client(uuidA, "a@x"))}); err != nil {
		t.Fatal(err)
	}
	if _, _, v := r.Status(); v != 4 {
		t.Fatalf("version = %d", v)
	}
}

func userProto(t *testing.T, id, email string) []byte {
	t.Helper()
	raw, err := protobuf.Marshal(&protocol.User{Email: email, Account: serial.ToTypedMessage(&vless.Account{Id: id})})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestHotOpsChangeUsersWithoutRestart(t *testing.T) {
	r := newRunner(t)
	port := freePort(t)
	if err := r.Apply(&pb.DesiredState{Version: 1, XrayConfigJson: config(port, client(uuidA, "a@x"))}); err != nil {
		t.Fatal(err)
	}
	before := r.inst

	next := config(port, client(uuidB, "b@x"))
	ds := &pb.DesiredState{
		Version: 2, XrayConfigJson: next, BaseVersion: 1, HotOpsValid: true,
		HotOps: []*pb.HotOp{
			{Op: &pb.HotOp_RemoveUser{RemoveUser: &pb.RemoveUser{InboundTag: "in1", Email: "a@x"}}},
			{Op: &pb.HotOp_RemoveUser{RemoveUser: &pb.RemoveUser{InboundTag: "in1", Email: "b@x", BestEffort: true}}},
			{Op: &pb.HotOp_AddUser{AddUser: &pb.AddUser{InboundTag: "in1", User: userProto(t, uuidB, "b@x")}}},
		},
	}
	if err := r.Apply(ds); err != nil {
		t.Fatal(err)
	}
	if r.inst != before {
		t.Fatal("a hot update must not restart the core")
	}
	if hasUser(t, r, "in1", "a@x") || !hasUser(t, r, "in1", "b@x") {
		t.Fatal("users were not swapped")
	}
	if _, _, v := r.Status(); v != 2 {
		t.Fatalf("version = %d", v)
	}

	// Re-adding an existing user replaces it instead of failing.
	ds = &pb.DesiredState{
		Version: 3, XrayConfigJson: config(port, client(uuidB, "b@x"), client(uuidA, "a@x")), BaseVersion: 2, HotOpsValid: true,
		HotOps: []*pb.HotOp{{Op: &pb.HotOp_AddUser{AddUser: &pb.AddUser{InboundTag: "in1", User: userProto(t, uuidB, "b@x")}}}},
	}
	if err := r.Apply(ds); err != nil {
		t.Fatal(err)
	}

	// A plan built on a version the node does not run falls back to a restart with the full config.
	ds = &pb.DesiredState{Version: 9, XrayConfigJson: config(port, client(uuidA, "a@x")), BaseVersion: 7, HotOpsValid: true}
	if err := r.Apply(ds); err != nil {
		t.Fatal(err)
	}
	if r.inst == before {
		t.Fatal("a stale base must restart the core")
	}
	if !hasUser(t, r, "in1", "a@x") || hasUser(t, r, "in1", "b@x") {
		t.Fatal("full config was not applied")
	}

	// A failing hot op falls back to the full config as well.
	ds = &pb.DesiredState{
		Version: 10, XrayConfigJson: config(port, client(uuidB, "b@x")), BaseVersion: 9, HotOpsValid: true,
		HotOps: []*pb.HotOp{{Op: &pb.HotOp_AddUser{AddUser: &pb.AddUser{InboundTag: "missing", User: userProto(t, uuidB, "b@x")}}}},
	}
	if err := r.Apply(ds); err != nil {
		t.Fatal(err)
	}
	if !hasUser(t, r, "in1", "b@x") || hasUser(t, r, "in1", "a@x") {
		t.Fatal("fallback restart did not apply the full config")
	}
}

func counter(t *testing.T, r *Runner, name string) stats.Counter {
	t.Helper()
	sm := r.inst.GetFeature(stats.ManagerType()).(stats.Manager)
	c, err := sm.RegisterCounter(name)
	if err != nil {
		c = sm.GetCounter(name)
	}
	if c == nil {
		t.Fatalf("no counter %s", name)
	}
	return c
}

func TestCollectResetsAndSurvivesRestart(t *testing.T) {
	r := newRunner(t)
	port := freePort(t)
	if err := r.Apply(&pb.DesiredState{Version: 1, XrayConfigJson: config(port, client(uuidA, "a@x"))}); err != nil {
		t.Fatal(err)
	}
	counter(t, r, "user>>>a@x>>>traffic>>>uplink").Add(100)
	counter(t, r, "user>>>a@x>>>traffic>>>downlink").Add(250)
	counter(t, r, "inbound>>>in1>>>traffic>>>uplink").Add(7)
	counter(t, r, "inbound>>>api>>>traffic>>>uplink").Add(0)

	got := map[string][2]uint64{}
	for _, e := range r.Collect() {
		got[fmt.Sprintf("%v/%s", e.Inbound, e.Name)] = [2]uint64{e.Up, e.Down}
	}
	if got["false/a@x"] != [2]uint64{100, 250} || got["true/in1"] != [2]uint64{7, 0} || len(got) != 2 {
		t.Fatalf("collected %v", got)
	}
	if again := r.Collect(); len(again) != 0 {
		t.Fatalf("counters were not reset: %v", again)
	}

	// Usage that has not been collected yet is kept across a restart of the core.
	counter(t, r, "user>>>a@x>>>traffic>>>uplink").Add(40)
	if err := r.Apply(&pb.DesiredState{Version: 2, XrayConfigJson: config(port, client(uuidB, "b@x"))}); err != nil {
		t.Fatal(err)
	}
	counter(t, r, "user>>>a@x>>>traffic>>>uplink").Add(2)
	var up uint64
	for _, e := range r.Collect() {
		if !e.Inbound && e.Name == "a@x" {
			up += e.Up
		}
	}
	if up != 42 {
		t.Fatalf("usage across restart = %d, want 42", up)
	}
}
