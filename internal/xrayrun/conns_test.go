package xrayrun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
	xproxy "golang.org/x/net/proxy"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

// echoServer accepts connections and echoes every byte back.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// startClient runs an Xray client: a local SOCKS5 inbound whose traffic goes to the node over VLESS,
// optionally from a chosen source address.
func startClient(t *testing.T, nodePort int, sendThrough string) (socksAddr string) {
	t.Helper()
	socks := freePort(t)
	st := ""
	if sendThrough != "" {
		st = fmt.Sprintf(`"sendThrough": %q,`, sendThrough)
	}
	cfg := fmt.Sprintf(`{"log":{"loglevel":"none"},
	  "inbounds":[{"listen":"127.0.0.1","port":%d,"protocol":"socks","settings":{"udp":false}}],
	  "outbounds":[{%s"protocol":"vless","settings":{"vnext":[{"address":"127.0.0.1","port":%d,"users":[{"id":%q,"encryption":"none"}]}]}}]}`,
		socks, st, nodePort, uuidA)
	c, err := serial.LoadJSONConfig(bytes.NewReader([]byte(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	inst, err := core.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(socks))
}

func dialVia(socksAddr string, target string) (net.Conn, error) {
	d, err := xproxy.SOCKS5("tcp", socksAddr, nil, &net.Dialer{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	return d.Dial("tcp", target)
}

func roundTrip(c net.Conn, payload []byte) error {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(payload); err != nil {
		return err
	}
	got := make([]byte, len(payload))
	_, err := io.ReadFull(c, got)
	return err
}

func startNodeWithAlice(t *testing.T) (r *Runner, nodePort int) {
	t.Helper()
	r = newRunner(t)
	nodePort = freePort(t)
	if err := r.Apply(&pb.DesiredState{Version: 1, XrayConfigJson: config(nodePort, client(uuidA, "alice@x"))}); err != nil {
		t.Fatal(err)
	}
	return r, nodePort
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConnectionsAreAttributedToClientSourceAndDestination(t *testing.T) {
	r, nodePort := startNodeWithAlice(t)
	echoPort := echoServer(t)
	socks := startClient(t, nodePort, "")

	c, err := dialVia(socks, net.JoinHostPort("127.0.0.1", strconv.Itoa(echoPort)))
	if err != nil {
		t.Fatal(err)
	}
	if err := roundTrip(c, bytes.Repeat([]byte("x"), 1000)); err != nil {
		t.Fatal(err)
	}

	// While the connection is open it shows up as active, with its source and destination.
	var act *pb.ActiveConn
	waitUntil(t, "the live connection", func() bool {
		for _, a := range r.Conns().Snapshot().Conns {
			if a.Email == "alice@x" && a.Up >= 1000 && a.Down >= 1000 {
				act = a
				return true
			}
		}
		return false
	})
	if act.InboundTag != "in1" || act.SourceIp != "127.0.0.1" || act.Dest != "127.0.0.1" || act.Port != uint32(echoPort) || act.Network != "tcp" || act.StartUnix == 0 {
		t.Fatalf("active connection = %+v", act)
	}
	ips := r.Conns().Snapshot().Ips
	if len(ips) != 1 || ips[0].Email != "alice@x" || ips[0].Ip != "127.0.0.1" {
		t.Fatalf("device list = %+v", ips)
	}

	// Usage is booked per hour and destination, and the progress of a long connection is not lost
	// when it is drained before it ends.
	first := r.Conns().Drain()
	if len(first) != 1 || first[0].Conns != 1 || first[0].Up < 1000 || first[0].Down < 1000 {
		t.Fatalf("first drain = %+v", first)
	}
	if err := roundTrip(c, bytes.Repeat([]byte("y"), 500)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	waitUntil(t, "the connection to be closed", func() bool { return r.Conns().ActiveCount() == 0 })
	second := r.Conns().Drain()
	if len(second) != 1 || second[0].Conns != 0 || second[0].Up < 500 || second[0].Down < 500 {
		t.Fatalf("second drain = %+v", second)
	}
	if first[0].SourceIp != "127.0.0.1" || first[0].LastUnix < time.Now().Add(-time.Minute).Unix() || first[0].LastUnix > time.Now().Add(time.Minute).Unix() {
		t.Fatalf("source address / last use missing: %+v", first[0])
	}
	if first[0].Dest != "127.0.0.1" || first[0].Network != "tcp" || first[0].InboundTag != "in1" || first[0].HourUnix != time.Now().Truncate(time.Hour).Unix() {
		t.Fatalf("entry = %+v", first[0])
	}
	if again := r.Conns().Drain(); len(again) != 0 {
		t.Fatalf("a drained store must be empty: %+v", again)
	}
}

func TestDeviceLimitRejectsAnotherAddressButNotTheSameOne(t *testing.T) {
	r, nodePort := startNodeWithAlice(t)
	echoPort := echoServer(t)
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(echoPort))
	r.Conns().SetDeviceLimits(map[string]uint32{"alice@x": 1})

	first, err := dialVia(startClient(t, nodePort, ""), target)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := roundTrip(first, []byte("hello")); err != nil {
		t.Fatal(err)
	}

	// A second connection from the same address is the same device.
	again, err := dialVia(startClient(t, nodePort, ""), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := roundTrip(again, []byte("hello")); err != nil {
		t.Fatalf("the same address must be admitted: %v", err)
	}
	_ = again.Close()

	// A different source address is a second device and is refused while the first one is connected.
	other, err := dialVia(startClient(t, nodePort, "127.0.0.2"), target)
	if err == nil {
		err = roundTrip(other, []byte("hello"))
		_ = other.Close()
	}
	if err == nil {
		t.Fatal("a second device must be refused at the limit")
	}

	// Once the first device is gone the other address may connect.
	_ = first.Close()
	waitUntil(t, "the first device to leave", func() bool { return r.Conns().ActiveCount() == 0 })
	ok, err := dialVia(startClient(t, nodePort, "127.0.0.2"), target)
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Close()
	if err := roundTrip(ok, []byte("hello")); err != nil {
		t.Fatalf("the new device must be admitted after the first left: %v", err)
	}
}
