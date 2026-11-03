package connstats

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// carrierValue marks the StringValue app entry that stands in for the default dispatcher in the core
// config. Any registered protobuf message can carry the request; this one is unused by xray-core itself.
const carrierValue = "d256-connstats-dispatcher"

// ErrDeviceLimit is returned to the inbound when a client is over its device limit.
var ErrDeviceLimit = errors.New("device limit reached")

var (
	sharedMu sync.RWMutex
	shared   *Store
)

func init() {
	common.Must(common.RegisterConfig((*wrapperspb.StringValue)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		if v, _ := config.(*wrapperspb.StringValue); v == nil || v.Value != carrierValue {
			return nil, errors.New("connstats: unknown configuration")
		}
		// The default dispatcher is built exactly as xray-core would (fake DNS, sniffing, stats) and
		// then wrapped, so routing behaviour is untouched.
		obj, err := common.CreateObject(ctx, &dispatcher.Config{})
		if err != nil {
			return nil, err
		}
		inner, ok := obj.(routing.Dispatcher)
		if !ok {
			return nil, errors.New("connstats: the default dispatcher has an unexpected type")
		}
		sharedMu.RLock()
		st := shared
		sharedMu.RUnlock()
		if st == nil {
			return nil, errors.New("connstats: no store installed")
		}
		return &Tracker{inner: inner, store: st}, nil
	}))
}

// Install makes the core created from cfg use the tracking dispatcher, recording into store.
func Install(cfg *core.Config, store *Store) {
	sharedMu.Lock()
	shared = store
	sharedMu.Unlock()
	defaultType := serial.ToTypedMessage(&dispatcher.Config{}).Type
	apps := cfg.App[:0:0]
	for _, a := range cfg.App {
		if a.Type != defaultType {
			apps = append(apps, a)
		}
	}
	cfg.App = append(apps, serial.ToTypedMessage(wrapperspb.String(carrierValue)))
}

// Tracker wraps the default dispatcher.
type Tracker struct {
	inner routing.Dispatcher
	store *Store
}

// Type implements features.Feature.
func (*Tracker) Type() interface{} { return routing.DispatcherType() }

// Start implements features.Feature.
func (t *Tracker) Start() error { return t.inner.Start() }

// Close implements features.Feature.
func (t *Tracker) Close() error { return t.inner.Close() }

func destOf(ctx context.Context, fallback net.Destination) func() (string, uint32) {
	return func() (string, uint32) {
		d := fallback
		if obs := session.OutboundsFromContext(ctx); len(obs) > 0 {
			ob := obs[len(obs)-1]
			switch {
			case ob.RouteTarget.IsValid():
				d = ob.RouteTarget
			case ob.Target.IsValid():
				d = ob.Target
			}
		}
		return cleanDest(d.Address.String()), uint32(d.Port)
	}
}

// identify returns the client the connection belongs to; connections without a user are not tracked.
func identify(ctx context.Context) (email, inbound, srcIP string, ok bool) {
	in := session.InboundFromContext(ctx)
	if in == nil || in.User == nil || in.User.Email == "" {
		return "", "", "", false
	}
	if in.Source.Address != nil {
		srcIP = in.Source.Address.String()
	}
	return in.User.Email, in.Tag, srcIP, true
}

// Dispatch implements routing.Dispatcher. The returned link belongs to the inbound side: what the
// inbound writes is the client's upload, what it reads is the download.
func (t *Tracker) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	email, inbound, srcIP, tracked := identify(ctx)
	if !tracked {
		return t.inner.Dispatch(ctx, dest)
	}
	if !t.store.admit(email, srcIP) {
		return nil, ErrDeviceLimit
	}
	link, err := t.inner.Dispatch(ctx, dest)
	if err != nil || link == nil {
		return link, err
	}
	c := t.store.open(email, inbound, srcIP, dest.Network.SystemString(), destOf(ctx, dest))
	link.Writer = &countWriter{Writer: link.Writer, n: &c.up}
	link.Reader = &countReader{Reader: link.Reader, n: &c.down}
	context.AfterFunc(ctx, func() { t.store.close(c) })
	return link, nil
}

// DispatchLink implements routing.Dispatcher. Here the link is the inbound's own: reading it yields the
// client's upload, writing it sends the download.
func (t *Tracker) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	email, inbound, srcIP, tracked := identify(ctx)
	if !tracked {
		return t.inner.DispatchLink(ctx, dest, link)
	}
	if !t.store.admit(email, srcIP) {
		return ErrDeviceLimit
	}
	c := t.store.open(email, inbound, srcIP, dest.Network.SystemString(), destOf(ctx, dest))
	link.Reader = &countReader{Reader: link.Reader, n: &c.up}
	link.Writer = &countWriter{Writer: link.Writer, n: &c.down}
	context.AfterFunc(ctx, func() { t.store.close(c) })
	return t.inner.DispatchLink(ctx, dest, link)
}

// countReader counts bytes read through it. It forwards every optional interface the core probes for.
type countReader struct {
	buf.Reader
	n *atomic.Uint64
}

func (r *countReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.Reader.ReadMultiBuffer()
	if l := mb.Len(); l > 0 {
		r.n.Add(uint64(l))
	}
	return mb, err
}

func (r *countReader) ReadMultiBufferTimeout(d time.Duration) (buf.MultiBuffer, error) {
	tr, ok := r.Reader.(buf.TimeoutReader)
	if !ok {
		return nil, errors.New("connstats: reader has no timeout support")
	}
	mb, err := tr.ReadMultiBufferTimeout(d)
	if l := mb.Len(); l > 0 {
		r.n.Add(uint64(l))
	}
	return mb, err
}

func (r *countReader) Interrupt() { _ = common.Interrupt(r.Reader) }

func (r *countReader) Close() error { return common.Close(r.Reader) }

// countWriter counts bytes written through it.
type countWriter struct {
	buf.Writer
	n *atomic.Uint64
}

func (w *countWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	if l := mb.Len(); l > 0 {
		w.n.Add(uint64(l))
	}
	return w.Writer.WriteMultiBuffer(mb)
}

func (w *countWriter) Interrupt() { _ = common.Interrupt(w.Writer) }

func (w *countWriter) Close() error { return common.Close(w.Writer) }
