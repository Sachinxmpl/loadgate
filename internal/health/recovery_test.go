package health

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sachinxmpl/loadgate/internal/config"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func testHealth(interval, timeout, cooldown time.Duration, rise, fall int) config.Health {
	return config.Health{
		Active: config.ActiveHealth{
			Interval: config.Duration(interval),
			Timeout:  config.Duration(timeout),
			Rise:     rise,
		},
		Passive: config.PassiveHealth{
			Fall:     fall,
			Cooldown: config.Duration(cooldown),
		},
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// An evicted backend that comes back is readmitted by the prober after cooldown + rise good probes and never by anything else.
func TestManager_ReadmitsRecoveredBackend(t *testing.T) {
	// A backend we can turn on and off by opening/closing a listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	acceptClose(t, ln) // start accepting so probes succeed

	reg := NewRegistry()
	added, _ := reg.Reconcile([]string{addr})

	h := testHealth(20*time.Millisecond, 100*time.Millisecond, 40*time.Millisecond, 2, 3)
	mgr := NewManager(reg, h, discardLog(), nil)
	mgr.Sync(added, nil)
	defer mgr.Stop()

	// Evict it via the passive path.
	st := reg.Get(addr)
	for i := 0; i < 3; i++ {
		st.ReportFailure(3)
	}
	if st.Admits() {
		t.Fatal("backend should be evicted before recovery")
	}

	// The backend is actually alive (listener still accepting), so once
	// cooldown elapses the prober should readmit it within a few ticks.
	if !waitFor(2*time.Second, func() bool { return st.Admits() }) {
		t.Fatalf("backend was not readmitted; final phase = %v", st.Phase())
	}
}

// A backend that stays dead is never readmitted, no matter how long the prober runs.
func TestManager_DoesNotReadmitDeadBackend(t *testing.T) {

	// Bind then close: the address refuses connections.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	reg := NewRegistry()
	added, _ := reg.Reconcile([]string{addr})
	h := testHealth(20*time.Millisecond, 100*time.Millisecond, 40*time.Millisecond, 2, 3)
	mgr := NewManager(reg, h, discardLog(), nil)
	mgr.Sync(added, nil)
	defer mgr.Stop()

	st := reg.Get(addr)
	for i := 0; i < 3; i++ {
		st.ReportFailure(3)
	}

	// Give the prober plenty of ticks,  a dead backend must never come back.
	time.Sleep(300 * time.Millisecond)
	if st.Admits() {
		t.Errorf("dead backend was readmitted; phase = %v", st.Phase())
	}
}

// With health.active.path set, an evicted backend whose port is open but health endpoint answers 503 stays out
// Only 2xx answers are readmitted
func TestManager_HTTPProbeChecksStatus(t *testing.T) {
	var healthy atomic.Bool
	be := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if !healthy.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			},
		),
	)
	defer be.Close()
	addr := strings.TrimPrefix(be.URL, "http://")

	reg := NewRegistry()
	added, _ := reg.Reconcile([]string{addr})

	h := testHealth(20*time.Millisecond, 100*time.Millisecond, 40*time.Millisecond, 2, 3)
	h.Active.Path = "/health"
	mgr := NewManager(reg, h, discardLog(), nil)
	mgr.Sync(added, nil)
	defer mgr.Stop()

	st := reg.Get(addr)
	for i := 0; i < 3; i++ {
		st.ReportFailure(3)
	}

	// port open, /health says 503, must stay evicted
	time.Sleep(300 * time.Millisecond)
	if st.Admits() {
		t.Fatalf("backend answering 503 on /health was readmitted; phase = %v", st.Phase())
	}

	// /health recovers, must come back
	healthy.Store(true)
	if !waitFor(2*time.Second, st.Admits) {
		t.Fatalf("backend answering 200 on /health was not readmitted; phase = %v", st.Phase())
	}

}

// With health.active.fall set, a backend that is Healthy (no passive failures at all) is evicted by the prober when /health starts failing, and readmitted when it recovers
func TestManager_ActiveFallEvictsHealthyBackend(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer be.Close()
	addr := strings.TrimPrefix(be.URL, "http://")

	reg := NewRegistry()
	added, _ := reg.Reconcile([]string{addr})
	h := testHealth(20*time.Millisecond, 100*time.Millisecond, 40*time.Millisecond, 2, 3)
	h.Active.Path = "/health"
	h.Active.Fall = 2
	mgr := NewManager(reg, h, discardLog(), nil)
	mgr.Sync(added, nil)
	defer mgr.Stop()

	st := reg.Get(addr)

	healthy.Store(false)
	if !waitFor(time.Second, func() bool { return !st.Admits() }) {
		t.Fatalf("healthy backend whose /health fails was not evicted; phase = %v", st.Phase())
	}

	healthy.Store(true)
	if !waitFor(2*time.Second, st.Admits) {
		t.Fatalf("backend was not readmitted after /health recovered; phase = %v", st.Phase())
	}
}

// Removed backends probers are cancelled, and no goroutine leaks.
func TestManager_SyncStartsAndStopsProbers(t *testing.T) {

	reg := NewRegistry()
	h := testHealth(20*time.Millisecond, 100*time.Millisecond, 40*time.Millisecond, 2, 3)
	mgr := NewManager(reg, h, discardLog(), nil)

	added, _ := reg.Reconcile([]string{"a:1", "b:2", "c:3"})
	mgr.Sync(added, nil)

	// Reload: drop b:2.
	added2, removed2 := reg.Reconcile([]string{"a:1", "c:3"})
	mgr.Sync(added2, removed2)

	mgr.mu.Lock()
	_, bStillProbing := mgr.cancels["b:2"]
	count := len(mgr.cancels)
	mgr.mu.Unlock()

	if bStillProbing {
		t.Error("prober for removed backend b:2 was not cancelled")
	}
	if count != 2 {
		t.Errorf("expected 2 active probers, got %d", count)
	}

	mgr.Stop()
}

// Keeps accepting and immediately closing connections until the
// listener is closed by test cleanup , enough for a probe to see it alive.
func acceptClose(t *testing.T, ln net.Listener) {
	t.Helper()
	go func() { // exits when: ln is closed.
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
