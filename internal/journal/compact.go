package journal

import (
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

const (
	// compactAbove and compactTo are fractions (of maxBytes) in eighths-of-ten: start merging above 80 %, stop at 50 %.
	compactAbove = 80
	compactTo    = 50
	hour         = int64(3600)
)

// Merge combines two adjacent batches into one covering both ranges. Usage per name is summed, so no
// traffic is ever dropped; only the time resolution gets coarser.
func Merge(a, b Batch) Batch {
	type key struct {
		inbound bool
		name    string
	}
	sum := map[key]*pb.TrafficEntry{}
	order := make([]key, 0, len(a.Entries)+len(b.Entries))
	for _, list := range [][]*pb.TrafficEntry{a.Entries, b.Entries} {
		for _, e := range list {
			k := key{e.Inbound, e.Name}
			if cur, ok := sum[k]; ok {
				cur.Up += e.Up
				cur.Down += e.Down
				continue
			}
			sum[k] = &pb.TrafficEntry{Inbound: e.Inbound, Name: e.Name, Up: e.Up, Down: e.Down}
			order = append(order, k)
		}
	}
	entries := make([]*pb.TrafficEntry, 0, len(order))
	for _, k := range order {
		entries = append(entries, sum[k])
	}
	start, end := a.Start, b.End
	if b.Start != 0 && (start == 0 || b.Start < start) {
		start = b.Start
	}
	if a.End > end {
		end = a.End
	}
	return Batch{From: a.From, To: b.To, Start: start, End: end, Entries: entries, Conns: mergeConns(a.Conns, b.Conns)}
}

func batchSize(b Batch) int64 {
	return int64(proto.Size(b.Proto())) + 40 // 40 = sealing overhead (nonce + tag) and key
}

// compactLocked merges the oldest records, first within the same hour and then across hours, until the
// journal is back under the target size. The caller holds j.mu.
func (j *Journal) compactLocked() error {
	if j.size <= j.maxBytes*compactAbove/100 {
		return nil
	}
	var batches []Batch
	err := j.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketBatches)).ForEach(func(_, v []byte) error {
			b, err := j.openRow(v)
			if err != nil {
				return err
			}
			batches = append(batches, b)
			return nil
		})
	})
	if err != nil || len(batches) < 2 {
		return err
	}
	target := j.maxBytes * compactTo / 100
	total := func(list []Batch) (n int64) {
		for _, b := range list {
			n += batchSize(b)
		}
		return n
	}

	// Pass 1: collapse neighbours that fall in the same hour.
	merged := []Batch{batches[0]}
	for _, b := range batches[1:] {
		last := &merged[len(merged)-1]
		if last.Start/hour == b.Start/hour && last.Start != 0 {
			*last = Merge(*last, b)
		} else {
			merged = append(merged, b)
		}
	}
	// Pass 2: if that is not enough, fold the oldest records together regardless of the hour.
	for total(merged) > target && len(merged) > 1 {
		merged = append([]Batch{Merge(merged[0], merged[1])}, merged[2:]...)
	}

	return j.db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket([]byte(bucketBatches)); err != nil {
			return err
		}
		b, err := tx.CreateBucket([]byte(bucketBatches))
		if err != nil {
			return err
		}
		var n int64
		for _, m := range merged {
			if err := j.putRow(b, m); err != nil {
				return err
			}
		}
		n = j.scanSize(b)
		j.size = n
		return nil
	})
}

// mergeConns sums per-destination usage that falls on the same client, inbound, destination, network
// and hour; entries of different hours stay separate, so merging never blurs the hourly resolution.
func mergeConns(a, b []*pb.ConnEntry) []*pb.ConnEntry {
	type key struct {
		email, inbound, dest, network string
		hour                          int64
	}
	sum := map[key]*pb.ConnEntry{}
	var order []key
	for _, list := range [][]*pb.ConnEntry{a, b} {
		for _, e := range list {
			k := key{e.Email, e.InboundTag, e.Dest, e.Network, e.HourUnix}
			if cur, ok := sum[k]; ok {
				cur.Up += e.Up
				cur.Down += e.Down
				cur.Conns += e.Conns
				continue
			}
			sum[k] = &pb.ConnEntry{
				Email: e.Email, InboundTag: e.InboundTag, Dest: e.Dest, Network: e.Network,
				HourUnix: e.HourUnix, Up: e.Up, Down: e.Down, Conns: e.Conns,
			}
			order = append(order, k)
		}
	}
	out := make([]*pb.ConnEntry, 0, len(order))
	for _, k := range order {
		out = append(out, sum[k])
	}
	return out
}
