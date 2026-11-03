package listener

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sachinxmpl/loadgate/internal/balancer"
	"github.com/Sachinxmpl/loadgate/internal/config"
	"github.com/Sachinxmpl/loadgate/internal/health"
	"github.com/Sachinxmpl/loadgate/internal/metrics"
	"github.com/Sachinxmpl/loadgate/internal/proxy"
	"github.com/Sachinxmpl/loadgate/internal/ratelimit"
)

const (
	acceptBackOffMin = 5 * time.Millisecond
	acceptBackOfMax  = time.Second
	maxDialAttempts  = 3 // total tries across distinct healthy backends
)

type Server struct {
	addr     string
	store    *config.Store
	balancer balancer.Balancer
	registry *health.Registry
	log      *slog.Logger

	limiter *ratelimit.Limiter
	metrics *metrics.Metrics

	ln net.Listener
	wg sync.WaitGroup

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	shutdown bool

	connID atomic.Uint64
}

type Options struct {
	Addr     string
	Store    *config.Store
	Balancer balancer.Balancer
	Registry *health.Registry
	Limiter  *ratelimit.Limiter
	Log      *slog.Logger
	Metrics  *metrics.Metrics
}

func New(o Options) *Server {
	return &Server{
		addr:     o.Addr,
		store:    o.Store,
		balancer: o.Balancer,
		registry: o.Registry,
		log:      o.Log,
		limiter:  o.Limiter,
		conns:    make(map[net.Conn]struct{}),
		metrics:  o.Metrics,
	}
}

// binds listen address and begins accepting connections in background
// a bind failure is reported synchronously before any goroutine exists
// Every successful start -> paired with a shutdown
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	s.ln = ln

	s.wg.Add(1)

	// exists when -> Shutdown closes the listener, this makes Accept return net.ErrClosed
	go func() {
		defer s.wg.Done()
		s.acceptLoop()
	}()

	s.log.Info("listening", "addr", ln.Addr().String())

	return nil
}

// Addr reports the address actually bound (differs from the configured address
// when the config asks for port 0). Valid only after a successful Start.
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

func (s *Server) acceptLoop() {
	backoff := acceptBackOffMin

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			// listener closed by Shutdown -> only way out of loop, it not a failure
			if errors.Is(err, net.ErrClosed) {
				return
			}

			// anything else -> process most liekly out of file descriptors, try exponentially
			s.log.Warn("accept failed, retrying", "err", err, "backoff", backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, acceptBackOfMax)
			continue
		}
		backoff = acceptBackOffMin

		// Shutdown may have started between accept returning and here,
		if !s.track(conn) {
			conn.Close()
			continue
		}

		s.wg.Add(1)

		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.handle(conn)
		}()
	}
}

// register conn as live, reports false if shutdown has already begun (caller must conn itself)
func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.shutdown {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

// Serves one client connection: pick a healthy backend, dial it , and replay bytes both ways until connection ends
// exists when proxy.L4 returns, which its drain deadline bounds
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()

	log := s.log.With(
		"conn_id", s.connID.Add(1),
		"client", conn.RemoteAddr().String(),
	)

	if s.limiter != nil && !s.limiter.Allow(clientKey(conn)) {
		log.Debug("rate limited")
		s.metrics.RateLimitRejected()
		return
	}

	cfg := s.store.Load()

	backend, up := s.dialBackend(clientKey(conn), cfg, log)
	if up == nil {
		return
	}
	log = log.With("backend", backend.Addr)
	log.Debug("l4 backend selected")

	st := s.registry.Get(backend.Addr)
	st.ReportSuccess()
	st.AddConn()
	defer st.RemoveConn()

	proxy.L4(conn, up, cfg.Timeouts.Drain.Std())
}

// Picks and dials up to maxDialAttempts distinct healthy backends and returns the first one that connects, or nil if none did.
// Retry in l4 - safe -> a failed dial means no client byte has reached any backend.
func (s *Server) dialBackend(key string, cfg *config.Config, log *slog.Logger) (*config.Backend, net.Conn) {
	tried := make(map[string]bool)

	for attempt := range maxDialAttempts {
		pool := balancer.Untried(balancer.HealthyBackends(cfg.L4Pool(), s.registry), tried)

		backend, err := s.balancer.Pick(key, pool)
		if err != nil {
			log.Warn("no backend for connection", "tried", len(tried), "err", err)
			return nil, nil
		}
		tried[backend.Addr] = true

		up, err := net.DialTimeout("tcp", backend.Addr, cfg.Timeouts.Dial.Std())
		if err == nil {
			return backend, up
		}

		st := s.registry.Get(backend.Addr)
		before := st.Phase()
		st.ReportFailure(cfg.Health.Passive.Fall)
		if after := st.Phase(); after != before {
			s.metrics.HealthTransition(backend.Addr, after.String())
		}
		log.Warn("dial backend failed", "backend", backend.Addr, "attempt", attempt+1, "err", err)
	}

	log.Warn("all dial attempts failed", "tried", len(tried))
	return nil, nil
}

func clientKey(conn net.Conn) string {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

// Stops accepting new connections and waits for the live ones to finish
// if ctx epires first remaining connections are force-closed and ctx.Err() returned
func (s *Server) ShutDown(ctx context.Context) error {
	s.log.Info("shutting down l4 listener")
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return nil
	}
	s.shutdown = true
	s.mu.Unlock()

	// close lisener
	closeErr := s.ln.Close()

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		return closeErr
	case <-ctx.Done():
		s.closeAll()
		<-drained
		return ctx.Err()
	}
}

func (s *Server) closeAll() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()

	if len(conns) == 0 {
		return
	}

	s.log.Warn("drain deadline expired, force-closing connections", "count", len(conns))

	for _, conn := range conns {
		conn.Close()
	}
}
