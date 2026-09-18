package migrator

import (
	"context"
	"errors"
	"testing"
	"time"
)

func fastCfg() Config {
	c := FastConfig()
	return c
}

func TestLeaseMutexAndTakeover(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(dir)
	cfg := fastCfg()
	cfg.LeaseDuration = 120 * time.Millisecond
	m1 := NewLeaseManager(s, "inst-A", cfg)
	m2 := NewLeaseManager(s, "inst-B", cfg)
	ctx := context.Background()

	l1, err := m1.Acquire(ctx)
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	if _, err := m2.Acquire(ctx); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("B should be blocked, got %v", err)
	}
	// A renews fine.
	if err := m1.Renew(l1.FenceToken); err != nil {
		t.Fatalf("A renew: %v", err)
	}
	// simulate A crash: stop renewing; wait for expiry.
	time.Sleep(200 * time.Millisecond)
	l2, err := m2.Acquire(ctx)
	if err != nil {
		t.Fatalf("B takeover after expiry: %v", err)
	}
	if l2.FenceToken <= l1.FenceToken {
		t.Fatalf("fence token should increase: %d -> %d", l1.FenceToken, l2.FenceToken)
	}
	// stale A renew must fail with ErrLeaseLost.
	if err := m1.Renew(l1.FenceToken); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale A renew should fail, got %v", err)
	}
	if err := m2.Release(l2.FenceToken); err != nil {
		t.Fatalf("B release: %v", err)
	}
	// after release A can acquire again with a newer token.
	l3, err := m1.Acquire(ctx)
	if err != nil {
		t.Fatalf("A re-acquire: %v", err)
	}
	if l3.FenceToken <= l2.FenceToken {
		t.Fatalf("token should increase after release: %d -> %d", l2.FenceToken, l3.FenceToken)
	}
}
