package link

import (
	"context"
	"time"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"

	"github.com/dest1256/d256-node/internal/cache"
	"github.com/dest1256/d256-node/internal/journal"
)

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

// applyLoop applies desired states one at a time, remembers each applied state for offline starts and
// acknowledges every outcome.
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
				// The panel's state wins over anything the node enforced on its own.
				d.enf.Reset()
				d.saveCache(ds.Version, ds.XrayConfigJson)
			}
			_ = send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_StateAck{StateAck: ack}})
		}
	}
}

func (d *Daemon) saveCache(version uint64, cfg []byte) {
	d.mu.Lock()
	if cfg != nil {
		d.cacheVersion, d.cacheConfig = version, cfg
	}
	st := cache.State{Version: d.cacheVersion, ConfigJSON: d.cacheConfig, Limits: d.limits}
	d.mu.Unlock()
	if st.ConfigJSON == nil {
		return
	}
	if err := cache.Save(d.Dir.CachePath(), d.sealer, st); err != nil {
		d.Log.Warn("saving the state cache failed", "err", err)
	}
}

// tick turns the usage measured since the last tick into a batch. While the panel is unreachable the
// batch goes straight to disk and the node enforces client limits on its own; while the link is up
// the batch is held in memory and sent (send != nil).
func (d *Daemon) tick(send func(*pb.NodeToPanel) error) error {
	d.tickMu.Lock()
	defer d.tickMu.Unlock()

	entries := d.collectUsage()
	now := time.Now().Unix()
	if len(entries) > 0 {
		d.enf.AddUsage(entries)
		seq := d.jr.AllocSeq()
		b := journal.Batch{From: seq, To: seq, Start: d.lastTick, End: now, Entries: entries}
		d.mu.Lock()
		online := d.online
		if online {
			d.pending = append(d.pending, b)
		}
		d.mu.Unlock()
		if !online {
			if err := d.jr.Put(b); err != nil {
				d.Log.Error("journal write failed, usage kept in memory", "err", err)
				d.mu.Lock()
				d.pending = append(d.pending, b)
				d.mu.Unlock()
			}
		}
	}
	d.lastTick = now

	d.mu.Lock()
	online := d.online
	d.mu.Unlock()
	if !online {
		if over := d.enf.Violations(now); len(over) > 0 {
			d.Log.Warn("panel unreachable: dropping clients over their limit", "clients", over)
			if err := d.Xray.Drop(over); err != nil {
				d.Log.Error("dropping clients failed", "err", err)
			}
		}
	}
	if send != nil {
		return d.sendPending(send)
	}
	return nil
}

// sendPending (re)sends every batch the panel has not seen yet.
func (d *Daemon) sendPending(send func(*pb.NodeToPanel) error) error {
	d.mu.Lock()
	var todo []journal.Batch
	for _, b := range d.pending {
		if b.To > d.sentTo {
			todo = append(todo, b)
		}
	}
	d.mu.Unlock()
	for _, b := range todo {
		if err := send(&pb.NodeToPanel{Msg: &pb.NodeToPanel_Traffic{Traffic: b.Proto()}}); err != nil {
			return err
		}
		d.mu.Lock()
		d.sentTo = b.To
		d.mu.Unlock()
	}
	return nil
}

// onJournalAck drops acknowledged batches from memory and disk. An acknowledgement behind what was
// sent means the panel did not apply something, so sending restarts from its position.
func (d *Daemon) onJournalAck(acked uint64) {
	d.mu.Lock()
	keep := d.pending[:0]
	for _, b := range d.pending {
		if b.To > acked {
			keep = append(keep, b)
		}
	}
	d.pending = keep
	if acked < d.sentTo {
		d.sentTo = acked
	}
	d.mu.Unlock()
	if err := d.jr.Ack(acked); err != nil {
		d.Log.Warn("journal ack failed", "err", err)
	}
}

// goOnline loads what the disk journal holds (usage from the offline period) ahead of the memory queue.
func (d *Daemon) goOnline() {
	stored, err := d.jr.Batches()
	if err != nil {
		d.Log.Error("reading the journal failed", "err", err)
	}
	d.mu.Lock()
	byFrom := make(map[uint64]bool, len(d.pending))
	for _, b := range d.pending {
		byFrom[b.From] = true
	}
	merged := make([]journal.Batch, 0, len(stored)+len(d.pending))
	for _, b := range stored {
		if !byFrom[b.From] {
			merged = append(merged, b)
		}
	}
	d.pending = append(merged, d.pending...)
	d.sentTo = 0
	d.online = true
	d.mu.Unlock()
}

// goOffline writes the in-memory queue to disk so nothing unacknowledged is lost.
func (d *Daemon) goOffline() {
	d.mu.Lock()
	d.online = false
	pending := d.pending
	d.pending = nil
	d.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	if err := d.jr.Put(pending...); err != nil {
		d.Log.Error("spilling the queue to the journal failed", "err", err)
		d.mu.Lock()
		d.pending = append(pending, d.pending...)
		d.mu.Unlock()
	}
}

// journalBytes is what the dashboard shows: the disk journal plus the in-memory queue.
func (d *Daemon) journalBytes() uint64 {
	n := d.jr.Size()
	d.mu.Lock()
	for _, b := range d.pending {
		n += int64(len(b.Entries)) * 48
	}
	d.mu.Unlock()
	return uint64(max(n, 0))
}
