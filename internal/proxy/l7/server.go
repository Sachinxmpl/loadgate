package l7

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Sachinxmpl/loadgate/internal/balancer"
	"github.com/Sachinxmpl/loadgate/internal/config"
	"github.com/Sachinxmpl/loadgate/internal/health"
	"github.com/Sachinxmpl/loadgate/internal/metrics"
	"github.com/Sachinxmpl/loadgate/internal/ratelimit"
)

// readHeaderTimeout bounds how long a client may take to send its request headers.
// Prevents slowloris attacks.
const (
	readHeaderTimeout = 5 * time.Second
	maxAttempts       = 3 // total tries across distinct healthy backends
)

type Server struct {
	addr      string
	store     *config.Store
	balancers *balancer.Pools
	registry  *health.Registry
	log       *slog.Logger
	limiter   *ratelimit.Limiter

	httpSrv *http.Server
	ln      net.Listener

	transport *http.Transport
	metrics   *metrics.Metrics
}

type Options struct {
	Config    *config.Config
	Store     *config.Store
	Balancers *balancer.Pools
	Registry  *health.Registry
	Limiter   *ratelimit.Limiter
	Log       *slog.Logger
	Metrics   *metrics.Metrics
}

func New(o Options) *Server {
	s := &Server{
		addr:      o.Config.Listen,
		store:     o.Store,
		balancers: o.Balancers,
		registry:  o.Registry,
		limiter:   o.Limiter,
		log:       o.Log,
		metrics:   o.Metrics,
	}

	s.httpSrv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       o.Config.Timeouts.Read.Std(),
		WriteTimeout:      o.Config.Timeouts.Write.Std(),
		IdleTimeout:       o.Config.Timeouts.Idle.Std(),
		ErrorLog:          slog.NewLogLogger(o.Log.Handler(), slog.LevelWarn),
	}

	s.transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: o.Config.Timeouts.Dial.Std()}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       15 * time.Second,
		ResponseHeaderTimeout: o.Config.Timeouts.ResponseHeader.Std(),
		ForceAttemptHTTP2:     false,
	}

	return s
}

// Binds the listen addr and serves HTTP in background
// like l4, bind failure is returned synchronously before any goroutine exists
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}

	scheme := "http"
	if s.store.Load().TLSCert != nil {
		ln = tls.NewListener(ln, &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				return s.store.Load().TLSCert, nil
			},
		})
		scheme = "https"
	}
	s.ln = ln

	//exists when: Shutdown (or close ) stops the server making Serve return
	// http.ErrServerClosed
	go func(ln net.Listener) {
		err := s.httpSrv.Serve(ln)
		if err != nil && err != http.ErrServerClosed {
			s.log.Error("http server stopped unexpectedly", "err", err)
		}
	}(ln)

	s.log.Info("listening", "addr", ln.Addr().String(), "mode", "l7", "scheme", scheme)

	return nil
}

// Routes one request to a pool by path, picks a healthy be, and forwards request
// Follows 4 http proxy rules
// Clear RequestURI, strip hop-by-hop header, append X-Forwarded-For, Stream the body
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if s.limiter != nil && !s.limiter.Allow(clientIP(r)) {
		s.metrics.RateLimitRejected()
		s.log.Debug("rate limited", "client", clientIP(r))
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	cfg := s.store.Load()

	// websocket/upgrade -> needs raw byte piping
	if r.Header.Get("Upgrade") != "" {
		s.log.Debug("upgrade request rejected in l7 mode", "client", clientIP(r))
		http.Error(w, "upgrade not supported in l7 mode, use l4", http.StatusNotImplemented)
		return
	}

	poolName, ok := route(cfg, r.URL.Path)
	if !ok {
		s.log.Warn("no route for path", "path", r.URL.Path, "client", clientIP(r))
		http.Error(w, "no route for path", http.StatusNotFound)
		return
	}

	// Try up to maxAttempts distinct healthy backends.
	// Retry happens only for a connection-level failure on an idempotent method, and only before any response byte has been written back to the client
	tried := make(map[string]bool)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Client already hung up
		if r.Context().Err() != nil {
			return
		}
		pool := balancer.Untried(balancer.HealthyBackends(cfg.Pools[poolName], s.registry), tried)

		backend, err := s.balancers.For(poolName).Pick(clientIP(r), pool)
		if err != nil {
			status := http.StatusServiceUnavailable
			if len(tried) > 0 {
				status = http.StatusBadGateway
			}
			s.log.Warn("no healthy backend to try", "pool", poolName, "path", r.URL.Path, "tried", len(tried))
			http.Error(w, http.StatusText(status), status)
			s.metrics.RequestObserved("none", status, time.Since(start).Seconds())
			return
		}
		tried[backend.Addr] = true

		last := attempt == maxAttempts-1
		if s.forwardOnce(w, r, cfg, backend, start, last) == resultDone {
			return
		}
	}
}

type attemptResult int

const (
	resultDone attemptResult = iota
	resultRetry
)

func (s *Server) forwardOnce(w http.ResponseWriter, r *http.Request, cfg *config.Config, backend *config.Backend, start time.Time, last bool) attemptResult {
	st := s.registry.Get(backend.Addr)
	st.AddConn()
	defer st.RemoveConn()

	ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeouts.Request.Std())
	defer cancel()
	out := r.Clone(ctx)
	out.RequestURI = ""
	out.URL.Scheme = "http"
	out.URL.Host = backend.Addr
	out.Host = r.Host

	stripHopByHop(out.Header)
	ip := clientIP(r)
	setXForwardedFor(out, ip, cfg.IsTrustedProxy(ip))

	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		if r.Context().Err() != nil {
			return resultDone
		}

		s.reportFailure(st, backend, cfg)
		s.log.Warn("backend request failed", "backend", backend.Addr, "path", r.URL.Path, "final", last, "err", err)

		if !last && isIdempotent(r.Method) {
			return resultRetry
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
		s.metrics.RequestObserved(backend.Addr, http.StatusBadGateway, time.Since(start).Seconds())
		return resultDone

	}
	defer resp.Body.Close()

	st.ReportSuccess()
	stripHopByHop(resp.Header)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if err := copyBody(w, resp.Body, resp.ContentLength == -1); err != nil {
		s.log.Debug("copying response body", "backend", backend.Addr, "err", err)
	}

	s.metrics.RequestObserved(backend.Addr, resp.StatusCode, time.Since(start).Seconds())
	return resultDone
}

// Copies the backend's body to the client.
// When stream is true the body has no known length (SSE, chunked), so each piece is flushed to the client as soon as it arrives
func copyBody(w http.ResponseWriter, body io.Reader, stream bool) error {
	if !stream {
		_, err := io.Copy(w, body)
		return err
	}

	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if ferr := rc.Flush(); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) reportFailure(st *health.State, backend *config.Backend, cfg *config.Config) {
	before := st.Phase()
	st.ReportFailure(cfg.Health.Passive.Fall)
	after := st.Phase()
	if after == before {
		return
	}
	s.metrics.HealthTransition(backend.Addr, after.String())
	if after == health.Evicted {
		s.transport.CloseIdleConnections()
	}
}

// isIdempotent reports whether a method is safe to replay on another backend.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// Finds the poll for path using longest-prefix match
func route(cfg *config.Config, path string) (pool string, ok bool) {
	best := -1
	for _, r := range cfg.Routes {
		p := r.Match.PathPrefix
		if strings.HasPrefix(path, p) && len(p) > best {
			best = len(p)
			pool = r.Pool
			ok = true
		}
	}
	return pool, ok
}

// related to a single connection
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Removes connection-scoped header, including any header names inside the Connection Header
func stripHopByHop(h http.Header) {
	for _, name := range h["Connection"] {
		for tok := range strings.SplitSeq(name, ",") {
			tok = strings.TrimSpace(tok)
			if tok != "" {
				h.Del(tok)
			}
		}
	}

	for _, name := range hopByHop {
		h.Del(name)
	}
}

// sets X-Forwarded-For to the forwarding chain plus clientIP.
// The incoming chain (from every X-Forwarded-For line) is kept only when the direct peer
// is a trusted proxy; from anyone else it could be made up, so it is dropped
func setXForwardedFor(out *http.Request, clientIP string, trusted bool) {
	var chain []string
	if trusted {
		chain = append(chain, out.Header.Values("X-Forwarded-For")...)
	}
	if clientIP != "" {
		chain = append(chain, clientIP)
	}

	out.Header.Del("X-Forwarded-For")
	if len(chain) > 0 {
		out.Header.Set("X-Forwarded-For", strings.Join(chain, ", "))
	}
}

// Copies every header value from src to desk
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Addr reports the address actually bound (differs from the configured address
// when the config asks for port 0). Valid only after a successful Start.
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

// stops accepting new requests and wait for in-flight ones to finished
// http.Server.Shudown() already implements drain
func (s *Server) ShutDown(ctx context.Context) error {
	s.log.Info("shutting down l7 server")
	err := s.httpSrv.Shutdown(ctx)
	s.transport.CloseIdleConnections()
	return err
}
