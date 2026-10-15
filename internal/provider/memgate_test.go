package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
)

// newMemGateSub is a subscriber whose memory reading is *usage against a soft
// limit of 100.
func newMemGateSub(usage *atomic.Uint64, maxWait time.Duration) *EventSubscriber {
	p := &StarknetProvider{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sub := p.NewSubscriber(nil, nil, nil)
	sub.memSoftLimit = 100
	sub.memUsage = usage.Load
	sub.memPoll = time.Millisecond
	sub.memMaxWait = maxWait
	return sub
}

// Above the soft limit a resubscribe waits without taking a slot, and proceeds
// once memory falls back under it.
func TestMemoryGateHoldsUntilBelowLimit(t *testing.T) {
	var usage atomic.Uint64
	usage.Store(150)
	sub := newMemGateSub(&usage, time.Minute)

	got := make(chan error, 1)
	go func() {
		release, err := sub.acquireResubscribe(context.Background())
		if err == nil {
			release()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("acquired above the soft limit (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	if n := sub.memWaiting.Load(); n != 1 {
		t.Fatalf("memWaiting = %d while held, want 1", n)
	}
	if n := len(sub.resubSem); n != 0 {
		t.Fatalf("%d slots taken while held by the memory gate, want 0", n)
	}

	usage.Store(99)
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("acquire after memory fell: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still held after memory fell below the soft limit")
	}
	if n := sub.memWaiting.Load(); n != 0 {
		t.Fatalf("memWaiting = %d after release, want 0", n)
	}
}

// Memory that never falls (steady state above the threshold) delays a
// resubscribe by at most memMaxWait; it never blocks it for good.
func TestMemoryGateGivesUpAfterMaxWait(t *testing.T) {
	var usage atomic.Uint64
	usage.Store(150)
	sub := newMemGateSub(&usage, 30*time.Millisecond)

	start := time.Now()
	release, err := sub.acquireResubscribe(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	if took := time.Since(start); took < 30*time.Millisecond || took > 5*time.Second {
		t.Fatalf("acquire took %v, want about memMaxWait (30ms)", took)
	}
}

// Canceling a held resubscribe returns the ctx error and takes no slot.
func TestMemoryGateCancel(t *testing.T) {
	var usage atomic.Uint64
	usage.Store(150)
	sub := newMemGateSub(&usage, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := sub.acquireResubscribe(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire = %v, want context.DeadlineExceeded", err)
	}
	if n := len(sub.resubSem); n != 0 {
		t.Fatalf("%d slots taken after cancel, want 0", n)
	}
}

// With no soft limit (no GOMEMLIMIT) the gate is off.
func TestMemoryGateOff(t *testing.T) {
	var usage atomic.Uint64
	usage.Store(1 << 62)
	sub := newMemGateSub(&usage, time.Minute)
	sub.memSoftLimit = 0

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := sub.acquireResubscribe(ctx)
	if err != nil {
		t.Fatalf("acquire with the gate off: %v", err)
	}
	release()
}

// The threshold follows GOMEMLIMIT and is off when none is set.
func TestMemSoftLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	debug.SetMemoryLimit(1000 << 20)
	if got, want := memSoftLimit(), uint64(800<<20); got != want {
		t.Fatalf("memSoftLimit() = %d with GOMEMLIMIT=1000MiB, want %d", got, want)
	}
	debug.SetMemoryLimit(1<<63 - 1)
	if got := memSoftLimit(); got != 0 {
		t.Fatalf("memSoftLimit() = %d with no GOMEMLIMIT, want 0", got)
	}
	if runtimeMemUsage() == 0 {
		t.Fatal("runtimeMemUsage() = 0, want the process's mapped memory")
	}
}
