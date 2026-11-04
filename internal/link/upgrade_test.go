package link

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

type fakeUpdater struct {
	mu        sync.Mutex
	err       error
	installed []string
	confirmed atomic.Int32
	block     chan struct{}
}

func (f *fakeUpdater) Install(_ context.Context, v string) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append(f.installed, v)
	return f.err
}

func (f *fakeUpdater) Confirm() { f.confirmed.Add(1) }

func (f *fakePanel) upgradeResult() *pb.UpgradeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.upgradeResults) == 0 {
		return nil
	}
	return f.upgradeResults[0]
}

func TestNodeInstallsAnUpdateAndRestartsOnRequest(t *testing.T) {
	h := newHarness(t)
	fp := &fakePanel{upgrade: &pb.UpgradeRequest{Version: "v1.2.3"}}
	h.servePanel(fp)
	upd := &fakeUpdater{}
	var restarted atomic.Int32
	n := h.startNode(Options{Updater: upd, OnUpgraded: func() { restarted.Add(1) }})
	defer n.stop()
	waitFor(t, "the update result", func() bool { return fp.upgradeResult() != nil })
	if r := fp.upgradeResult(); !r.Ok || r.Version != "v1.2.3" || r.Error != "" {
		t.Fatalf("result = %+v", r)
	}
	waitFor(t, "the restart", func() bool { return restarted.Load() == 1 })
	upd.mu.Lock()
	defer upd.mu.Unlock()
	if len(upd.installed) != 1 || upd.installed[0] != "v1.2.3" {
		t.Fatalf("installed %v", upd.installed)
	}
	if upd.confirmed.Load() == 0 {
		t.Fatal("reaching the panel must confirm the running binary")
	}
}

func TestFailedUpdateIsReportedAndDoesNotRestart(t *testing.T) {
	h := newHarness(t)
	fp := &fakePanel{upgrade: &pb.UpgradeRequest{Version: "v1.2.3"}}
	h.servePanel(fp)
	var restarted atomic.Int32
	n := h.startNode(Options{Updater: &fakeUpdater{err: errors.New("signature check failed")}, OnUpgraded: func() { restarted.Add(1) }})
	defer n.stop()
	waitFor(t, "the update result", func() bool { return fp.upgradeResult() != nil })
	if r := fp.upgradeResult(); r.Ok || r.Error != "signature check failed" {
		t.Fatalf("result = %+v", r)
	}
	if restarted.Load() != 0 {
		t.Fatal("a failed update must not restart the node")
	}
}

func TestNodeWithoutUpdaterAnswersThatItCannotUpdate(t *testing.T) {
	h := newHarness(t)
	fp := &fakePanel{upgrade: &pb.UpgradeRequest{Version: "v1.2.3"}}
	h.servePanel(fp)
	n := h.startNode(Options{})
	defer n.stop()
	waitFor(t, "the update result", func() bool { return fp.upgradeResult() != nil })
	if r := fp.upgradeResult(); r.Ok || r.Error == "" {
		t.Fatalf("result = %+v", r)
	}
}
