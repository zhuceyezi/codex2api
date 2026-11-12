// Package liveupdates broadcasts invalidations after data becomes readable.
// It carries no account data or credentials and never blocks request handling.
package liveupdates

import (
	"sync"
	"sync/atomic"
	"time"
)

var usageRevision atomic.Uint64
var accountRevision atomic.Uint64

func init()                   { usageRevision.Store(uint64(time.Now().UnixNano())) }
func UsageRevision() uint64   { return usageRevision.Load() }
func AccountRevision() uint64 { return accountRevision.Load() }

type Topics uint32

const (
	Usage Topics = 1 << iota
	Accounts
	Keys
	Proxies
	Settings
	Runtime
	Requests
	All = Usage | Accounts | Keys | Proxies | Settings | Runtime | Requests
)

func (t Topics) Names() []string {
	names := []string{}
	for _, item := range []struct {
		flag Topics
		name string
	}{
		{Usage, "usage"}, {Accounts, "accounts"}, {Keys, "keys"}, {Proxies, "proxies"}, {Settings, "settings"}, {Runtime, "runtime"}, {Requests, "requests"},
	} {
		if t&item.flag != 0 {
			names = append(names, item.name)
		}
	}
	return names
}

type Subscription struct {
	keyID   int64
	pending Topics
}

var bus = struct {
	sync.Mutex
	subscribers map[*Subscription]struct{}
}{subscribers: make(map[*Subscription]struct{})}

// A zero keyID is an admin subscription. Public subscriptions only receive
// changes to their own key, including no notifications of other users' traffic.
func Subscribe(keyID int64) (*Subscription, func()) {
	s := &Subscription{keyID: keyID}
	bus.Lock()
	bus.subscribers[s] = struct{}{}
	bus.Unlock()
	return s, func() { bus.Lock(); delete(bus.subscribers, s); bus.Unlock() }
}
func Publish(topics Topics, keyID int64) {
	if topics&Usage != 0 {
		usageRevision.Add(1)
	}
	if topics&Accounts != 0 {
		accountRevision.Add(1)
	}
	bus.Lock()
	defer bus.Unlock()
	for s := range bus.subscribers {
		if s.keyID == 0 || (keyID > 0 && s.keyID == keyID) {
			s.pending |= topics
		}
	}
}
func (s *Subscription) Take() Topics {
	bus.Lock()
	defer bus.Unlock()
	pending := s.pending
	s.pending = 0
	return pending
}
