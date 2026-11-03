package limits

import (
	"slices"
	"testing"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

func snap(cl ...*pb.ClientLimit) *pb.LimitsSnapshot { return &pb.LimitsSnapshot{Clients: cl} }

func TestQuotaAndExpiryAreEnforcedOnce(t *testing.T) {
	e := New()
	e.SetSnapshot(snap(
		&pb.ClientLimit{Email: "q", QuotaBytes: 100, UsedBytes: 90},
		&pb.ClientLimit{Email: "x", ExpiryUnix: 1000},
		&pb.ClientLimit{Email: "free"},
	))
	if v := e.Violations(500); len(v) != 0 {
		t.Fatalf("nothing is over yet: %v", v)
	}
	e.AddUsage([]*pb.TrafficEntry{{Name: "q", Up: 4, Down: 5}, {Name: "free", Up: 1 << 40}, {Inbound: true, Name: "q", Up: 1 << 40}})
	if v := e.Violations(500); len(v) != 0 {
		t.Fatalf("99 of 100 used: %v", v)
	}
	e.AddUsage([]*pb.TrafficEntry{{Name: "q", Up: 1}})
	got := e.Violations(1000)
	slices.Sort(got)
	if !slices.Equal(got, []string{"q", "x"}) {
		t.Fatalf("violations = %v", got)
	}
	if v := e.Violations(2000); len(v) != 0 {
		t.Fatalf("a dropped client must be reported once: %v", v)
	}
	e.Reset()
	if v := e.Violations(2000); len(v) != 2 {
		t.Fatalf("after Reset both are reported again: %v", v)
	}
}

func TestNewSnapshotReplacesUsage(t *testing.T) {
	e := New()
	e.SetSnapshot(snap(&pb.ClientLimit{Email: "q", QuotaBytes: 100, UsedBytes: 50}))
	e.AddUsage([]*pb.TrafficEntry{{Name: "q", Up: 40}})
	e.SetSnapshot(snap(&pb.ClientLimit{Email: "q", QuotaBytes: 100, UsedBytes: 10}))
	e.AddUsage([]*pb.TrafficEntry{{Name: "q", Up: 80}})
	if v := e.Violations(0); len(v) != 0 {
		t.Fatalf("the panel's count must win over the node's: %v", v)
	}
}
