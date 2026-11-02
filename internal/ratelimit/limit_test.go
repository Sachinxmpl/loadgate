package ratelimit

import (
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestLimiter_GlobalBurstThenRefill(t *testing.T) {
	l := New(10, 0)

	// The full burst of 10 is allowed immediately
	allowed := 0
	for i := 0; i < 10; i++ {
		if l.Allow("1.2.3.4") {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("burst allowed %d, want 10", allowed)
	}
	// The 11th in the same instant is rejected (bucket empty)
	if l.Allow("1.2.3.4") {
		t.Error("11th request allowed; burst should be exhausted")
	}
	// After ~1 token's worth of time, one more is allowed (refill)
	time.Sleep(150 * time.Millisecond)
	if !l.Allow("1.2.3.4") {
		t.Error("no token after refill window")
	}
}

func TestLimiter_PerClientIsolation(t *testing.T) {
	l := New(0, 5)

	// Client A exhausts its own bucket
	for i := 0; i < 5; i++ {
		if !l.Allow("10.0.0.1") {
			t.Fatalf("client A request %d rejected within its limit", i)
		}
	}
	if l.Allow("10.0.0.1") {
		t.Error("client A allowed past its per-client limit")
	}
	// Client B is unaffected isolation
	if !l.Allow("10.0.0.2") {
		t.Error("client B rejecged, client A was over limit, not isolated")
	}
}

func TestLimiter_ZeroIsUnlimited(t *testing.T) {
	l := New(0, 0)
	for i := 0; i < 1000; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatal("zero rates should mean unlimited")
		}
	}
}

func TestLRU_EvictsBeyondCap(t *testing.T) {
	c := newLRU(3)
	mk := func() *rate.Limiter { return rate.NewLimiter(1, 1) }

	a := c.get("a", mk)
	c.get("b", mk)
	c.get("c", mk)
	c.get("a", mk) // touch a -> most recent b is now least recent
	c.get("d", mk) // over cap -> evict b

	if _, ok := c.items["b"]; ok {
		t.Error("b should have been evicted as least-recently-used")
	}
	if len(c.items) != 3 {
		t.Errorf("cap exceeded: %d entries, want 3", len(c.items))
	}
	if c.get("a", mk) != a {
		t.Error("a was evicted or replaced; it should have survived (recently used)")
	}
}

func TestLimiter_AbuserDoesNotStarveOthers(t *testing.T) {
	l := New(100, 10)

	for i := 0; i < 200; i++ {
		l.Allow("abuser")
	}

	if !l.Allow("innocent") {
		t.Fatal("innocent client rejected: abuser drained the global bucket")
	}
}
