// Package supervisor implements Vertical 01's OTP-style supervision trees
// (Workstream B). It wraps thejerf/suture to give the daemon Erlang/OTP-grade
// fault tolerance: subsystems and peer sessions fail and recover predictably.
//
// Restart semantics (ARCHITECTURE.md §01):
//   - Permanent: always restart (e.g. zenoh/libp2p host, telemetry worker).
//   - Transient: restart only on abnormal exit (e.g. per-peer session).
//   - Temporary: never restart.
//
// suture restarts any service whose Serve returns a non-nil error (abnormal),
// with exponential backoff and a max-restart-intensity circuit breaker. A
// service that returns suture.ErrDoNotRestart / suture.ErrTerminateSupervisorTree
// opts out, which is how Transient/Temporary are expressed.
package supervisor

import (
	"context"
	"errors"

	"github.com/thejerf/suture/v4"
)

// Restart classifies a child's restart policy.
type Restart int

const (
	// Permanent services are always restarted on exit.
	Permanent Restart = iota
	// Transient services restart only on abnormal (error) exit.
	Transient
	// Temporary services are never restarted.
	Temporary
)

// Service is a long-running, restartable unit. Its Serve must honour ctx
// cancellation and return promptly when it is done.
type Service = suture.Service

// Tree is an OTP-style supervisor (a suture.Supervisor with Cerberus defaults).
type Tree struct {
	sup *suture.Supervisor
}

// New creates a named supervision tree.
func New(name string) *Tree {
	sup := suture.New(name, suture.Spec{
		// Keep failure accounting sensitive enough to trip on crash loops but
		// forgiving enough to tolerate a one-off restart.
		FailureDecay:     30,
		FailureThreshold: 5,
		FailureBackoff:   0, // use suture's default backoff
	})
	return &Tree{sup: sup}
}

// Supervise adds a child under the given restart policy and returns a token that
// can later be used to Remove it.
func (t *Tree) Supervise(policy Restart, svc Service) suture.ServiceToken {
	return t.sup.Add(wrap(policy, svc))
}

// Remove stops and detaches a previously supervised child.
func (t *Tree) Remove(tok suture.ServiceToken) error {
	return t.sup.Remove(tok)
}

// Serve runs the tree until ctx is cancelled. Run it in its own goroutine.
func (t *Tree) Serve(ctx context.Context) error {
	return t.sup.Serve(ctx)
}

// wrap adapts a Service to its restart policy.
func wrap(policy Restart, svc Service) Service {
	switch policy {
	case Permanent:
		return svc
	case Transient:
		return &transientService{inner: svc}
	case Temporary:
		return &temporaryService{inner: svc}
	default:
		return svc
	}
}

// transientService restarts only on abnormal exit: a clean return (nil or
// context cancellation) opts out of restart.
type transientService struct{ inner Service }

func (s *transientService) Serve(ctx context.Context) error {
	err := s.inner.Serve(ctx)
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return suture.ErrDoNotRestart
	}
	return err
}

// temporaryService is never restarted regardless of how it exits.
type temporaryService struct{ inner Service }

func (s *temporaryService) Serve(ctx context.Context) error {
	_ = s.inner.Serve(ctx)
	return suture.ErrDoNotRestart
}
