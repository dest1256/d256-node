package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"

	"github.com/dest1256/d256-node/internal/vault"
)

func sealer(t *testing.T, secret string) *vault.Sealer {
	t.Helper()
	s, err := vault.FromSecret([]byte(secret), "journal-test")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func entries(name string, up, down uint64) []*pb.TrafficEntry {
	return []*pb.TrafficEntry{{Name: name, Up: up, Down: down}}
}

func open(t *testing.T, path string, max int64) *Journal {
	t.Helper()
	j, err := Open(path, sealer(t, "k"), max)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func totals(bs []Batch) (up, down uint64) {
	for _, b := range bs {
		for _, e := range b.Entries {
			up += e.Up
			down += e.Down
		}
	}
	return
}

func TestAppendAckAndCleanReopenKeepsEpochAndSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	j := open(t, path, 0)
	epoch := j.Epoch()
	if epoch == 0 || epoch >= EpochLimit {
		t.Fatalf("epoch = %d", epoch)
	}
	for i := 1; i <= 3; i++ {
		b, err := j.Append(entries("u", uint64(i), 1), 100, 105)
		if err != nil || b.From != uint64(i) {
			t.Fatalf("append %d: %+v %v", i, b, err)
		}
	}
	if err := j.Ack(2); err != nil {
		t.Fatal(err)
	}
	left, _ := j.Batches()
	if len(left) != 1 || left[0].From != 3 {
		t.Fatalf("after ack: %+v", left)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	j = open(t, path, 0)
	defer j.Close()
	if j.Epoch() != epoch {
		t.Fatalf("a clean reopen must keep the epoch: %d != %d", j.Epoch(), epoch)
	}
	if j.NextSeq() != 4 {
		t.Fatalf("next seq = %d", j.NextSeq())
	}
	left, _ = j.Batches()
	if len(left) != 1 || left[0].From != 3 {
		t.Fatalf("the unacknowledged batch must survive: %+v", left)
	}
}

func TestUncleanShutdownStartsANewEpochAndRenumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	j := open(t, path, 0)
	epoch := j.Epoch()
	for i := 0; i < 3; i++ {
		_, _ = j.Append(entries("u", 10, 20), 1, 2)
	}
	_ = j.Ack(1)
	// Simulate a crash: close the database without marking the journal clean.
	_ = j.db.Close()

	j2 := open(t, path, 0)
	defer j2.Close()
	if j2.Epoch() == epoch {
		t.Fatal("an unclean shutdown must not reuse the epoch")
	}
	bs, _ := j2.Batches()
	if len(bs) != 2 || bs[0].From != 1 || bs[1].From != 2 || bs[0].To != 1 || bs[1].To != 2 {
		t.Fatalf("survivors must be renumbered from 1: %+v", bs)
	}
	if j2.NextSeq() != 3 {
		t.Fatalf("next seq = %d", j2.NextSeq())
	}
	if up, down := totals(bs); up != 20 || down != 40 {
		t.Fatalf("usage lost on renumbering: %d/%d", up, down)
	}
}

func TestDataIsEncryptedAndWrongKeyCannotReadIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	j := open(t, path, 0)
	_, _ = j.Append(entries("alice@secret-domain.example", 1, 1), 1, 2)
	_ = j.Close()
	raw, _ := os.ReadFile(path)
	if containsBytes(raw, []byte("alice@secret-domain.example")) {
		t.Fatal("client name found in plaintext on disk")
	}
	j2, err := Open(path, sealer(t, "wrong"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if bs, _ := j2.Batches(); len(bs) != 0 {
		t.Fatalf("rows readable with the wrong key: %+v", bs)
	}
}

func containsBytes(hay, needle []byte) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if string(hay[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

func TestPutIsIdempotentForTheSameSequence(t *testing.T) {
	j := open(t, filepath.Join(t.TempDir(), "j.db"), 0)
	defer j.Close()
	b := Batch{From: 5, To: 5, Start: 1, End: 2, Entries: entries("u", 7, 8)}
	for i := 0; i < 3; i++ {
		if err := j.Put(b); err != nil {
			t.Fatal(err)
		}
	}
	bs, _ := j.Batches()
	if len(bs) != 1 {
		t.Fatalf("duplicate rows: %+v", bs)
	}
	if j.NextSeq() != 6 {
		t.Fatalf("next seq = %d", j.NextSeq())
	}
}

func TestCompactionKeepsTotalsRangesAndShrinksTheJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	// A tiny limit forces compaction after a handful of records.
	j := open(t, path, 4096)
	defer j.Close()
	var wantUp, wantDown uint64
	const n = 400
	for i := 0; i < n; i++ {
		var es []*pb.TrafficEntry
		for u := 0; u < 5; u++ {
			es = append(es, &pb.TrafficEntry{Name: fmt.Sprintf("user%d", u), Up: uint64(i + 1), Down: 2})
			wantUp += uint64(i + 1)
			wantDown += 2
		}
		// 1 800 s apart: two records per hour, so hour merging has something to do.
		if _, err := j.Append(es, int64(i)*1800, int64(i)*1800+5); err != nil {
			t.Fatal(err)
		}
	}
	bs, err := j.Batches()
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) >= n {
		t.Fatalf("no compaction happened: %d records", len(bs))
	}
	if up, down := totals(bs); up != wantUp || down != wantDown {
		t.Fatalf("compaction changed the totals: %d/%d, want %d/%d", up, down, wantUp, wantDown)
	}
	next := uint64(1)
	for _, b := range bs {
		if b.From != next || b.To < b.From {
			t.Fatalf("ranges are not contiguous at %d: %+v", next, b)
		}
		next = b.To + 1
	}
	if next != n+1 {
		t.Fatalf("sequence coverage ends at %d, want %d", next-1, n)
	}
	if j.Size() > 4096 {
		t.Fatalf("journal is still %d bytes", j.Size())
	}
}

func TestMergeCoversBothRangesAndSpans(t *testing.T) {
	a := Batch{From: 1, To: 1, Start: 100, End: 110, Entries: []*pb.TrafficEntry{{Name: "u", Up: 1}, {Inbound: true, Name: "in", Up: 5}}}
	b := Batch{From: 2, To: 4, Start: 120, End: 150, Entries: []*pb.TrafficEntry{{Name: "u", Up: 10, Down: 3}}}
	m := Merge(a, b)
	if m.From != 1 || m.To != 4 || m.Start != 100 || m.End != 150 || len(m.Entries) != 2 {
		t.Fatalf("merged = %+v", m)
	}
}

func TestPerDestinationUsageSurvivesStorageAndCompactionPerHour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	j := open(t, path, 3000)
	defer j.Close()
	var wantDown uint64
	for i := 0; i < 300; i++ {
		hour := int64(i%3) * 3600
		conns := []*pb.ConnEntry{
			{Email: "a@x", InboundTag: "in", Dest: "example.com", Network: "tcp", HourUnix: hour, Up: 1, Down: 10, Conns: 1},
			{Email: "a@x", InboundTag: "in", Dest: "1.2.3.4", Network: "udp", HourUnix: hour, Up: 1, Down: 5, Conns: 1},
		}
		wantDown += 15
		seq := j.AllocSeq()
		if err := j.Put(Batch{From: seq, To: seq, Start: hour, End: hour + 5, Conns: conns}); err != nil {
			t.Fatal(err)
		}
	}
	bs, err := j.Batches()
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) >= 300 {
		t.Fatalf("no compaction: %d", len(bs))
	}
	var down uint64
	hours := map[int64]bool{}
	for _, b := range bs {
		for _, c := range b.Conns {
			down += c.Down
			hours[c.HourUnix] = true
		}
	}
	if down != wantDown {
		t.Fatalf("per-destination usage changed: %d != %d", down, wantDown)
	}
	if len(hours) != 3 {
		t.Fatalf("hourly resolution lost: %v", hours)
	}
}
