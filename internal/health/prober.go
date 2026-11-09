package health

import (
	"context"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Sachinxmpl/loadgate/internal/config"
)

// Manager runs one prober goroutine per backend.
// Prober is only thing that can move a backend back toward healthy

type Manager struct {
	reg *Registry
	log *slog.Logger

	interval time.Duration
	timeout  time.Duration
	rise     int
	fall     int
	cooldown time.Duration

	// HTTP probe; path == "" means TCP-connect probe only
	path   string
	client *http.Client

	// Is called whenever a probe moves a backend to a new phase
	// Wired to metrics in server.go
	onTransition func(addr, phase string)

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	stopped bool
	wg      sync.WaitGroup
}

func NewManager(reg *Registry, h config.Health, log *slog.Logger, onTransition func(addr, phase string)) *Manager {
	return &Manager{
		reg:      reg,
		log:      log,
		interval: h.Active.Interval.Std(),
		timeout:  h.Active.Timeout.Std(),
		rise:     h.Active.Rise,
		fall:     h.Active.Fall,
		cooldown: h.Passive.Cooldown.Std(),
		path:     h.Active.Path,
		client: &http.Client{
			Transport: &http.Transport{DisableKeepAlives: true},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		onTransition: onTransition,
		cancels:      make(map[string]context.CancelFunc),
	}
}

// Starts probers for newly added backends and cancels probers for removed ones
// takes the diff Registry.Reconcile returns
func (m *Manager) Sync(added, removed []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopped {
		return
	}

	for _, addr := range removed {
		if cancel, ok := m.cancels[addr]; ok {
			m.log.Info("stopping health prober", "backend", addr)
			cancel()
			delete(m.cancels, addr)
		}
	}

	for _, addr := range added {
		if _, ok := m.cancels[addr]; ok {
			continue
		}
		m.log.Info("starting health prober", "backend", addr)
		ctx, cancel := context.WithCancel(context.Background())
		m.cancels[addr] = cancel

		st := m.reg.Get(addr)

		m.wg.Add(1)

		go func(ctx context.Context, addr string, st *State) {
			defer m.wg.Done()
			m.probeLoop(ctx, addr, st)
		}(ctx, addr, st)
	}
}

// Probe every ticker with ctx (is cancelled)
func (m *Manager) probeLoop(ctx context.Context, addr string, st *State) {
	t := time.NewTicker(withJitter(m.interval))
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			before := st.Phase()
			admitted := st.BeginProbation(m.cooldown)
			m.reportTransition(addr, before, st.Phase())

			// Healthy backends are probed when active.fall is set
			watching := m.fall > 0 && st.Phase() == Healthy
			if !admitted && !watching {
				continue
			}
			before = st.Phase()
			ok := m.probeOnce(ctx, addr)
			st.ProbeResult(ok, m.rise, m.fall)
			m.reportTransition(addr, before, st.Phase())
			if before == Healthy && st.Phase() == Evicted {
				m.log.Warn("backend evicted by active probes", "backend", addr, "fall", m.fall)
			}

			if ok {
				m.log.Debug("probe ok", "backend", addr, "phase", st.Phase())
			} else {
				m.log.Debug("probe failed", "backend", addr, "phase", st.Phase())
			}
		}
	}
}

// Reports whether addr accepts a TCP connection within probe timeout
func (m *Manager) probeOnce(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if m.path == "" {
		return tcpProbe(ctx, addr)
	}
	return m.httpProbe(ctx, addr)
}

func tcpProbe(ctx context.Context, addr string) bool {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// passes if GET http://addr<path> answers 2xx
func (m *Manager) httpProbe(ctx context.Context, addr string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+m.path, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "loadgate-health")

	resp, err := m.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// returns d with +- 10%, so all probers don't start together
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	delta := float64(d) * 0.1
	offset := (rand.Float64()*2 - 1) * delta
	return time.Duration(float64(d) + offset)
}

func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	for addr, cancel := range m.cancels {
		m.log.Info("stopping health prober", "backend", addr)
		cancel()
		delete(m.cancels, addr)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *Manager) reportTransition(addr string, before, after Phase) {
	if before != after && m.onTransition != nil {
		m.onTransition(addr, after.String())
	}
}
