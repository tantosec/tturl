package timing

// cspell:ignore synctest

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestCommittedReadinessInterruptsBlockedControlWriter(t *testing.T) {
	client, peer := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = peer.Close() }()
	budget := newPhaseBudget(t.Context(), time.Second)
	defer budget.close()
	pacer := newStartPacer(0, 0)
	committed, err := pacer.commit(t.Context(), 2, budget, func(commit func()) bool {
		commit()
		return true
	}, nil)
	if !committed || err != nil {
		t.Fatalf("initial commitment: committed=%t err=%v", committed, err)
	}
	// The control writer owns the wire after commitment. Cancellation closes
	// blocked I/O without obtaining that same mutex, so the sender can wake.
	stop := context.AfterFunc(budget.ctx, func() { _ = client.Close() })
	defer stop()
	var wire sync.Mutex
	wire.Lock()
	writerDone := make(chan error, 1)
	go func() {
		_, err := client.Write([]byte("control"))
		wire.Unlock()
		writerDone <- err
	}()
	senderDone := make(chan error, 1)
	go func() {
		wire.Lock()
		defer wire.Unlock()
		senderDone <- context.Cause(budget.ctx)
	}()
	select {
	case err := <-senderDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unreleased sender lost readiness deadline: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked writer stranded the sender")
	}
	if err := <-writerDone; err == nil {
		t.Fatal("blocked control write unexpectedly succeeded")
	}
	if pacer.charges != 2 {
		t.Fatalf("failed committed trial charge = %d, want 2", pacer.charges)
	}
}

func TestPhaseBudgetAccumulatesActiveReadinessAcrossPauses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := newPhaseBudget(t.Context(), 20*time.Millisecond)
		defer budget.close()
		for _, phase := range []struct {
			active, remaining time.Duration
		}{
			{5 * time.Millisecond, 15 * time.Millisecond},
			{3 * time.Millisecond, 12 * time.Millisecond},
		} {
			time.Sleep(phase.active)
			if err := budget.pause(); err != nil {
				t.Fatal(err)
			}
			if budget.remaining != phase.remaining {
				t.Fatalf("remaining readiness = %s, want %s", budget.remaining, phase.remaining)
			}
			time.Sleep(100 * time.Millisecond)
			synctest.Wait()
			if err := context.Cause(budget.ctx); err != nil {
				t.Fatalf("paused readiness expired: %v", err)
			}
			if budget.remaining != phase.remaining {
				t.Fatal("pause changed remaining readiness")
			}
			budget.resume()
		}
		time.Sleep(11 * time.Millisecond)
		synctest.Wait()
		if err := context.Cause(budget.ctx); err != nil {
			t.Fatalf("readiness expired before cumulative allowance: %v", err)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if err := context.Cause(budget.ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("readiness did not expire after 20ms active time: %v", err)
		}
	})
}
