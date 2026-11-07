package balancer

import (
	"sync"

	"github.com/Sachinxmpl/loadgate/internal/config"
)

type Pools struct {
	alg     config.Algorithm
	counter ConnCounter

	mu     sync.Mutex
	byName map[string]Balancer
}

func NewPools(alg config.Algorithm, counter ConnCounter) (*Pools, error) {
	if _, err := New(alg, counter); err != nil {
		return nil, err
	}
	return &Pools{
		alg:     alg,
		counter: counter,
		byName:  make(map[string]Balancer),
	}, nil
}

// Returns the balancer for pool, (lazy creation)
func (p *Pools) For(pool string) Balancer {
	p.mu.Lock()
	defer p.mu.Unlock()

	b, ok := p.byName[pool]
	if !ok {
		b, _ = New(p.alg, p.counter)
		p.byName[pool] = b
	}
	return b
}
