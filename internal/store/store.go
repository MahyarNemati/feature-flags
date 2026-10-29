// Package store persists flags and their change history.
package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/MahyarNemati/feature-flags/internal/flag"
)

// Errors returned by a Store.
var (
	ErrNotFound = errors.New("store: flag not found")
	ErrConflict = errors.New("store: flag already exists")
	// ErrStaleVersion is returned when an update carries an out-of-date
	// version, which means someone else changed the flag first.
	ErrStaleVersion = errors.New("store: stale version")
)

// Change is one audit record. Keeping the full before and after snapshot,
// rather than a diff, means history survives schema changes and can answer
// "what exactly was being served at 14:00 last Tuesday".
type Change struct {
	ID      int64     `json:"id"`
	FlagKey string    `json:"flag_key"`
	At      time.Time `json:"at"`
	// Actor is whoever made the change, from the API token's identity.
	Actor string `json:"actor"`
	// Action is create, update, delete, enable, disable or archive.
	Action string `json:"action"`
	// Comment is an optional note supplied with the change.
	Comment string     `json:"comment,omitempty"`
	Before  *flag.Flag `json:"before,omitempty"`
	After   *flag.Flag `json:"after,omitempty"`
}

// Store persists flags.
type Store interface {
	Get(ctx context.Context, key string) (*flag.Flag, error)
	List(ctx context.Context, includeArchived bool) ([]*flag.Flag, error)
	Create(ctx context.Context, f *flag.Flag, actor, comment string) (*flag.Flag, error)
	// Update replaces a flag. When expectVersion is non-zero it must match
	// the stored version, or ErrStaleVersion is returned.
	Update(ctx context.Context, f *flag.Flag, expectVersion int, actor, comment string) (*flag.Flag, error)
	Delete(ctx context.Context, key, actor, comment string) error
	History(ctx context.Context, key string, limit int) ([]Change, error)
	// Snapshot returns every active flag plus a version for the whole set,
	// which clients use to detect staleness cheaply.
	Snapshot(ctx context.Context) (map[string]*flag.Flag, int64, error)
	Ping(ctx context.Context) error
	Close() error
}

// Memory is an in-process Store. Flags live in memory only, so a restart
// loses them: suitable for development and tests, not production.
type Memory struct {
	mu      sync.RWMutex
	flags   map[string]*flag.Flag
	changes []Change
	nextID  int64
	// revision increments on every mutation and is the snapshot version.
	revision int64
	now      func() time.Time
	// subscribers are notified after each change, which is what lets the API
	// stream updates instead of polling.
	subs map[int]chan struct{}
	next int
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		flags:  map[string]*flag.Flag{},
		nextID: 1,
		now:    time.Now,
		subs:   map[int]chan struct{}{},
	}
}

// WithClock overrides the time source, for tests.
func (m *Memory) WithClock(now func() time.Time) *Memory {
	m.now = now
	return m
}

// Subscribe returns a channel signalled after every change, and a function to
// unsubscribe. The channel has capacity 1 and sends are dropped when full, so
// a slow subscriber cannot block a write — it just coalesces notifications,
// which is correct for "something changed, re-read the snapshot".
func (m *Memory) Subscribe() (<-chan struct{}, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.next
	m.next++
	ch := make(chan struct{}, 1)
	m.subs[id] = ch
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if c, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(c)
		}
	}
}

// notify signals subscribers. Caller holds m.mu.
func (m *Memory) notify() {
	for _, ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default: // already pending; coalesce
		}
	}
}

func (m *Memory) Get(_ context.Context, key string) (*flag.Flag, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.flags[key]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(f), nil
}

func (m *Memory) List(_ context.Context, includeArchived bool) ([]*flag.Flag, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*flag.Flag, 0, len(m.flags))
	for _, f := range m.flags {
		if f.Archived && !includeArchived {
			continue
		}
		out = append(out, clone(f))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *Memory) Create(_ context.Context, f *flag.Flag, actor, comment string) (*flag.Flag, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.flags[f.Key]; exists {
		return nil, ErrConflict
	}
	now := m.now()
	stored := clone(f)
	stored.Version = 1
	stored.CreatedAt, stored.UpdatedAt = now, now
	m.flags[f.Key] = stored

	m.record(Change{
		FlagKey: f.Key, At: now, Actor: actor, Action: "create",
		Comment: comment, After: clone(stored),
	})
	m.revision++
	m.notify()
	return clone(stored), nil
}

func (m *Memory) Update(_ context.Context, f *flag.Flag, expectVersion int, actor, comment string) (*flag.Flag, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.flags[f.Key]
	if !ok {
		return nil, ErrNotFound
	}
	// Optimistic concurrency. Without it, two people editing the same flag in
	// a dashboard silently overwrite each other, and the loser has no idea.
	if expectVersion != 0 && existing.Version != expectVersion {
		return nil, ErrStaleVersion
	}

	now := m.now()
	before := clone(existing)
	stored := clone(f)
	stored.Version = existing.Version + 1
	stored.CreatedAt = existing.CreatedAt
	stored.UpdatedAt = now
	m.flags[f.Key] = stored

	action := "update"
	switch {
	case !before.Enabled && stored.Enabled:
		action = "enable"
	case before.Enabled && !stored.Enabled:
		action = "disable"
	case !before.Archived && stored.Archived:
		action = "archive"
	}

	m.record(Change{
		FlagKey: f.Key, At: now, Actor: actor, Action: action,
		Comment: comment, Before: before, After: clone(stored),
	})
	m.revision++
	m.notify()
	return clone(stored), nil
}

func (m *Memory) Delete(_ context.Context, key, actor, comment string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.flags[key]
	if !ok {
		return ErrNotFound
	}
	before := clone(existing)
	delete(m.flags, key)

	m.record(Change{
		FlagKey: key, At: m.now(), Actor: actor, Action: "delete",
		Comment: comment, Before: before,
	})
	m.revision++
	m.notify()
	return nil
}

func (m *Memory) History(_ context.Context, key string, limit int) ([]Change, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []Change
	// Newest first: the question is almost always "what changed recently".
	for i := len(m.changes) - 1; i >= 0; i-- {
		c := m.changes[i]
		if key != "" && c.FlagKey != key {
			continue
		}
		out = append(out, c)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *Memory) Snapshot(_ context.Context) (map[string]*flag.Flag, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*flag.Flag, len(m.flags))
	for k, f := range m.flags {
		out[k] = clone(f)
	}
	return out, m.revision, nil
}

func (m *Memory) Ping(context.Context) error { return nil }

func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ch := range m.subs {
		delete(m.subs, id)
		close(ch)
	}
	return nil
}

// record appends a change. Caller holds m.mu.
func (m *Memory) record(c Change) {
	c.ID = m.nextID
	m.nextID++
	m.changes = append(m.changes, c)
}

// clone deep-copies the parts of a flag that callers could otherwise mutate.
// Returning a pointer into the store would let a caller change a live flag
// without going through Update, bypassing validation and the audit log.
func clone(f *flag.Flag) *flag.Flag {
	if f == nil {
		return nil
	}
	out := *f
	out.Variations = append([]flag.Variation(nil), f.Variations...)
	out.Tags = append([]string(nil), f.Tags...)

	out.Rules = make([]flag.Rule, len(f.Rules))
	for i, r := range f.Rules {
		cr := r
		cr.Conditions = make([]flag.Condition, len(r.Conditions))
		for j, c := range r.Conditions {
			cc := c
			cc.Values = append([]string(nil), c.Values...)
			cr.Conditions[j] = cc
		}
		if r.Rollout != nil {
			ro := *r.Rollout
			ro.Weights = make(map[string]int, len(r.Rollout.Weights))
			for k, v := range r.Rollout.Weights {
				ro.Weights[k] = v
			}
			cr.Rollout = &ro
		}
		out.Rules[i] = cr
	}
	return &out
}
