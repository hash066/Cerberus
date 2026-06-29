package supervisor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// crashOnce fails on its first run (abnormal exit) and signals on the second,
// proving the supervisor restarted it after a forced crash.
type crashOnce struct {
	runs     int32
	restarted chan struct{}
}

func (c *crashOnce) Serve(ctx context.Context) error {
	n := atomic.AddInt32(&c.runs, 1)
	if n == 1 {
		return errors.New("forced crash")
	}
	select {
	case c.restarted <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestPermanentChildRestartsAfterCrash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tree := New("root")
	child := &crashOnce{restarted: make(chan struct{}, 1)}
	tree.Supervise(Permanent, child)

	go func() { _ = tree.Serve(ctx) }()

	select {
	case <-child.restarted:
	case <-ctx.Done():
		t.Fatal("supervisor did not restart child after forced crash")
	}
	if got := atomic.LoadInt32(&child.runs); got < 2 {
		t.Fatalf("runs = %d, want >= 2 (initial + restart)", got)
	}
}

// cleanExit returns nil immediately; under Transient policy it must NOT restart.
type cleanExit struct{ runs int32 }

func (c *cleanExit) Serve(_ context.Context) error {
	atomic.AddInt32(&c.runs, 1)
	return nil
}

func TestTransientDoesNotRestartOnCleanExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tree := New("root")
	child := &cleanExit{}
	tree.Supervise(Transient, child)
	go func() { _ = tree.Serve(ctx) }()

	time.Sleep(1 * time.Second)
	if got := atomic.LoadInt32(&child.runs); got != 1 {
		t.Fatalf("runs = %d, want 1 (clean exit must not restart)", got)
	}
}
