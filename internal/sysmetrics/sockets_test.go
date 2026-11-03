package sysmetrics

import "testing"

func TestParseSockstat(t *testing.T) {
	v4 := "sockets: used 120\nTCP: inuse 12 orphan 0 tw 3 alloc 14 mem 1\nUDP: inuse 5 mem 1\nUDPLITE: inuse 0\nRAW: inuse 0\n"
	v6 := "TCP6: inuse 4\nUDP6: inuse 2\n"
	if got := parseSockstat(v4, "TCP") + parseSockstat(v6, "TCP6"); got != 16 {
		t.Fatalf("tcp = %d", got)
	}
	if got := parseSockstat(v4, "UDP") + parseSockstat(v6, "UDP6"); got != 7 {
		t.Fatalf("udp = %d", got)
	}
	if got := parseSockstat("garbage\nTCP: inuse x\n", "TCP"); got != 0 {
		t.Fatalf("garbage = %d", got)
	}
}
