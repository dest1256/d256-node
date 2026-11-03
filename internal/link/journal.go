package link

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"maps"
	"time"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

const (
	// maxPendingBatches bounds memory while the panel is away; older batches are merged, never dropped.
	maxPendingBatches = 200
	// epochLimit keeps the epoch inside the panel's signed 64-bit column.
	epochLimit = uint64(1) << 62
)

// trafficBatch is a contiguous sequence range [from, to] with its usage deltas.
type trafficBatch struct {
	from, to uint64
	entries  []*pb.TrafficEntry
}

func randomEpoch() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])%(epochLimit-1) + 1
}

func enqueue(ch chan *pb.DesiredState, ds *pb.DesiredState) {
	for {
		select {
		case ch <- ds:
			return
		default:
			select {
			case <-ch: // the newest state supersedes an unapplied older one
			default:
			}
		}
	}
}

func (d *Daemon) appliedVersion() uint64 {
	_, _, v := d.Xray.Status()
	return v
}

// applyLoop applies desired states one at a time and acknowledges each outcome.
func (d *Daemon) applyLoop(ctx context.Context, ch <-chan *pb.DesiredState, send func(*pb.NodeToPanel) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case ds := <-ch:
			start := time.Now()
			err := d.Xray.Apply(ds)
			ack := &pb.StateAck{Version: ds.Version, Ok: err == nil}
			if err != nil {
				ack.Error = err.Error()
				d.Log.Error("desired state rejected", "version", ds.Version, "err", err)
			} else {
				d.Log.Info("desired state applied", "version", ds.Version, "took", time.Since(start).Round(time.Millisecond))
			}
			_ = send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_StateAck{StateAck: ack}})
		}
	}
}

// collect turns the usage accumulated in the core into the next pending batch.
func (d *Daemon) collect() {
	entries := d.Xray.Collect()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(entries) > 0 {
		d.pending = append(d.pending, trafficBatch{from: d.nextSeq, to: d.nextSeq, entries: entries})
		d.nextSeq++
	}
	for len(d.pending) > maxPendingBatches {
		a, b := d.pending[0], d.pending[1]
		d.pending = append([]trafficBatch{mergeBatches(a, b)}, d.pending[2:]...)
		if d.sentTo >= a.from && d.sentTo < b.to {
			d.sentTo = a.from - 1 // the merged range must be resent as a whole
		}
	}
}

func mergeBatches(a, b trafficBatch) trafficBatch {
	type k struct {
		in   bool
		name string
	}
	sum := map[k]*pb.TrafficEntry{}
	for _, e := range append(append([]*pb.TrafficEntry{}, a.entries...), b.entries...) {
		key := k{e.Inbound, e.Name}
		if cur, ok := sum[key]; ok {
			cur.Up += e.Up
			cur.Down += e.Down
		} else {
			sum[key] = &pb.TrafficEntry{Inbound: e.Inbound, Name: e.Name, Up: e.Up, Down: e.Down}
		}
	}
	out := make([]*pb.TrafficEntry, 0, len(sum))
	for e := range maps.Values(sum) {
		out = append(out, e)
	}
	return trafficBatch{from: a.from, to: b.to, entries: out}
}

// sendTraffic collects fresh usage and (re)sends every batch the panel has not seen yet.
func (d *Daemon) sendTraffic(send func(*pb.NodeToPanel) error) error {
	d.collect()
	d.mu.Lock()
	var todo []trafficBatch
	for _, b := range d.pending {
		if b.to > d.sentTo {
			todo = append(todo, b)
		}
	}
	d.mu.Unlock()
	for _, b := range todo {
		if err := send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_Traffic{Traffic: &pb.TrafficBatch{
			FromSeq: b.from, ToSeq: b.to, Entries: b.entries,
		}}}); err != nil {
			return err
		}
		d.mu.Lock()
		d.sentTo = b.to
		d.mu.Unlock()
	}
	return nil
}

// onJournalAck drops acknowledged batches. An acknowledgement behind what was sent means the panel
// did not apply something, so sending restarts from its position.
func (d *Daemon) onJournalAck(acked uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	keep := d.pending[:0]
	for _, b := range d.pending {
		if b.to > acked {
			keep = append(keep, b)
		}
	}
	d.pending = keep
	if acked < d.sentTo {
		d.sentTo = acked
	}
}

// resetSent makes the next send start from the first unacknowledged batch (new session).
func (d *Daemon) resetSent() {
	d.mu.Lock()
	d.sentTo = 0
	d.mu.Unlock()
}
