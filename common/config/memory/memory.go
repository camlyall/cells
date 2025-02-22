package memory

import (
	"context"
	"errors"
	"fmt"
	"github.com/pydio/cells/v4/common/utils/std"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pydio/cells/v4/common/config"
	"github.com/pydio/cells/v4/common/utils/configx"
	"github.com/r3labs/diff/v3"
)

var (
	scheme           = "mem"
	errClosedChannel = errors.New("channel is closed")
)

type URLOpener struct{}

const timeout = 500 * time.Millisecond

func init() {
	o := &URLOpener{}
	config.DefaultURLMux().Register(scheme, o)
}

func (o *URLOpener) OpenURL(ctx context.Context, u *url.URL) (config.Store, error) {

	var opts []configx.Option

	encode := u.Query().Get("encode")
	switch encode {
	case "string":
		opts = append(opts, configx.WithString())
	case "yaml":
		opts = append(opts, configx.WithYAML())
	case "json":
		opts = append(opts, configx.WithJSON())
	default:
		opts = append(opts, configx.WithJSON())
	}

	store := New(opts...)

	return store, nil
}

type memory struct {
	v    configx.Values
	snap configx.Values

	opts            []configx.Option
	receiversLocker *sync.RWMutex
	receivers       []*receiver

	reset chan bool
	timer *time.Timer

	internalLocker *sync.RWMutex
	externalLocker *sync.RWMutex
}

func New(opt ...configx.Option) config.Store {
	opts := configx.Options{}
	for _, o := range opt {
		o(&opts)
	}

	internalLocker := opts.RWMutex
	if internalLocker == nil {
		internalLocker = &sync.RWMutex{}
		opt = append(opt, configx.WithLock(internalLocker))
	}

	m := &memory{
		v:               configx.New(opt...),
		opts:            opt,
		internalLocker:  internalLocker,
		externalLocker:  &sync.RWMutex{},
		receiversLocker: &sync.RWMutex{},
		reset:           make(chan bool),
		timer:           time.NewTimer(timeout),
		snap:            configx.New(opt...),
	}

	go m.flush()

	return m
}

func (m *memory) flush() {
	for {
		select {
		case <-m.reset:
			m.timer.Stop()
			m.timer = time.NewTimer(timeout)
		case <-m.timer.C:
			m.internalLocker.RLock()
			clone := std.DeepClone(m.v.Interface())
			snapClone := std.DeepClone(m.snap.Interface())
			m.internalLocker.RUnlock()

			patch, err := diff.Diff(snapClone, clone)
			if err != nil {
				continue
			}

			snap := configx.New(m.opts...)
			if err := snap.Set(clone); err != nil {
				continue
			}

			m.snap = snap

			for _, op := range patch {
				var updated []*receiver

				m.receiversLocker.RLock()
				for _, r := range m.receivers {
					if err := r.call(op); err == nil {
						updated = append(updated, r)
					}
				}
				m.receiversLocker.RUnlock()

				m.receiversLocker.Lock()
				m.receivers = updated
				m.receiversLocker.Unlock()
			}

		}
	}
}

func (m *memory) update() {
	m.reset <- true
}

func (m *memory) Get() configx.Value {
	return m.v
}

func (m *memory) Set(data interface{}) error {
	if err := m.v.Set(data); err != nil {
		return err
	}

	m.update()

	return nil
}

func (m *memory) Val(path ...string) configx.Values {
	return &values{Values: m.v.Val(path...), m: m}
}

func (m *memory) Del() error {
	return fmt.Errorf("not implemented")
}

func (m *memory) Close() error {
	return nil
}

func (m *memory) Done() <-chan struct{} {
	// Never returns
	return nil
}

func (m *memory) Save(string, string) error {
	// do nothing
	return nil
}

func (m *memory) Lock() {
	m.externalLocker.Lock()
}

func (m *memory) Unlock() {
	m.externalLocker.Unlock()
}

func (m *memory) Watch(opts ...configx.WatchOption) (configx.Receiver, error) {
	o := &configx.WatchOptions{}
	for _, opt := range opts {
		opt(o)
	}

	regPath, err := regexp.Compile("^" + strings.Join(o.Path, "/"))
	if err != nil {
		return nil, err
	}

	r := &receiver{
		closed:      false,
		ch:          make(chan diff.Change),
		regPath:     regPath,
		level:       len(o.Path),
		m:           m,
		timer:       time.NewTimer(timeout),
		changesOnly: o.ChangesOnly,
	}

	m.receiversLocker.Lock()
	m.receivers = append(m.receivers, r)
	m.receiversLocker.Unlock()

	return r, nil
}

type receiver struct {
	closed bool
	ch     chan diff.Change

	regPath     *regexp.Regexp
	level       int
	changesOnly bool

	timer *time.Timer

	m *memory
}

func (r *receiver) call(op diff.Change) error {
	if r.closed {
		return errClosedChannel
	}

	if r.level == 0 {
		r.ch <- op
	}

	if r.level > len(op.Path) {
		return nil
	}

	if r.regPath.MatchString(strings.Join(op.Path, "/")) {
		r.ch <- op
	}
	return nil
}

func (r *receiver) Next() (interface{}, error) {
	changes := []diff.Change{}

	for {
		select {
		case op := <-r.ch:
			if r.closed {
				return nil, errClosedChannel
			}

			changes = append(changes, op)

			r.timer.Stop()
			r.timer = time.NewTimer(timeout)

		case <-r.timer.C:
			c := configx.New()
			if r.changesOnly {
				for _, op := range changes {
					switch op.Type {
					case diff.CREATE:
						if len(op.Path) > r.level {
							if err := c.Val(diff.UPDATE).Val(op.Path...).Set(op.To); err != nil {
								return nil, err
							}
						} else {
							if err := c.Val(diff.CREATE).Val(op.Path...).Set(op.To); err != nil {
								return nil, err
							}
						}
					case diff.DELETE:
						if len(op.Path) > r.level {
							if err := c.Val(diff.UPDATE).Val(op.Path...).Set(nil); err != nil {
								return nil, err
							}
						} else {
							if err := c.Val(diff.DELETE).Val(op.Path...).Set(op.From); err != nil {
								return nil, err
							}
						}
					case diff.UPDATE:
						if err := c.Val(diff.UPDATE).Val(op.Path...).Set(op.To); err != nil {
							return nil, err
						}
					}
				}

				return c, nil
			}

			for _, op := range changes {
				if err := c.Val(op.Path...).Set(op.To); err != nil {
					return nil, err
				}
			}

			return c, nil
		}
	}

	return r.Next()
}

func (r *receiver) Stop() {
	r.closed = true
	close(r.ch)
}

type values struct {
	configx.Values

	m *memory
}

func (v *values) Set(data interface{}) error {
	if err := v.Values.Set(data); err != nil {
		return err
	}

	v.m.update()

	return nil
}

func (v *values) Del() error {
	if err := v.Values.Del(); err != nil {
		return err
	}

	v.m.update()

	return nil
}

func (v *values) Val(path ...string) configx.Values {
	return &values{Values: v.Values.Val(path...), m: v.m}
}
