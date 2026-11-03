package sysmetrics

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// parseSockstat sums the "inuse" counters of the given protocol prefixes (e.g. "TCP", "TCP6")
// from /proc/net/sockstat-formatted text.
func parseSockstat(text string, protos ...string) uint32 {
	var total uint64
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		match := false
		for _, p := range protos {
			if name == p {
				match = true
			}
		}
		if !match {
			continue
		}
		for i := 1; i+1 < len(fields); i++ {
			if fields[i] == "inuse" {
				if n, err := strconv.ParseUint(fields[i+1], 10, 32); err == nil {
					total += n
				}
			}
		}
	}
	return uint32(min(total, 1<<32-1))
}

// openSockets reads the kernel's socket counters; both are 0 where /proc is unavailable.
func openSockets() (tcp, udp uint32) {
	v4, _ := os.ReadFile("/proc/net/sockstat")
	v6, _ := os.ReadFile("/proc/net/sockstat6")
	tcp = parseSockstat(string(v4), "TCP") + parseSockstat(string(v6), "TCP6")
	udp = parseSockstat(string(v4), "UDP") + parseSockstat(string(v6), "UDP6")
	return tcp, udp
}
