package ratelimit

import (
	"sync"

	"golang.org/x/time/rate"
)

const maxClients = 10_000

type Limiter struct {
	global *rate.Limiter

	perClientRPS   rate.Limit
	perClientBurst int

	mu      sync.Mutex
	clients *lru
}

func New(globalRPS, perClientRPS int) *Limiter {
	l := &Limiter{clients: newLRU(maxClients)}
	if globalRPS > 0 {
		l.global = rate.NewLimiter(rate.Limit(globalRPS), globalRPS)
	}
	if perClientRPS > 0 {
		l.perClientRPS = rate.Limit(perClientRPS)
		l.perClientBurst = perClientRPS
	}
	return l
}

func (l *Limiter) Allow(clientIP string) bool {
	// client's own bucket first
	if l.perClientRPS > 0 {
		l.mu.Lock()
		cl := l.clients.get(clientIP, func() *rate.Limiter {
			return rate.NewLimiter(l.perClientRPS, l.perClientBurst)
		})
		l.mu.Unlock()
		if !cl.Allow() {
			return false
		}
	}
	return l.global == nil || l.global.Allow()
}
