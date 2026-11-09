package health

import (
	"sync"
	"sync/atomic"
	"time"
)

type Phase int32

const (
	Healthy Phase = iota
	Evicted
	Cooldown
	Probation
)

func (p Phase) String() string {
	switch p {
	case Healthy:
		return "healthy"
	case Evicted:
		return "evicted"
	case Cooldown:
		return "cooldown"
	case Probation:
		return "probation"
	default:
		return "unknown"
	}
}

type State struct {
	phase atomic.Int32

	mu         sync.Mutex
	fails      int // passive: real-traffic failures in a row
	probeFails int // active: failed probes in a row while healthy
	successes  int
	evictedAt  time.Time

	conns atomic.Int64
}

// Reports if BE is healthy
// no mutex as -> called frequently every conn (hot path)
func (s *State) Admits() bool {
	return Phase(s.phase.Load()) == Healthy
}

func (s *State) Phase() Phase {
	return Phase(s.phase.Load())
}

func (s *State) AddConn() {
	s.conns.Add(1)
}

func (s *State) RemoveConn() {
	s.conns.Add(-1)
}

func (s *State) Conns() int64 {
	return s.conns.Load()
}

// Passive Path
// Records that customer request to this be failed
// After 'fall' consecutive failures a Healthy backed is evicted
func (s *State) ReportFailure(fall int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if Phase(s.phase.Load()) != Healthy {
		return
	}

	s.fails++
	if s.fails >= fall {
		s.evictLocked()
	}
}

func (s *State) evictLocked() {
	s.phase.Store(int32(Evicted))
	s.evictedAt = time.Now()
	s.fails = 0
	s.probeFails = 0
}

// Passive path
// Resets fails counter if be found healthy
func (s *State) ReportSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = 0
}

// Moves EVICTED BE to Cooldown once 'cooldown' time passes
func (s *State) BeginProbation(cooldown time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch Phase(s.phase.Load()) {
	case Evicted:
		if time.Since(s.evictedAt) >= cooldown {
			s.phase.Store(int32(Cooldown))
			s.successes = 0
			return true
		}
		return false
	case Cooldown, Probation:
		return true
	default:
		return false
	}
}

// feeds the outcome of one active probe into the state machine
// BE is promoted Cooldown -> Probation on first success
// Probation -> Healthy after 'rise' consecutive success
// Any failures drops it back to Cooldown
func (s *State) ProbeResult(ok bool, rise int, fall int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	phase := Phase(s.phase.Load())

	if phase == Healthy {
		if fall == 0 {
			return
		}
		if ok {
			s.probeFails = 0
			return
		}
		s.probeFails++
		if s.probeFails >= fall {
			s.evictLocked()
		}
		return
	}

	if phase != Cooldown && phase != Probation {
		return
	}

	if !ok {
		s.phase.Store(int32(Cooldown))
		s.successes = 0
		return
	}

	s.successes++
	if s.successes >= rise {
		s.phase.Store(int32(Healthy))
		s.successes = 0
		s.fails = 0
		return
	}

	// not enough successes
	s.phase.Store(int32(Probation))
}
