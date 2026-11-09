package l7

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sachinxmpl/loadgate/internal/balancer"
	"github.com/Sachinxmpl/loadgate/internal/config"
	"github.com/Sachinxmpl/loadgate/internal/health"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func newL7(t *testing.T, cfg *config.Config) *Server {
	t.Helper()

	if cfg.Timeouts.Request == 0 {
		cfg.Timeouts.Request = config.Duration(5 * time.Second)
	}
	reg := health.NewRegistry()
	reg.Reconcile(cfg.BackendAddrs())

	bals, _ := balancer.NewPools(config.AlgRoundRobin, reg)

	s := New(Options{Config: cfg, Store: config.NewStore(cfg), Balancers: bals, Registry: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.ShutDown(ctx); err != nil {
			t.Errorf("ShutDown: %v", err)
		}
	})

	return s
}

func recordingBackend(t *testing.T, gotReq *http.Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotReq = *r
		w.Header().Set("X-Backend", "hit")
		io.WriteString(w, "backend-reply")
	}))
}

func TestServeHTTP_RoutesLongestPrefix(t *testing.T) {
	var apiGot, webGot http.Request
	api := recordingBackend(t, &apiGot)
	web := recordingBackend(t, &webGot)
	t.Cleanup(api.Close)
	t.Cleanup(web.Close)

	cfg := &config.Config{
		Mode:   config.ModeL7,
		Listen: "127.0.0.1:0",
		Routes: []config.Route{
			{Match: config.RouteMatch{PathPrefix: "/"}, Pool: "web"},
			{Match: config.RouteMatch{PathPrefix: "/api"}, Pool: "api"},
		},
		Pools: map[string][]config.Backend{
			"api": {{Addr: strings.TrimPrefix(api.URL, "http://"), Weight: 1}},
			"web": {{Addr: strings.TrimPrefix(web.URL, "http://"), Weight: 1}},
		},
	}
	s := newL7(t, cfg)

	// /api/users -> api pool (longer prefix), even though "/" is listed first.
	get(t, s, "/api/users")
	if apiGot.URL.Path != "/api/users" {
		t.Errorf("api backend got path %q, want /api/users", apiGot.URL.Path)
	}
	// /about -> web pool.
	get(t, s, "/about")
	if webGot.URL.Path != "/about" {
		t.Errorf("web backend got path %q, want /about", webGot.URL.Path)
	}
}

// sendXFF sends a GET with the given X-Forwarded-For header lines and returns the single X-Forwarded-For value the backend received
func sendXFF(t *testing.T, cfg *config.Config, got *http.Request, lines ...string) string {
	t.Helper()
	s := newL7(t, cfg)

	req, _ := http.NewRequest("GET", "http://"+s.Addr().String()+"/", nil)
	for _, l := range lines {
		req.Header.Add("X-Forwarded-For", l)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(resp)

	if n := len(got.Header.Values("X-Forwarded-For")); n != 1 {
		t.Errorf("backend got %d X-Forwarded-For lines, want 1", n)
	}
	return got.Header.Get("X-Forwarded-For")
}

// From a trusted proxy, the whole incoming chain is kept (every header line)
// the proxy's own IP is appended
func TestServeHTTP_TrustedProxyKeepsXForwardedForChain(t *testing.T) {
	var got http.Request
	be := recordingBackend(t, &got)
	t.Cleanup(be.Close)

	cfg := singlePoolL7(be)
	cfg.TrustedProxies = []config.CIDR{{Prefix: netip.MustParsePrefix("127.0.0.1/32")}}

	xff := sendXFF(t, cfg, &got, "203.0.113.7", "198.51.100.1")
	if want := "203.0.113.7, 198.51.100.1, 127.0.0.1"; xff != want {
		t.Errorf("X-Forwarded-For = %q, want %q", xff, want)
	}
}

// From a client that is not a trusted proxy, any X-Forwarded-For it sent is dropped
// the backend sees only the client's real address
func TestServeHTTP_UntrustedClientCannotSpoofXForwardedFor(t *testing.T) {
	var got http.Request
	be := recordingBackend(t, &got)
	t.Cleanup(be.Close)

	xff := sendXFF(t, singlePoolL7(be), &got, "6.6.6.6")
	if xff != "127.0.0.1" {
		t.Errorf("X-Forwarded-For = %q, want only the real client IP %q", xff, "127.0.0.1")
	}
}

// hop-by-hop headers are stripped, including the header named inside the Connection header (the part that is easy to forget).
func TestServeHTTP_StripsHopByHop(t *testing.T) {
	var got http.Request
	be := recordingBackend(t, &got)
	t.Cleanup(be.Close)

	s := newL7(t, singlePoolL7(be))

	req, _ := http.NewRequest("GET", "http://"+s.Addr().String()+"/", nil)
	req.Header.Set("Connection", "X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "should-be-stripped")
	req.Header.Set("Keep-Alive", "timeout=5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(resp)

	if v := got.Header.Get("X-Custom-Hop"); v != "" {
		t.Errorf("Connection-named header leaked to backend: %q", v)
	}
	if v := got.Header.Get("Keep-Alive"); v != "" {
		t.Errorf("Keep-Alive leaked to backend: %q", v)
	}
}

// An unmatched path answers 404
func TestServeHTTP_NoRouteIs404(t *testing.T) {
	var got http.Request
	be := recordingBackend(t, &got)
	t.Cleanup(be.Close)

	cfg := &config.Config{
		Mode:   config.ModeL7,
		Listen: "127.0.0.1:0",
		Routes: []config.Route{{Match: config.RouteMatch{PathPrefix: "/api"}, Pool: "api"}},
		Pools:  map[string][]config.Backend{"api": {{Addr: strings.TrimPrefix(be.URL, "http://"), Weight: 1}}},
	}
	s := newL7(t, cfg)

	resp := mustGet(t, s, "/nowhere")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unmatched path", resp.StatusCode)
	}
}

// An unreachable backend answers 502 AND is reported to health: a round-trip
// error is the l7 passive-eviction signal. With fall=1 one failure evicts.
func TestServeHTTP_DeadBackendIs502AndReportsFailure(t *testing.T) {
	const deadAddr = "127.0.0.1:1" // port 1: connection refused

	cfg := &config.Config{
		Mode:   config.ModeL7,
		Listen: "127.0.0.1:0",
		Health: config.Health{Passive: config.PassiveHealth{Fall: 1}},
		Timeouts: config.Timeouts{
			Dial:    config.Duration(200 * time.Millisecond),
			Request: config.Duration(500 * time.Millisecond),
		},
		Routes: []config.Route{{Match: config.RouteMatch{PathPrefix: "/"}, Pool: "p"}},
		Pools:  map[string][]config.Backend{"p": {{Addr: deadAddr, Weight: 1}}},
	}
	s := newL7(t, cfg)

	resp := mustGet(t, s, "/")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a dead backend", resp.StatusCode)
	}
	if s.registry.Get(deadAddr).Admits() {
		t.Error("dead backend should be evicted after a round-trip failure (fall=1)")
	}
}

func singlePoolL7(be *httptest.Server) *config.Config {
	return &config.Config{
		Mode:   config.ModeL7,
		Listen: "127.0.0.1:0",
		Routes: []config.Route{{Match: config.RouteMatch{PathPrefix: "/"}, Pool: "p"}},
		Pools:  map[string][]config.Backend{"p": {{Addr: strings.TrimPrefix(be.URL, "http://"), Weight: 1}}},
	}
}

// Performs a GET through the server, drains and closes the body, and returns the response.
func mustGet(t *testing.T, s *Server, path string) *http.Response {
	t.Helper()
	resp, err := http.Get("http://" + s.Addr().String() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	drain(resp)
	return resp
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// get fires a request and discards the response, for tests that assert on what
// the backend received rather than on the response. It returns nothing so the bodyclose linter can see the body is closed.
func get(t *testing.T, s *Server, path string) {
	t.Helper()
	resp, err := http.Get("http://" + s.Addr().String() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	drain(resp)
}

// A slow BE but healthy must be served
func TestServeHTTP_SlowBackendWithinResponseHeaderTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		io.WriteString(w, "slow but fine")
	}))
	defer slow.Close()

	cfg := singlePoolL7(slow)
	cfg.Health.Passive.Fall = 1
	cfg.Timeouts.ResponseHeader = config.Duration(2 * time.Second)
	s := newL7(t, cfg)

	resp := mustGet(t, s, "/")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from a 400ms backend", resp.StatusCode)
	}
	addr := strings.TrimPrefix(slow.URL, "http://")
	if !s.registry.Get(addr).Admits() {
		t.Error("slow but healthy backend was evicted")
	}
}

// A streamed response must reach the client as the backend sends it
func TestServeHTTP_StreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer be.Close()
	defer close(release)

	s := newL7(t, singlePoolL7(be))

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + s.Addr().String() + "/")
	if err != nil {
		t.Fatalf("no response within 1s, the stream is being buffered: %v", err)
	}
	defer resp.Body.Close()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("reading first event: %v", err)
	}
	if line != "data: first\n" {
		t.Errorf("first line = %q, want %q", line, "data: first\n")
	}
}

func TestServeHTTP_EachPoolBalancesIndependently(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	counting := func(name string) *httptest.Server {
		be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name]++
			mu.Unlock()
		}))
		t.Cleanup(be.Close)
		return be
	}

	addr := func(be *httptest.Server) string {
		return strings.TrimPrefix(be.URL, "http://")
	}

	a1, a2, b1, b2 := counting("a1"), counting("a2"), counting("b1"), counting("b2")

	cfg := &config.Config{
		Mode:   config.ModeL7,
		Listen: "127.0.0.1:0",
		Routes: []config.Route{
			{Match: config.RouteMatch{PathPrefix: "/a"}, Pool: "a"},
			{Match: config.RouteMatch{PathPrefix: "/b"}, Pool: "b"},
		},
		Pools: map[string][]config.Backend{
			"a": {{Addr: addr(a1), Weight: 1}, {Addr: addr(a2), Weight: 1}},
			"b": {{Addr: addr(b1), Weight: 1}, {Addr: addr(b2), Weight: 1}},
		},
	}

	s := newL7(t, cfg)

	for i := 0; i < 10; i++ {
		get(t, s, "/a")
		get(t, s, "/b")
	}

	for _, name := range []string{"a1", "a2", "b1", "b2"} {
		if hits[name] != 5 {
			t.Errorf("%s got %d requests, want 5 (hits: %v)", name, hits[name], hits)
		}
	}
}

// When every backend in the pool is evicted, requests are still sent to them (fail-open) instead of a 503, so a backend that has already recovered answers straight away
func TestServeHTTP_AllEvictedFailsOpen(t *testing.T) {
	var got http.Request
	be := recordingBackend(t, &got)
	t.Cleanup(be.Close)

	s := newL7(t, singlePoolL7(be))

	// evicted, although it is actually alive
	st := s.registry.Get(strings.TrimPrefix(be.URL, "http://"))
	st.ReportFailure(1)
	if st.Admits() {
		t.Fatal("setup: backend should be evicted")
	}

	resp := mustGet(t, s, "/")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 from the evicted but alive backend (fail-open)", resp.StatusCode)
	}
}

// With passive.fail_on_5xx, 5xx answers count toward passive.fall and still reach the client unchanged.
func TestServeHTTP_FailOn5xx(t *testing.T) {
	cases := []struct {
		status  int
		evicted bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusNotFound, false},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(be.Close)

			cfg := singlePoolL7(be)
			cfg.Health.Passive.Fall = 2
			cfg.Health.Passive.FailOn5xx = true
			s := newL7(t, cfg)

			for i := 0; i < 2; i++ {
				resp := mustGet(t, s, "/")
				resp.Body.Close()
				if resp.StatusCode != tc.status {
					t.Fatalf("status %d reached the client as %d", tc.status, resp.StatusCode)
				}
			}

			st := s.registry.Get(strings.TrimPrefix(be.URL, "http://"))
			if evicted := !st.Admits(); evicted != tc.evicted {
				t.Errorf("after 2 answers of %d: evicted = %v, want %v", tc.status, evicted, tc.evicted)
			}
		})
	}
}
