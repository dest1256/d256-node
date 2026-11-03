// Package xrayrun embeds xray-core as a library and applies the panel's desired state to it.
package xrayrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all" // register every protocol, transport and app
	"google.golang.org/protobuf/proto"

	pb "github.com/dest1256/d256-panel/proto/nodelink/v1"
)

// State values reported to the panel.
const (
	StateNotConfigured = "not_configured"
	StateRunning       = "running"
	StateStopped       = "stopped"
	StateError         = "error"
)

const hotOpTimeout = 10 * time.Second

type usage struct{ up, down uint64 }

type key struct {
	inbound bool
	name    string
}

// Runner owns the embedded Xray instance.
type Runner struct {
	log *slog.Logger

	mu      sync.Mutex
	inst    *core.Instance
	cfgJSON []byte
	version uint64
	state   string
	errText string
	// carry holds usage read from instances that were closed since the last Collect.
	carry map[key]usage
}

// New returns a runner with nothing running.
func New(log *slog.Logger) *Runner {
	return &Runner{log: log, state: StateNotConfigured, carry: map[key]usage{}}
}

// Version is the core version this node embeds.
func Version() string { return core.Version() }

// Status reports the lifecycle state, the last error and the applied desired-state version.
func (r *Runner) Status() (state, errText string, version uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.errText, r.version
}

func build(cfgJSON []byte) (*core.Instance, error) {
	cfg, err := serial.LoadJSONConfig(bytes.NewReader(cfgJSON))
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	inst, err := core.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("create core: %w", err)
	}
	return inst, nil
}

// Apply moves the node to ds. It prefers the panel's restart-free plan when the node runs exactly
// the version the plan is based on, and otherwise restarts with the full config. A config that
// cannot be built or started leaves (or restores) the previous one running.
func (r *Runner) Apply(ds *pb.DesiredState) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.inst != nil && bytes.Equal(ds.XrayConfigJson, r.cfgJSON) {
		r.version = ds.Version
		return nil
	}
	if r.inst != nil && ds.HotOpsValid && ds.BaseVersion == r.version {
		if err := r.applyHot(ds.HotOps); err == nil {
			r.cfgJSON, r.version = bytes.Clone(ds.XrayConfigJson), ds.Version
			return nil
		} else {
			r.log.Warn("hot update failed, restarting xray", "err", err)
		}
	}
	return r.restartLocked(ds.Version, ds.XrayConfigJson)
}

func (r *Runner) restartLocked(version uint64, cfgJSON []byte) error {
	next, err := build(cfgJSON)
	if err != nil {
		r.setError(err)
		return err
	}
	prevJSON := r.cfgJSON
	r.stopLocked()
	if err := next.Start(); err != nil {
		_ = next.Close()
		err = fmt.Errorf("start core: %w", err)
		r.log.Error("new config failed to start, restoring the previous one", "err", err)
		r.restoreLocked(prevJSON)
		r.errText = err.Error()
		if r.inst != nil {
			r.state = StateRunning
		} else {
			r.state = StateError
		}
		return err
	}
	r.inst, r.cfgJSON, r.version = next, bytes.Clone(cfgJSON), version
	r.state, r.errText = StateRunning, ""
	r.log.Info("xray started", "version", version, "core", core.Version())
	return nil
}

func (r *Runner) restoreLocked(prevJSON []byte) {
	if prevJSON == nil {
		return
	}
	inst, err := build(prevJSON)
	if err == nil {
		if err = inst.Start(); err != nil {
			_ = inst.Close()
		}
	}
	if err != nil {
		r.log.Error("could not restore the previous config", "err", err)
		return
	}
	r.inst, r.cfgJSON = inst, prevJSON
}

func (r *Runner) setError(err error) {
	r.errText = err.Error()
	if r.inst == nil {
		r.state = StateError
	}
}

// stopLocked closes the running instance after harvesting its counters.
func (r *Runner) stopLocked() {
	if r.inst == nil {
		return
	}
	r.harvestLocked(r.carry)
	if err := r.inst.Close(); err != nil {
		r.log.Warn("closing xray", "err", err)
	}
	r.inst = nil
	r.state = StateStopped
}

// Stop shuts the core down.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

func (r *Runner) applyHot(ops []*pb.HotOp) error {
	mgr, _ := r.inst.GetFeature(inbound.ManagerType()).(inbound.Manager)
	if mgr == nil {
		return errors.New("inbound manager unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), hotOpTimeout)
	defer cancel()
	for _, op := range ops {
		var err error
		switch o := op.Op.(type) {
		case *pb.HotOp_RemoveUser:
			err = removeUser(ctx, mgr, o.RemoveUser)
		case *pb.HotOp_AddUser:
			err = addUser(ctx, mgr, o.AddUser)
		case *pb.HotOp_RemoveInbound:
			err = mgr.RemoveHandler(ctx, o.RemoveInbound.InboundTag)
			if err != nil && isMissing(err) {
				err = nil
			}
		case *pb.HotOp_AddInbound:
			err = r.addInbound(o.AddInbound.InboundJson)
		default:
			err = errors.New("unknown hot operation")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func isMissing(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not found") || strings.Contains(m, "not enough information")
}

func removeUser(ctx context.Context, mgr inbound.Manager, o *pb.RemoveUser) error {
	h, err := mgr.GetHandler(ctx, o.InboundTag)
	if err != nil {
		if isMissing(err) {
			return nil
		}
		return err
	}
	err = (&command.RemoveUserOperation{Email: o.Email}).ApplyInbound(ctx, h)
	if err != nil && (o.BestEffort || isMissing(err)) {
		return nil
	}
	return err
}

func addUser(ctx context.Context, mgr inbound.Manager, o *pb.AddUser) error {
	var u protocol.User
	if err := proto.Unmarshal(o.User, &u); err != nil {
		return fmt.Errorf("decode user: %w", err)
	}
	h, err := mgr.GetHandler(ctx, o.InboundTag)
	if err != nil {
		return err
	}
	op := &command.AddUserOperation{User: &u}
	err = op.ApplyInbound(ctx, h)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		_ = (&command.RemoveUserOperation{Email: u.Email}).ApplyInbound(ctx, h)
		err = op.ApplyInbound(ctx, h)
	}
	return err
}

func (r *Runner) addInbound(raw []byte) error {
	var d conf.InboundDetourConfig
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("decode inbound: %w", err)
	}
	cfg, err := d.Build()
	if err != nil {
		return fmt.Errorf("build inbound: %w", err)
	}
	return core.AddInboundHandler(r.inst, cfg)
}

// Collect returns the usage accumulated since the previous call and resets the counters.
func (r *Runner) Collect() []*pb.TrafficEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	sum := r.carry
	r.carry = map[key]usage{}
	r.harvestLocked(sum)
	out := make([]*pb.TrafficEntry, 0, len(sum))
	for k, u := range sum {
		if u.up == 0 && u.down == 0 {
			continue
		}
		out = append(out, &pb.TrafficEntry{Inbound: k.inbound, Name: k.name, Up: u.up, Down: u.down})
	}
	return out
}

// harvestLocked reads and zeroes the core's traffic counters into into.
func (r *Runner) harvestLocked(into map[key]usage) {
	if r.inst == nil {
		return
	}
	sm, _ := r.inst.GetFeature(stats.ManagerType()).(stats.Manager)
	if sm == nil {
		return
	}
	sm.VisitCounters(func(name string, c stats.Counter) bool {
		// Names look like user>>>email>>>traffic>>>uplink or inbound>>>tag>>>traffic>>>downlink.
		parts := strings.Split(name, ">>>")
		if len(parts) != 4 || parts[2] != "traffic" || (parts[0] != "user" && parts[0] != "inbound") {
			return true
		}
		v := c.Set(0)
		if v <= 0 {
			return true
		}
		k := key{inbound: parts[0] == "inbound", name: parts[1]}
		u := into[k]
		switch parts[3] {
		case "uplink":
			u.up += uint64(v)
		case "downlink":
			u.down += uint64(v)
		}
		into[k] = u
		return true
	})
}

// OnlineUsers counts the users that currently have at least one connection.
func (r *Runner) OnlineUsers() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inst == nil {
		return 0
	}
	sm, _ := r.inst.GetFeature(stats.ManagerType()).(stats.Manager)
	if sm == nil {
		return 0
	}
	n := 0
	sm.VisitOnlineMaps(func(name string, om stats.OnlineMap) bool {
		if strings.HasPrefix(name, "user>>>") && om.Count() > 0 {
			n++
		}
		return true
	})
	return n
}

// OnlineEmails lists up to limit users that currently have a connection.
func (r *Runner) OnlineEmails(limit int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inst == nil {
		return nil
	}
	sm, _ := r.inst.GetFeature(stats.ManagerType()).(stats.Manager)
	if sm == nil {
		return nil
	}
	var out []string
	sm.VisitOnlineMaps(func(name string, om stats.OnlineMap) bool {
		if email, ok := strings.CutPrefix(name, "user>>>"); ok && om.Count() > 0 {
			email = strings.TrimSuffix(email, ">>>online")
			out = append(out, email)
		}
		return len(out) < limit
	})
	return out
}

// Config returns the configuration the core currently runs (nil when stopped).
func (r *Runner) Config() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inst == nil {
		return nil
	}
	return bytes.Clone(r.cfgJSON)
}
