package balancer

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Sachinxmpl/loadgate/internal/config"
	"github.com/Sachinxmpl/loadgate/internal/health"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func testPool(n int) []config.Backend {
	pool := make([]config.Backend, n)
	for i := range pool {
		pool[i] = config.Backend{Addr: string(rune('a' + i)), Weight: 1}
	}
	return pool
}

func TestRoundRobin_Order(t *testing.T) {
	pool := testPool(3)
	var rr RoundRobin

	want := []string{"a", "b", "c", "a", "b", "c", "a"}
	for i, w := range want {
		got, err := rr.Pick("", pool)
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if got.Addr != w {
			t.Errorf("pick %d = %q, want %q", i, got.Addr, w)
		}
	}
}

func TestRoundRobin_EmptyPool(t *testing.T) {
	var rr RoundRobin

	for _, pool := range [][]config.Backend{nil, {}} {
		got, err := rr.Pick("", pool)
		if !errors.Is(err, ErrNoBackends) {
			t.Errorf("Pick(%v) error = %v, want ErrNoBackends", pool, err)
		}
		if got != nil {
			t.Errorf("Pick(%v) = %v, want nil backend alongside the error", pool, got)
		}
	}
}

func TestRoundRobin_ConcurrentDistribution(t *testing.T) {
	const (
		goroutines = 50
		picks      = 60
		backends   = 3
	)

	pool := testPool(backends)
	var rr RoundRobin

	counts := make([]map[string]int, goroutines)
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			local := make(map[string]int, backends)
			for i := 0; i < picks; i++ {
				b, err := rr.Pick("", pool)
				if err != nil {
					t.Errorf("goroutine %d pick %d: %v", g, i, err)
					return
				}
				local[b.Addr]++
			}
			counts[g] = local
		}(g)
	}
	wg.Wait()

	total := make(map[string]int, backends)
	for _, local := range counts {
		for addr, n := range local {
			total[addr] += n
		}
	}

	want := goroutines * picks / backends
	for _, b := range pool {
		if total[b.Addr] != want {
			t.Errorf("backend %q chosen %d times, want exactly %d", b.Addr, total[b.Addr], want)
		}
	}
}

// weightest RR tests

func weightedPool(weights ...int) []config.Backend {
	pool := make([]config.Backend, len(weights))
	names := []string{"A", "B", "C", "D", "E"}
	for i, w := range weights {
		pool[i] = config.Backend{
			Addr: names[i], Weight: w,
		}
	}
	return pool
}

func TestWeightedRoundRobin_SmoothOrder(t *testing.T) {
	pool := weightedPool(5, 1, 1) // A=5, B=1, C=1
	wrr := NewWeightedRoundRobin()

	want := []string{"A", "A", "B", "A", "C", "A", "A"}
	for i, w := range want {
		got, err := wrr.Pick("", pool)
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if got.Addr != w {
			t.Errorf("pick %d = %q, want %q (full sequence must be A A B A C A A)", i, got.Addr, w)
		}
	}
}

func TestWeightedRoundRobin_Distribution(t *testing.T) {
	pool := weightedPool(3, 1) // A should get 3x B
	wrr := NewWeightedRoundRobin()

	counts := map[string]int{}
	const picks = 4000 // multiple of total weight (4)
	for i := 0; i < picks; i++ {
		got, _ := wrr.Pick("", pool)
		counts[got.Addr]++
	}

	// Exact ratio ->  the algorithm is periodic with period = total weight
	if counts["A"] != 3000 || counts["B"] != 1000 {
		t.Errorf("distribution A=%d B=%d, want A=3000 B=1000", counts["A"], counts["B"])
	}
}

func TestWeightedRoundRobin_EqualWeightsIsRoundRobin(t *testing.T) {
	pool := weightedPool(1, 1, 1)
	wrr := NewWeightedRoundRobin()

	want := []string{"A", "B", "C", "A", "B", "C"}
	for i, w := range want {
		got, _ := wrr.Pick("", pool)
		if got.Addr != w {
			t.Errorf("pick %d = %q, want %q", i, got.Addr, w)
		}
	}
}

// Least Connections
type fakeCounter map[string]int64

func (f fakeCounter) Conns(addr string) int64 {
	return f[addr]
}

func TestLeastConnections_PicksFewest(t *testing.T) {
	pool := []config.Backend{{Addr: "a"}, {Addr: "b"}, {Addr: "c"}}
	counts := fakeCounter{"a": 5, "b": 1, "c": 9}
	lc := NewLeastConnections(counts)

	for i := 0; i < 10; i++ {
		got, err := lc.Pick("", pool)
		if err != nil {
			t.Fatal(err)
		}
		if got.Addr != "b" {
			t.Errorf("pick %d = %q, want b", i, got.Addr)
		}
	}
}

func TestLeastConnections_TiesSpread(t *testing.T) {
	pool := []config.Backend{{Addr: "a"}, {Addr: "b"}, {Addr: "c"}}
	counts := fakeCounter{"a": 0, "b": 0, "c": 0} // cold start all tied
	lc := NewLeastConnections(counts)

	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		got, _ := lc.Pick("", pool)
		seen[got.Addr]++
	}
	// round-robin tie-breaker must spread the cold-start burst
	for _, addr := range []string{"a", "b", "c"} {
		if seen[addr] != 100 {
			t.Errorf("tie spread: %q got %d, want 100 (herding on ties)", addr, seen[addr])
		}
	}
}

// Consistent Hashing
func hashPool(n int) []config.Backend {
	pool := make([]config.Backend, n)
	for i := range pool {
		pool[i] = config.Backend{Addr: fmt.Sprintf("10.0.0.%d:9000", i)}
	}
	return pool
}

func TestConsistentHash_MinimalRemap(t *testing.T) {
	const (
		backends = 10
		keys     = 100_000
	)
	full := hashPool(backends)
	reduced := full[:backends-1]

	ch := NewConsistentHash(defaultVNodes)

	// Two passes so the ring is built once per pool, not once per Pick.
	// Alternating full/reduced inside one loop defeats the ring cache and
	// rebuilds a 1500-vnode ring 200k times.
	before := make([]string, keys)
	for i := 0; i < keys; i++ {
		b, _ := ch.Pick(fmt.Sprintf("client-%d", i), full)
		before[i] = b.Addr
	}

	moved := 0
	for i := 0; i < keys; i++ {
		a, _ := ch.Pick(fmt.Sprintf("client-%d", i), reduced)
		if a.Addr != before[i] {
			moved++
		}
	}

	frac := float64(moved) / float64(keys)
	t.Logf("consistent hash: %.1f%% of keys moved when 1 of %d backends left (ideal ~%.1f%%)",
		frac*100, backends, 100.0/backends)

	// Should be near 1/N = 10%
	if frac > 0.20 {
		t.Errorf("remap fraction %.1f%% too high; consistent hashing should move ~1/N = 10%%", frac)
	}
}

func TestConsistentHash_BeatsModN(t *testing.T) {
	const backends, keys = 10, 100_000
	modN := func(key string, n int) int { return int(fnv64a(key) % uint64(n)) }

	moved := 0
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("client-%d", i)
		if modN(key, backends) != modN(key, backends-1) {
			moved++
		}
	}
	frac := float64(moved) / float64(keys)
	t.Logf("hash mod N: %.1f%% of keys moved (this is what consistent hashing avoids)", frac*100)
	if frac < 0.80 {
		t.Errorf("expected mod N to move ~90%%, got %.1f%%  check the contrast", frac*100)
	}
}

func TestConsistentHash_Sticky(t *testing.T) {
	pool := hashPool(5)
	ch := NewConsistentHash(defaultVNodes)

	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("k%d", i)
		a, _ := ch.Pick(key, pool)
		b, _ := ch.Pick(key, pool)
		if a.Addr != b.Addr {
			t.Fatalf("key %q mapped to %q then %q not sticky", key, a.Addr, b.Addr)
		}
	}
}

func TestConsistentHash_Distribution(t *testing.T) {
	for _, vnodes := range []int{10, 150} {
		pool := hashPool(5)
		r := buildRing(pool, vnodes, fnv64a)

		counts := map[string]int{}
		const keys = 50_000
		for i := 0; i < keys; i++ {
			addr, _ := r.lookup(fmt.Sprintf("k%d", i), fnv64a)
			counts[addr]++
		}

		ideal := keys / len(pool)
		maxDev := 0.0
		for _, c := range counts {
			dev := float64(c-ideal) / float64(ideal)
			if dev < 0 {
				dev = -dev
			}
			if dev > maxDev {
				maxDev = dev
			}
		}
		t.Logf("vnodes=%d: max deviation from even = %.1f%%", vnodes, maxDev*100)
	}
}

func TestPools_OneBalancerPerPool(t *testing.T) {
	p, err := NewPools(config.AlgRoundRobin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.For("a") != p.For("a") {
		t.Error("same pool name returned different balancers")
	}
	if p.For("a") == p.For("b") {
		t.Error("different pools share one balancer")
	}
}

func TestNewPools_RejectsUnknownAlgorithm(t *testing.T) {
	if _, err := NewPools("nope", nil); err == nil {
		t.Error("want error for unknown algorithm")
	}
}

// Only healthy backends are offered while at least one is healthy. When none is, the whole pool is offered (fail-open) instead of nothing
func TestHealthyBackends_FailsOpenWhenNoneHealthy(t *testing.T) {
	pool := testPool(3)
	reg := health.NewRegistry()
	reg.Reconcile([]string{"a", "b", "c"})

	reg.Get("a").ReportFailure(1)
	reg.Get("b").ReportFailure(1)
	if got := HealthyBackends(pool, reg); len(got) != 1 || got[0].Addr != "c" {
		t.Fatalf("with only c healthy got %v, want just c", got)
	}

	reg.Get("c").ReportFailure(1)
	if got := HealthyBackends(pool, reg); len(got) != 3 {
		t.Fatalf("with none healthy got %d backends, want all 3 (fail-open)", len(got))
	}
}
