package listener

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
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

func testServer(t *testing.T) *Server {
	t.Helper()

	cfg := &config.Config{
		Mode:     config.ModeL4,
		Listen:   "127.0.0.1:0",
		Balancer: config.AlgRoundRobin,
		Pools: map[string][]config.Backend{
			"default": {{
				Addr: "127.0.0.1:9001", Weight: 1,
			}},
		},
	}

	registry := health.NewRegistry()
	balancer, _ := balancer.New(cfg.Balancer, registry)

	s := New(Options{Addr: cfg.Listen, Store: config.NewStore(cfg), Balancer: balancer, Registry: registry, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.ShutDown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	return s
}

func TestServer_AcceptsConnections(t *testing.T) {
	// The handler is a real proxy, so a connection is served by relaying to the
	// backend — not closed on sight. Point at a live echo backend and prove a
	// full round-trip rather than asserting an immediate EOF (which only held
	// back when handle was a stub).
	backendAddr, stopBackend := echoBackend(t)
	defer stopBackend()

	cfg := &config.Config{
		Mode:     config.ModeL4,
		Listen:   "127.0.0.1:0",
		Balancer: config.AlgRoundRobin,
		Timeouts: config.Timeouts{
			Dial:  config.Duration(time.Second),
			Drain: config.Duration(time.Second),
		},
		Pools: map[string][]config.Backend{
			"default": {{Addr: backendAddr, Weight: 1}},
		},
	}
	reg := health.NewRegistry()
	reg.Reconcile(cfg.BackendAddrs())

	bal, _ := balancer.New(cfg.Balancer, reg)

	s := New(Options{Addr: cfg.Listen, Store: config.NewStore(cfg), Balancer: bal, Registry: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

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

	msg := []byte("ping\n")
	for i := 0; i < 5; i++ {
		conn, err := net.Dial("tcp", s.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write(msg); err != nil {
			t.Errorf("write %d: %v", i, err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Errorf("read %d: %v", i, err)
		} else if string(buf) != string(msg) {
			t.Errorf("echo %d = %q, want %q", i, buf, msg)
		}
		conn.Close()
	}
}

func TestServer_StartFailOnBusyAddress(t *testing.T) {
	s := testServer(t)

	copyServerOnSameAddr := New(Options{Addr: s.Addr().String(), Store: config.NewStore(&config.Config{}), Balancer: s.balancer, Registry: s.registry, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	if err := copyServerOnSameAddr.Start(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		copyServerOnSameAddr.ShutDown(ctx)
		t.Fatal("start on a busy address succeeded, expected an error")
	}
}

func TestServer_ShutdownStopsAccepting(t *testing.T) {
	s := testServer(t)
	addr := s.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.ShutDown(ctx); err != nil {
		t.Fatalf("Shutdown :%v", err)
	}

	conn, err := net.Dial("tcp", addr)
	if err == nil {
		conn.Close()
		t.Fatal("dial succeeded after Shutdown, expected connection refused")
	}
}

func TestServer_ShutdownIsIdempotent(t *testing.T) {
	s := testServer(t)

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.ShutDown(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Shutdown %d: %v", i, err)
		}
	}
}

func TestServer_AcceptLoopExitsOnClosedListener(t *testing.T) {
	s := testServer(t)

	// Closing the listener out from under the accept loop is what Shutdown does, the loop must treat it as a clean exit, not a transient error to retry.
	s.ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.ShutDown(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Shutdown after manual close: %v", err)
	}
}

// starts a loopback tcp echo server, returns its addr and stop func
func echoBackend(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	// exits when ln is closed by stop
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(done)
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	return ln.Addr().String(), func() { ln.Close(); <-done }
}

// With one dead backend among three, traffic stops going to it after `fall` failed dials
func TestHandle_EvictsDeadBackend(t *testing.T) {
	live1, stop1 := echoBackend(t)
	live2, stop2 := echoBackend(t)
	defer stop1()
	defer stop2()

	// dead backend
	// bind a port, then immediately close it so dials refuse
	deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := deadLn.Addr().String()
	deadLn.Close()

	cfg := &config.Config{
		Mode:   config.ModeL4,
		Listen: "127.0.0.1:0",
		Health: config.Health{Passive: config.PassiveHealth{Fall: 3}},
		Timeouts: config.Timeouts{
			Dial:  config.Duration(200 * time.Millisecond),
			Drain: config.Duration(time.Second),
		},
		Pools: map[string][]config.Backend{
			"default": {
				{Addr: live1, Weight: 1},
				{Addr: dead, Weight: 1},
				{Addr: live2, Weight: 1},
			},
		},
	}
	reg := health.NewRegistry()
	reg.Reconcile(cfg.BackendAddrs())

	bal, err := balancer.New(config.AlgRoundRobin, reg)
	if err != nil {
		t.Fatal(err)
	}

	srv := New(Options{Addr: cfg.Listen, Store: config.NewStore(cfg), Balancer: bal, Registry: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.ShutDown(ctx)
	})

	for i := 0; i < 15; i++ {
		conn, err := net.Dial("tcp", srv.Addr().String())
		if err != nil {
			t.Fatalf("client dial %d: %v", i, err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Write([]byte("ping\n"))
		buf := make([]byte, 5)
		io.ReadFull(conn, buf)
		conn.Close()
	}

	// dead backend must  be evicted.
	if reg.Get(dead).Admits() {
		t.Errorf("dead backend still admits traffic after %d connections; want evicted", 15)
	}
	// live ones must still be healthy.
	if !reg.Get(live1).Admits() || !reg.Get(live2).Admits() {
		t.Error("a live backend was wrongly evicted")
	}

}

// A refused dial must be retried on another backend, so the client never notices the dead one
// Fall is high so the dead backend stays in the rotation and every round-robin cycle hits it.
func TestHandle_RetriesDialOnAnotherBackend(t *testing.T) {
	live, stopLive := echoBackend(t)
	defer stopLive()

	deadLn, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := deadLn.Addr().String()
	deadLn.Close()

	cfg := &config.Config{
		Mode:   config.ModeL4,
		Listen: "127.0.0.1:0",
		Health: config.Health{Passive: config.PassiveHealth{Fall: 100}},
		Timeouts: config.Timeouts{
			Dial:  config.Duration(200 * time.Millisecond),
			Drain: config.Duration(time.Second),
		},
		Pools: map[string][]config.Backend{
			"default": {
				{Addr: live, Weight: 1},
				{Addr: dead, Weight: 1},
			},
		},
	}
	reg := health.NewRegistry()
	reg.Reconcile(cfg.BackendAddrs())

	bal, err := balancer.New(config.AlgRoundRobin, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Addr: cfg.Listen, Store: config.NewStore(cfg), Balancer: bal, Registry: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.ShutDown(ctx)
	})

	for i := 0; i < 6; i++ {
		if !roundTrip(srv.Addr().String(), []byte("ping\n")) {
			t.Errorf("connection %d failed; want it retried on the live backend", i)
		}
	}
}
func TestServer_MaxConnections(t *testing.T) {
	backendAddr, stopBackend := echoBackend(t)
	defer stopBackend()

	cfg := &config.Config{
		Mode:           config.ModeL4,
		Listen:         "127.0.0.1:0",
		MaxConnections: 1,
		Timeouts: config.Timeouts{
			Dial:  config.Duration(time.Second),
			Drain: config.Duration(time.Second),
		},
		Pools: map[string][]config.Backend{
			"default": {{Addr: backendAddr, Weight: 1}},
		},
	}
	reg := health.NewRegistry()
	reg.Reconcile(cfg.BackendAddrs())
	bal, _ := balancer.New(config.AlgRoundRobin, reg)

	srv := New(Options{Addr: cfg.Listen, Store: config.NewStore(cfg), Balancer: bal, Registry: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.ShutDown(ctx)
	})
	front := srv.Addr().String()

	// first connection: held open, must work
	first, err := net.Dial("tcp", front)
	if err != nil {
		t.Fatal(err)
	}
	first.SetDeadline(time.Now().Add(2 * time.Second))
	first.Write([]byte("ping\n"))
	if _, err := io.ReadFull(first, make([]byte, 5)); err != nil {
		t.Fatalf("first connection: %v", err)
	}

	// second connection: over the limit, must be refused
	if roundTrip(front, []byte("ping\n")) {
		t.Error("second connection was served; want it refused at max_connections=1")
	}

	// close the first; a new connection must be accepted again
	first.Close()
	deadline := time.Now().Add(time.Second)
	for !roundTrip(front, []byte("ping\n")) {
		if time.Now().After(deadline) {
			t.Fatal("no connection accepted after the first one closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
