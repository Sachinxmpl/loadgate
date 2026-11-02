package l7

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	bal, _ := balancer.New(config.AlgRoundRobin, reg)

	s := New(Options{Config: cfg, Store: config.NewStore(cfg), Balancer: bal, Registry: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

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

// X-Forwarded-For is appended to, not replaced, so an upstream proxy's entry survives.
func TestServeHTTP_AppendsXForwardedFor(t *testing.T) {
	var got http.Request
	be := recordingBackend(t, &got)
	t.Cleanup(be.Close)

	s := newL7(t, singlePoolL7(be))

	req, _ := http.NewRequest("GET", "http://"+s.Addr().String()+"/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7") // pretend an upstream proxy
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(resp)

	xff := got.Header.Get("X-Forwarded-For")
	if !strings.HasPrefix(xff, "203.0.113.7, ") {
		t.Errorf("X-Forwarded-For = %q, want the prior value kept then our IP appended", xff)
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
