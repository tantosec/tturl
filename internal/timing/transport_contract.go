package timing

import (
	"context"
	"time"
)

type releasePhase uint8

const (
	initialPhase releasePhase = iota
	finalPhase
)

type releaseGates struct {
	initial, final <-chan struct{}
	anchor         time.Time
}

// Hooks never perform I/O or re-enter a transport lease.
type senderHooks struct {
	initialRelease   func(time.Time) error
	prefixComplete   func() error
	exchangeComplete func(time.Time) error
}

type transportMetadata struct {
	negotiatedProtocol, localAddress, remoteAddress string
}

// A lease belongs to one worker slot. Preparation acquires its sole sender;
// run relinquishes it only after full drain or safe stream failure disposal.
type transportLease interface {
	prepare(context.Context, Request) (transportSender, error)
	lockState()
	unlockState()
	usable() (bool, string)
	// usableLocked requires the lease state lock to be held.
	usableLocked() (bool, string)
	retirement() (bool, string)
	metadata() transportMetadata
	dispose(error)
	join()
}

// waitReady holds no wire lock. readyLocked runs under the lease state lock
// through the scheduler's gate commitment. run revalidates at actual writing.
type transportSender interface {
	waitReady(context.Context, releasePhase) error
	readyLocked(releasePhase) error
	// readinessSnapshot captures readiness and its change channel
	// under the state lock. Changes close the channel; another
	// snapshot supplies its replacement. Nil means no asynchronous
	// readiness changes beyond cancellation.
	readinessSnapshot(releasePhase) (bool, <-chan struct{})
	run(context.Context, releaseGates, senderHooks) Outcome
}
