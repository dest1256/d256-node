// Package journal is the node's encrypted, crash-safe queue of traffic usage the panel has not
// acknowledged yet. It is written only while the panel is unreachable (and on shutdown); while the link
// is up the usage travels from memory.
//
// Sequence numbers are monotone per epoch. The epoch and the next sequence are stored only on a clean
// close. If the previous run did not close cleanly the node may have handed out sequence numbers the
// journal never saw, so a fresh epoch starts and the surviving records are renumbered: the panel then
// forgets its old position instead of silently treating new usage as already counted.
package journal

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"

	"github.com/dest1256/d256-node/internal/vault"
)

const (
	bucketBatches = "batches"
	bucketMeta    = "meta"

	keyEpoch    = "epoch"
	keyNextSeq  = "next_seq"
	keyClean    = "clean"
	purposeRow  = "journal/batch"
	purposeMeta = "journal/meta"

	// EpochLimit keeps the epoch inside the panel's signed 64-bit column.
	EpochLimit = uint64(1) << 62

	// DefaultMaxBytes is the logical size at which the journal starts merging old records.
	DefaultMaxBytes = int64(256) << 20
)

// Batch is a contiguous sequence range of usage deltas.
type Batch struct {
	From, To   uint64
	Start, End int64 // unix seconds of the measurement span
	Entries    []*pb.TrafficEntry
	Conns      []*pb.ConnEntry
}

// Proto converts the batch to its wire form.
func (b Batch) Proto() *pb.TrafficBatch {
	return &pb.TrafficBatch{FromSeq: b.From, ToSeq: b.To, Entries: b.Entries, StartUnix: b.Start, EndUnix: b.End, Conns: b.Conns}
}

// Journal is safe for concurrent use.
type Journal struct {
	mu       sync.Mutex
	db       *bolt.DB
	sealer   *vault.Sealer
	maxBytes int64
	size     int64 // logical bytes of sealed rows
	epoch    uint64
	nextSeq  uint64
	closed   bool
}

func u64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

func randomEpoch() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])%(EpochLimit-1) + 1
}

// Open opens or creates the journal at path. maxBytes <= 0 selects DefaultMaxBytes.
func Open(path string, sealer *vault.Sealer, maxBytes int64) (*Journal, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	j := &Journal{db: db, sealer: sealer, maxBytes: maxBytes}
	if err := j.load(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return j, nil
}

func (j *Journal) load() error {
	return j.db.Update(func(tx *bolt.Tx) error {
		batches, err := tx.CreateBucketIfNotExists([]byte(bucketBatches))
		if err != nil {
			return err
		}
		meta, err := tx.CreateBucketIfNotExists([]byte(bucketMeta))
		if err != nil {
			return err
		}
		epoch, nextSeq, clean := j.readMeta(meta)
		if !clean || epoch == 0 || nextSeq == 0 {
			// New epoch: survivors keep their order but start again at sequence 1.
			epoch = randomEpoch()
			nextSeq = 1
			var rows [][]byte
			c := batches.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				rows = append(rows, append([]byte(nil), v...))
				_ = k
			}
			if err := tx.DeleteBucket([]byte(bucketBatches)); err != nil {
				return err
			}
			if batches, err = tx.CreateBucket([]byte(bucketBatches)); err != nil {
				return err
			}
			for _, raw := range rows {
				b, err := j.openRow(raw)
				if err != nil {
					continue // an unreadable row cannot be recovered
				}
				b.From, b.To = nextSeq, nextSeq
				nextSeq++
				if err := j.putRow(batches, b); err != nil {
					return err
				}
			}
		}
		j.epoch, j.nextSeq = epoch, nextSeq
		j.size = j.scanSize(batches)
		// The journal is dirty from now until Close marks it clean.
		return j.putMeta(meta, keyClean, []byte{0})
	})
}

func (j *Journal) readMeta(meta *bolt.Bucket) (epoch, next uint64, clean bool) {
	get := func(key string) []byte {
		raw := meta.Get([]byte(key))
		if raw == nil {
			return nil
		}
		v, err := j.sealer.Open(raw, purposeMeta+"/"+key)
		if err != nil {
			return nil
		}
		return v
	}
	if v := get(keyEpoch); len(v) == 8 {
		epoch = binary.BigEndian.Uint64(v)
	}
	if v := get(keyNextSeq); len(v) == 8 {
		next = binary.BigEndian.Uint64(v)
	}
	if v := get(keyClean); len(v) == 1 && v[0] == 1 {
		clean = true
	}
	return epoch, next, clean
}

// putMeta stores one meta value under its own purpose string.
func (j *Journal) putMeta(meta *bolt.Bucket, key string, v []byte) error {
	return meta.Put([]byte(key), j.sealer.Seal(v, purposeMeta+"/"+key))
}

func (j *Journal) scanSize(b *bolt.Bucket) int64 {
	var n int64
	_ = b.ForEach(func(_, v []byte) error { n += int64(len(v)); return nil })
	return n
}

func (j *Journal) putRow(b *bolt.Bucket, batch Batch) error {
	raw, err := proto.Marshal(batch.Proto())
	if err != nil {
		return err
	}
	return b.Put(u64(batch.From), j.sealer.Seal(raw, purposeRow))
}

func (j *Journal) openRow(sealed []byte) (Batch, error) {
	raw, err := j.sealer.Open(sealed, purposeRow)
	if err != nil {
		return Batch{}, err
	}
	var p pb.TrafficBatch
	if err := proto.Unmarshal(raw, &p); err != nil {
		return Batch{}, err
	}
	return Batch{From: p.FromSeq, To: p.ToSeq, Start: p.StartUnix, End: p.EndUnix, Entries: p.Entries, Conns: p.Conns}, nil
}

// Epoch identifies this node's sequence space towards the panel.
func (j *Journal) Epoch() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.epoch
}

// NextSeq returns the next sequence number that will be assigned.
func (j *Journal) NextSeq() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.nextSeq
}

// Size is the logical size of the stored records in bytes.
func (j *Journal) Size() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.size
}

// AllocSeq reserves the next sequence number for a batch kept in memory.
func (j *Journal) AllocSeq() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.nextSeq
	j.nextSeq++
	return s
}

// Put stores batches (overwriting a record with the same first sequence) and advances the sequence
// counter past them. It is the only write path: Append reserves a number and calls it.
func (j *Journal) Put(batches ...Batch) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errors.New("journal is closed")
	}
	err := j.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketBatches))
		for _, batch := range batches {
			old := b.Get(u64(batch.From))
			if err := j.putRow(b, batch); err != nil {
				return err
			}
			if batch.To >= j.nextSeq {
				j.nextSeq = batch.To + 1
			}
			j.size -= int64(len(old))
			j.size += int64(len(b.Get(u64(batch.From))))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return j.compactLocked()
}

// Append assigns the next sequence number to a fresh batch and stores it.
func (j *Journal) Append(entries []*pb.TrafficEntry, start, end int64) (Batch, error) {
	seq := j.AllocSeq()
	b := Batch{From: seq, To: seq, Start: start, End: end, Entries: entries}
	return b, j.Put(b)
}

// Batches returns every stored batch in sequence order.
func (j *Journal) Batches() ([]Batch, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Batch
	err := j.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketBatches)).ForEach(func(_, v []byte) error {
			b, err := j.openRow(v)
			if err != nil {
				return err
			}
			out = append(out, b)
			return nil
		})
	})
	return out, err
}

// Ack deletes everything the panel has acknowledged (records ending at or before acked).
func (j *Journal) Ack(acked uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	return j.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketBatches))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			batch, err := j.openRow(v)
			if err != nil {
				return err
			}
			if batch.To > acked {
				break
			}
			j.size -= int64(len(v))
			if err := c.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close stores the epoch and the next sequence and marks the journal clean.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	err := j.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(bucketMeta))
		if err := j.putMeta(meta, keyEpoch, u64(j.epoch)); err != nil {
			return err
		}
		if err := j.putMeta(meta, keyNextSeq, u64(j.nextSeq)); err != nil {
			return err
		}
		return j.putMeta(meta, keyClean, []byte{1})
	})
	if cerr := j.db.Close(); err == nil {
		err = cerr
	}
	return err
}
