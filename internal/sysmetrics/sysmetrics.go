// Package sysmetrics samples host metrics for the panel dashboard.
package sysmetrics

import (
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"

	nodelinkv1 "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

// Collector keeps the previous network counters to derive rates.
type Collector struct {
	lastAt   time.Time
	lastSent uint64
	lastRecv uint64
	cores    uint32
	mhz      float64

	// JournalBytes and OnlineClients are filled in by components that own them.
	JournalBytes  uint64
	OnlineClients uint32
}

// New primes the CPU and network baselines.
func New() *Collector {
	c := &Collector{}
	if n, err := cpu.Counts(true); err == nil {
		c.cores = uint32(n)
	}
	if info, err := cpu.Info(); err == nil && len(info) > 0 {
		c.mhz = info[0].Mhz
	}
	_, _ = cpu.Percent(0, false) // the first call only sets the baseline
	c.sampleNet()
	return c
}

func (c *Collector) sampleNet() (sent, recv uint64, rateUp, rateDown uint64) {
	io, err := gnet.IOCounters(false)
	now := time.Now()
	if err != nil || len(io) == 0 {
		return 0, 0, 0, 0
	}
	sent, recv = io[0].BytesSent, io[0].BytesRecv
	if !c.lastAt.IsZero() {
		if dt := now.Sub(c.lastAt).Seconds(); dt > 0 {
			if sent >= c.lastSent {
				rateUp = uint64(float64(sent-c.lastSent) / dt)
			}
			if recv >= c.lastRecv {
				rateDown = uint64(float64(recv-c.lastRecv) / dt)
			}
		}
	}
	c.lastAt, c.lastSent, c.lastRecv = now, sent, recv
	return sent, recv, rateUp, rateDown
}

// Sample reads the current metrics. Sources that fail are left at zero.
func (c *Collector) Sample() *nodelinkv1.SystemMetrics {
	m := &nodelinkv1.SystemMetrics{CpuCores: c.cores, CpuMhz: c.mhz, JournalBytes: c.JournalBytes, OnlineClients: c.OnlineClients}
	if p, err := cpu.Percent(0, false); err == nil && len(p) > 0 {
		m.CpuPct = p[0]
	}
	if v, err := mem.VirtualMemory(); err == nil {
		m.MemTotal, m.MemUsed = v.Total, v.Used
	}
	if s, err := mem.SwapMemory(); err == nil {
		m.SwapTotal, m.SwapUsed = s.Total, s.Used
	}
	if d, err := disk.Usage("/"); err == nil {
		m.DiskTotal, m.DiskUsed = d.Total, d.Used
	}
	m.NetSentTotal, m.NetRecvTotal, m.NetUpBps, m.NetDownBps = c.sampleNet()
	m.TcpSockets, m.UdpSockets = openSockets()
	if up, err := host.Uptime(); err == nil {
		m.UptimeSecs = up
	}
	return m
}
