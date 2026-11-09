package balancer

import (
	"errors"
	"fmt"

	"github.com/Sachinxmpl/loadgate/internal/config"
	"github.com/Sachinxmpl/loadgate/internal/health"
)

var ErrNoBackends = errors.New("no backends available")

type Balancer interface {

	// Takes input pool(Already filtered by caller as healthy)
	// Returns a pointer aliases pool's backing array
	// no copying
	// callers load snapshot once per connection, hold it until conn ends (never modified) -> safe to return pointer into slices
	// key  -> identifies the client (clientIp in L4, header in l7). Statesless algo's simply ignores this
	Pick(key string, pool []config.Backend) (*config.Backend, error)
}

func New(alg config.Algorithm, counter ConnCounter) (Balancer, error) {
	switch alg {
	case config.AlgRoundRobin:
		return &RoundRobin{}, nil
	case config.AlgWeightedRR:
		return NewWeightedRoundRobin(), nil
	case config.AlgLeastConns:
		if counter == nil {
			return nil, fmt.Errorf("balancer %q: needs a connection counter", alg)
		}
		return NewLeastConnections(counter), nil
	case config.AlgConsistentHash:
		return NewConsistentHash(defaultVNodes), nil
	default:
		return nil, fmt.Errorf("balancer %q: unknown", alg)
	}
}

// Untried returns the backends in pool whose address is not in tried.
// Used by retry loops so each attempt goes to a different backend.
func Untried(pool []config.Backend, tried map[string]bool) []config.Backend {
	if len(tried) == 0 {
		return pool
	}
	out := make([]config.Backend, 0, len(pool))
	for _, b := range pool {
		if !tried[b.Addr] {
			out = append(out, b)
		}
	}
	return out
}

// Returns the backends in pool that are healthy, if none are healthy, it returns whe whole pool (fail open)
func HealthyBackends(pool []config.Backend, reg *health.Registry) []config.Backend {
	out := make([]config.Backend, 0, len(pool))
	for _, b := range pool {
		if reg.Get(b.Addr).Admits() {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return pool
	}
	return out
}
