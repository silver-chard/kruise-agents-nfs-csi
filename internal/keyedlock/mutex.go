// Package keyedlock provides cancellable exclusion for active resource keys.
package keyedlock

import (
	"context"
	"sync"
)

// Mutex serializes callers with the same key. Its zero value is ready to use.
// Entries are removed when no holder or waiter references them.
type Mutex[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*entry
}

type entry struct {
	token chan struct{}
	refs  int
}

// Lock waits for key or context cancellation. The returned unlock function
// must be called after a successful acquisition; further calls have no effect.
func (m *Mutex[K]) Lock(ctx context.Context, key K) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.entries == nil {
		m.entries = make(map[K]*entry)
	}
	e := m.entries[key]
	if e == nil {
		e = &entry{token: make(chan struct{}, 1)}
		e.token <- struct{}{}
		m.entries[key] = e
	}
	e.refs++
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		m.releaseRef(key, e)
		return nil, ctx.Err()
	case <-e.token:
		// Cancellation and the token can become ready together.
		if err := ctx.Err(); err != nil {
			e.token <- struct{}{}
			m.releaseRef(key, e)
			return nil, err
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				e.token <- struct{}{}
				m.releaseRef(key, e)
			})
		}, nil
	}
}

func (m *Mutex[K]) releaseRef(key K, e *entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(m.entries, key)
	}
}
