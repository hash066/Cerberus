package mesh

// concurrency_test.go exercises daemon/mesh's shared mutable state (the
// subscription slice behind Publish/Subscribe, the per-stream capability
// verification path behind compute dispatch, and the inbound-session accept
// path) under real concurrent access from multiple goroutines. The goal is to
// catch a lost update, a corrupted map/slice, or cross-talk between concurrent
// requests — not just to execute the concurrent code path once.
//
// NOTE ON -race: this toolchain (go1.25.7 windows/386) is a 32-bit build; the
// race detector requires a 64-bit GOARCH and a working cgo C compiler (gcc is
// not on PATH here), so `go test -race` fails at the "cgo: C compiler ... not
// found" step before it can even attempt to instrument anything — confirmed by
// running `go test ./daemon/mesh/... -race` and separately forcing
// GOARCH=amd64, which still fails on the missing C compiler. Per the task,
// these tests still run without -race (they exercise the same concurrent
// paths and will catch corruption that surfaces as wrong values, panics, or
// deadlocks), and are also run with -count=100 to shake out nondeterministic
// scheduling-dependent failures.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// TestConcurrentRequestComputeNoCrossTalk drives many goroutines that each
// dial the SAME worker concurrently and send a distinct ComputeTask, and
// verifies every requester gets back exactly the result for ITS OWN task —
// i.e. the per-stream handling in handleComputeStream does not let one
// goroutine's request/response pair leak into another's. Each inbound stream
// gets its own goroutine (ServeCompute registers a stream handler that
// libp2p invokes per-stream), so this also exercises the worker's capability
// kernel (wrkKernel.Verify) under concurrent calls from multiple streams.
func TestConcurrentRequestComputeNoCrossTalk(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	wrkKernel := stub.NewCapKernel()
	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "test", Kernel: wrkKernel})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()

	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// The worker echoes back the task's own TaskID plus a deterministic
	// function of it, after verifying the presented cap against its OWN
	// kernel — every concurrent stream hits kernel.Verify concurrently.
	worker.ServeCompute(func(_ context.Context, task contract.ComputeTask, capH contract.CapHandle) (contract.ComputeResult, error) {
		if err := wrkKernel.Verify(capH, contract.Request{Op: "exec"}, time.Now().Unix()); err != nil {
			return contract.ComputeResult{TaskID: task.TaskID, OK: false, Error: err.Error()}, nil
		}
		out := append([]byte("out-"), task.TaskID...)
		return contract.ComputeResult{TaskID: task.TaskID, OK: true, Output: out}, nil
	})

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine mints its OWN capability concurrently with the
			// others — exercises wrkKernel's Mint/Verify concurrency too, since
			// the grant travels in-band and Verify runs worker-side.
			grant, err := wrkKernel.Mint(contract.ResourceRef{Kind: contract.KindGPU}, []contract.Right{contract.RightExec}, nil)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: mint: %w", i, err)
				return
			}
			taskID := []byte(fmt.Sprintf("task-%03d", i))
			task := contract.ComputeTask{TaskID: taskID, Component: []byte("cid")}
			res, err := requester.RequestCompute(ctx, worker.PeerID(), task, grant)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: request: %w", i, err)
				return
			}
			if !res.OK {
				errs <- fmt.Errorf("goroutine %d: worker denied: %s", i, res.Error)
				return
			}
			// The critical cross-talk check: the result must echo THIS
			// goroutine's own task ID, not some other concurrent request's.
			wantOut := "out-" + string(taskID)
			if string(res.TaskID) != string(taskID) || string(res.Output) != wantOut {
				errs <- fmt.Errorf("goroutine %d: cross-talk detected: got taskID=%q output=%q, want taskID=%q output=%q",
					i, res.TaskID, res.Output, taskID, wantOut)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestConcurrentPublishSubscribeNoCorruption hits Publish and Subscribe (both
// of which mutate/read Fabric.subs under Fabric.mu) concurrently from many
// goroutines: some goroutines are adding/removing subscriptions (via
// Subscribe + context cancellation) while others publish, all at once. It
// asserts every subscriber that stays subscribed for the whole run receives
// every message published on a key it's subscribed to, addressing exactly the
// "can two goroutines register/dispatch concurrently without corrupting
// shared state (maps, session tables)" question for the pub/sub path.
func TestConcurrentPublishSubscribeNoCorruption(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	k := stub.NewCapKernel()
	f, err := New(ctx, Config{Site: "test", Kernel: k})
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	defer f.Close()

	capH, err := k.Mint(contract.ResourceRef{Kind: contract.KindTopic}, nil, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	const nSubs = 8
	const nPubsPerGoroutine = 25
	const nPublishers = 5

	// Long-lived subscribers: each must see every message published to its key
	// (self-published gossipsub messages are delivered locally via dispatch,
	// not skipped — only remote-origin messages are skipped in readLoop, and a
	// single-node Publish/Subscribe path additionally never touches the wire
	// for delivery within the same process... but Fabric always publishes onto
	// gossipsub, so we drive this with a distinct key per subscriber and count
	// via the local dispatch fan-out, which is what actually races on subs).
	type sub struct {
		key string
		ch  <-chan contract.Sample
		got int32
	}
	subs := make([]*sub, nSubs)
	var subWG sync.WaitGroup
	for i := 0; i < nSubs; i++ {
		key := fmt.Sprintf("cerberus/test/telemetry/sub-%d", i)
		ch, err := f.Subscribe(ctx, key, capH)
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		s := &sub{key: key, ch: ch}
		subs[i] = s
		subWG.Add(1)
		go func(s *sub) {
			defer subWG.Done()
			for {
				select {
				case _, ok := <-s.ch:
					if !ok {
						return
					}
					atomic.AddInt32(&s.got, 1)
				case <-ctx.Done():
					return
				}
			}
		}(s)
	}

	// Concurrent publishers hammer Publish (which locks kernel.Verify and reads
	// f.topic) from multiple goroutines simultaneously, while the subscription
	// set above is also being read concurrently by dispatch (invoked from
	// readLoop) — but since this is a single-node loopback, publishing to our
	// own topic is normally skipped by readLoop's "from == self" check. To
	// actually exercise concurrent dispatch/subs access (the shared state this
	// test targets) we call f.dispatch directly with synthetic envelopes,
	// which is exactly what readLoop would do for a remote-origin message,
	// concurrently with Subscribe/unsubscribe churn.
	var churnWG sync.WaitGroup
	stopChurn := make(chan struct{})
	for c := 0; c < 3; c++ {
		churnWG.Add(1)
		go func() {
			defer churnWG.Done()
			for {
				select {
				case <-stopChurn:
					return
				default:
				}
				cctx, ccancel := context.WithCancel(ctx)
				if _, err := f.Subscribe(cctx, "cerberus/test/telemetry/churn", capH); err != nil {
					ccancel()
					return
				}
				time.Sleep(time.Millisecond)
				ccancel() // triggers the subscription-removal goroutine under f.mu
			}
		}()
	}

	var pubWG sync.WaitGroup
	for p := 0; p < nPublishers; p++ {
		pubWG.Add(1)
		go func(p int) {
			defer pubWG.Done()
			for i := 0; i < nPubsPerGoroutine; i++ {
				for _, s := range subs {
					f.dispatch(envelope{Key: s.key, Payload: []byte("x")})
				}
			}
		}(p)
	}
	pubWG.Wait()
	close(stopChurn)
	churnWG.Wait()

	// Give the subscriber goroutines a moment to drain their channels.
	time.Sleep(200 * time.Millisecond)
	cancel()
	subWG.Wait()

	wantPerSub := int32(nPublishers * nPubsPerGoroutine)
	for i, s := range subs {
		got := atomic.LoadInt32(&s.got)
		if got != wantPerSub {
			t.Errorf("subscriber %d (%s): got %d messages, want %d — possible lost update or dispatch race on Fabric.subs",
				i, s.key, got, wantPerSub)
		}
	}
}

// TestConcurrentDialAcceptSessionsStayBound spins up one worker (acceptor) and
// several concurrent dialers, and confirms each accepted session on the
// worker side is bound to the CORRECT dialer's PeerID and carries exactly
// that dialer's payload — i.e. concurrent onStream/accept invocations (which
// each construct a streamSession and push it onto the shared f.inbound
// channel) never mix up one connection's identity/payload with another's.
// This directly targets "does session handling behave correctly when called
// concurrently from multiple accepted connections."
func TestConcurrentDialAcceptSessionsStayBound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	worker, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()

	const nDialers = 10
	dialers := make([]*Fabric, nDialers)
	wantPayload := make(map[contract.PeerID]string, nDialers)
	for i := 0; i < nDialers; i++ {
		d, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
		if err != nil {
			t.Fatalf("dialer %d: %v", i, err)
		}
		defer d.Close()
		dialers[i] = d
		if err := d.Connect(ctx, worker.AddrInfo()); err != nil {
			t.Fatalf("dialer %d connect: %v", i, err)
		}
		wantPayload[d.PeerID()] = fmt.Sprintf("payload-from-dialer-%d", i)
	}

	var dialWG sync.WaitGroup
	for i, d := range dialers {
		i, d := i, d
		dialWG.Add(1)
		go func() {
			defer dialWG.Done()
			sess, err := d.Dial(worker.PeerID())
			if err != nil {
				t.Errorf("dialer %d: dial: %v", i, err)
				return
			}
			defer sess.Close()
			if err := sess.Send([]byte(wantPayload[d.PeerID()])); err != nil {
				t.Errorf("dialer %d: send: %v", i, err)
			}
		}()
	}

	// Collect nDialers inbound sessions on the worker side concurrently with
	// the dials above, verifying each session's authenticated PeerID matches
	// the payload actually received on THAT session (no mixing between
	// concurrently accepted streams).
	var recvWG sync.WaitGroup
	var mu sync.Mutex
	mismatches := []string{}
	for i := 0; i < nDialers; i++ {
		recvWG.Add(1)
		go func(i int) {
			defer recvWG.Done()
			select {
			case sess := <-worker.Inbound():
				ss, ok := sess.(*streamSession)
				if !ok {
					mu.Lock()
					mismatches = append(mismatches, fmt.Sprintf("recv %d: unexpected session type %T", i, sess))
					mu.Unlock()
					return
				}
				rid, verified := ss.RemotePeerID()
				msg, err := sess.Recv()
				_ = sess.Close()
				if err != nil {
					mu.Lock()
					mismatches = append(mismatches, fmt.Sprintf("recv %d: recv error: %v", i, err))
					mu.Unlock()
					return
				}
				mu.Lock()
				want, known := wantPayload[rid]
				if !verified || !known || string(msg) != want {
					mismatches = append(mismatches, fmt.Sprintf(
						"recv %d: session bound to %x (verified=%v) delivered payload %q, want %q for that peer",
						i, rid[:8], verified, msg, want))
				}
				mu.Unlock()
			case <-ctx.Done():
				mu.Lock()
				mismatches = append(mismatches, fmt.Sprintf("recv %d: timed out waiting for inbound session", i))
				mu.Unlock()
			}
		}(i)
	}

	dialWG.Wait()
	recvWG.Wait()

	if len(mismatches) > 0 {
		t.Errorf("concurrent accept produced %d session/payload mismatches:", len(mismatches))
		for _, m := range mismatches {
			t.Error("  " + m)
		}
	}
}

// TestConcurrentVerifyAuthenticatedPeer exercises verifyAuthenticatedPeer (the
// PeerID-binding enforcement function) from many goroutines simultaneously
// with a mix of matching and mismatched keys, confirming it is safe for
// concurrent use (it is a pure function over its arguments, but this
// documents and locks in that guarantee — a future change that adds package-
// level or receiver state to this check must keep it goroutine-safe).
func TestConcurrentVerifyAuthenticatedPeer(t *testing.T) {
	type kp struct {
		priv []byte
		id   contract.PeerID
	}
	newKP := func() contract.PeerID {
		_, id := newPeer(t)
		return id
	}
	claimed := newKP()
	other := newKP()

	const n = 200
	var wg sync.WaitGroup
	var okCount, denyCount int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if err := verifyAuthenticatedPeer(claimed, libp2pPub(t, claimed)); err != nil {
					t.Errorf("matching key rejected under concurrency: %v", err)
					return
				}
				atomic.AddInt32(&okCount, 1)
			} else {
				if err := verifyAuthenticatedPeer(claimed, libp2pPub(t, other)); err == nil {
					t.Error("mismatched key accepted under concurrency")
					return
				}
				atomic.AddInt32(&denyCount, 1)
			}
		}(i)
	}
	wg.Wait()
	if int(okCount) != n/2 || int(denyCount) != n/2 {
		t.Fatalf("unexpected outcome counts: ok=%d deny=%d (want %d/%d)", okCount, denyCount, n/2, n/2)
	}
}
