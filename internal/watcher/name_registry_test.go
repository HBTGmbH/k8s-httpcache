package watcher

import (
	"fmt"
	"sync"
	"testing"
)

// TestNameRegistry_ClaimReleaseSemantics exercises every claim/release branch:
// free claim, idempotent re-claim by the same owner, conflict with a different
// owner, release by the owner, release by a non-owner (no-op), release of a
// never-claimed name (no-op), and independence of distinct names.
func TestNameRegistry_ClaimReleaseSemantics(t *testing.T) {
	t.Parallel()
	r := NewNameRegistry()

	// Free name → claim succeeds.
	if !r.claim("web", "owner-a") {
		t.Fatal("claiming a free name should succeed")
	}
	// Same name, same owner → idempotent success.
	if !r.claim("web", "owner-a") {
		t.Fatal("re-claiming by the same owner should succeed")
	}
	// Same name, different owner → conflict.
	if r.claim("web", "owner-b") {
		t.Fatal("claiming a name held by another owner must fail")
	}
	// Distinct names are independent.
	if !r.claim("api", "owner-b") {
		t.Fatal("claiming a different free name should succeed")
	}

	// Release by a non-owner is a no-op: owner-a still holds "web".
	r.release("web", "owner-b")
	if r.claim("web", "owner-b") {
		t.Fatal("release by a non-owner must not free the name")
	}

	// Release by the owner frees it; another owner can then claim.
	r.release("web", "owner-a")
	if !r.claim("web", "owner-b") {
		t.Fatal("after the owner releases, another owner should be able to claim")
	}

	// Releasing a never-claimed name is a harmless no-op.
	r.release("never-claimed", "owner-a")

	// Internal state holds exactly the expected owners.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners["web"] != "owner-b" || r.owners["api"] != "owner-b" {
		t.Fatalf("unexpected registry state: %v", r.owners)
	}
	if len(r.owners) != 2 {
		t.Fatalf("expected 2 owned names, got %d: %v", len(r.owners), r.owners)
	}
}

// TestNameRegistry_ConcurrentSingleWinner asserts that when many goroutines race
// to claim the same name, exactly one wins and the registry records it.
func TestNameRegistry_ConcurrentSingleWinner(t *testing.T) {
	t.Parallel()
	r := NewNameRegistry()

	const claimers = 64
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		wins   int
		winner string
	)
	for i := range claimers {
		owner := fmt.Sprintf("owner-%d", i)
		wg.Go(func() {
			if r.claim("web", owner) {
				mu.Lock()
				wins++
				winner = owner
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("exactly one claimer must win, got %d", wins)
	}
	r.mu.Lock()
	got := r.owners["web"]
	r.mu.Unlock()
	if got != winner {
		t.Fatalf("registry owner = %q, want the winning claimer %q", got, winner)
	}
}

// TestNameRegistry_ConcurrentClaimReleaseRace stresses concurrent claim/release
// over a shared set of names under the race detector. Because every successful
// claim is paired with a release by the same owner, the registry must be empty
// once all goroutines finish.
func TestNameRegistry_ConcurrentClaimReleaseRace(t *testing.T) {
	t.Parallel()
	r := NewNameRegistry()

	const (
		workers = 16
		names   = 8
		rounds  = 500
	)
	var wg sync.WaitGroup
	for w := range workers {
		owner := fmt.Sprintf("owner-%d", w)
		wg.Go(func() {
			for round := range rounds {
				name := fmt.Sprintf("svc-%d", (w+round)%names)
				if r.claim(name, owner) {
					r.release(name, owner)
				}
			}
		})
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.owners) != 0 {
		t.Fatalf("registry should be empty after all claims released, got %v", r.owners)
	}
}
