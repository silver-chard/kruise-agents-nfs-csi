package keyedlock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitingCallerCancelsWithoutBlockingAnotherKey(t *testing.T) {
	var locks Mutex[string]
	unlock, err := locks.Lock(context.Background(), "stalled-pv")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		unlock, err := locks.Lock(ctx, "stalled-pv")
		if err == nil {
			unlock()
		}
		done <- err
	}()
	other, err := locks.Lock(context.Background(), "healthy-pv")
	if err != nil {
		t.Fatal(err)
	}
	other()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Lock error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter stayed blocked")
	}
	unlock()
	if len(locks.entries) != 0 {
		t.Fatalf("retained %d inactive keys", len(locks.entries))
	}
}

func TestSameKeyExcludesCallersAndReleasesEntries(t *testing.T) {
	var locks Mutex[int]
	var active, overlap atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 100 {
				unlock, err := locks.Lock(context.Background(), 1)
				if err != nil {
					t.Error(err)
					return
				}
				if active.Add(1) != 1 {
					overlap.Add(1)
				}
				active.Add(-1)
				unlock()
			}
		})
	}
	wg.Wait()
	if overlap.Load() != 0 {
		t.Fatalf("same key had %d overlapping holders", overlap.Load())
	}
	for key := range 1000 {
		unlock, err := locks.Lock(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	}
	if len(locks.entries) != 0 {
		t.Fatalf("retained %d inactive keys", len(locks.entries))
	}
}

func TestCancelledContextDoesNotAcquireFreeKey(t *testing.T) {
	var locks Mutex[string]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if unlock, err := locks.Lock(ctx, "pv"); unlock != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Lock = (%v, %v), want cancellation", unlock != nil, err)
	}
	if len(locks.entries) != 0 {
		t.Fatal("cancelled context registered a key")
	}
}
