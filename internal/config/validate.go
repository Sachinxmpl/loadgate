package config

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

func (c *Config) Validate() error {
	switch c.Mode {
	case ModeL4, ModeL7:
	case "":
		return fmt.Errorf("mode: required (%q or %q)", ModeL4, ModeL7)
	default:
		return fmt.Errorf("mode: must be %q or %q, got %q", ModeL4, ModeL7, c.Mode)
	}

	if c.Listen == "" {
		return errors.New("listen: required")
	}

	// empty host -> fine, ":8080" -> all interfaces 0.0.0.0
	if err := validateHostPort(c.Listen, false); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	switch c.Balancer {
	case AlgRoundRobin, AlgWeightedRR, AlgLeastConns, AlgConsistentHash:
	default:
		return fmt.Errorf("balancer: unknown algorithm %q", c.Balancer)
	}

	if err := c.Timeouts.validate(); err != nil {
		return err
	}
	if err := c.Health.validate(); err != nil {
		return err
	}
	if err := c.RateLimit.validate(); err != nil {
		return err
	}
	if c.MaxConnections < 0 {
		return fmt.Errorf("max_connections: must be 0 (unlimited) or more, got %d", c.MaxConnections)
	}
	if err := c.validatePools(); err != nil {
		return err
	}
	if err := c.validateModeKeys(); err != nil {
		return err
	}
	if err := c.validateRoutes(); err != nil {
		return err
	}
	if c.TLS != nil {
		if err := c.TLS.validate(); err != nil {
			return err
		}
	}

	return nil
}

func validateHostPort(addr string, requireHost bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("invalid address %q: port is required", addr)
	}
	if requireHost && host == "" {
		return fmt.Errorf("invalid address %q: host is required", addr)
	}
	return nil
}

func (t Timeouts) validate() error {
	fields := []struct {
		name string
		d    Duration
	}{
		{"dial", t.Dial},
		{"read", t.Read},
		{"write", t.Write},
		{"idle", t.Idle},
		{"request", t.Request},
		{"response_header", t.ResponseHeader},
		{"drain", t.Drain},
	}
	for _, f := range fields {
		if f.d <= 0 {
			return fmt.Errorf("timeouts.%s: must be greater than 0, got %v", f.name, f.d)
		}
	}
	return nil
}

func (h Health) validate() error {
	if h.Active.Interval <= 0 {
		return fmt.Errorf("health.active.interval: must be greater than 0, got %v",
			h.Active.Interval)
	}
	if h.Active.Timeout <= 0 {
		return fmt.Errorf("health.active.timeout: must be greater than 0, got %v",
			h.Active.Timeout)
	}
	// A probe allowed to outlast its own interval means probes pile up on a
	// slow backend instead of reporting failure.
	if h.Active.Timeout >= h.Active.Interval {
		return fmt.Errorf("health.active.timeout: must be less than health.active.interval (%v), got %v",
			h.Active.Interval, h.Active.Timeout)
	}
	if h.Active.Path != "" && !strings.HasPrefix(h.Active.Path, "/") {
		return fmt.Errorf("health.active.path: must start with \"/\", got %q", h.Active.Path)
	}
	if h.Active.Rise < 1 {
		return fmt.Errorf("health.active.rise: must be at least 1, got %d", h.Active.Rise)
	}
	if h.Passive.Fall < 1 {
		return fmt.Errorf("health.passive.fall: must be at least 1, got %d", h.Passive.Fall)
	}
	if h.Passive.Cooldown <= 0 {
		return fmt.Errorf("health.passive.cooldown: must be greater than 0, got %v",
			h.Passive.Cooldown)
	}
	return nil
}

func (r RateLimit) validate() error {
	// Zero is allowed, means unlimited negative is not.
	if r.GlobalRPS < 0 {
		return fmt.Errorf("rate_limit.global_rps: must not be negative, got %d", r.GlobalRPS)
	}
	if r.PerClientRPS < 0 {
		return fmt.Errorf("rate_limit.per_client_rps: must not be negative, got %d", r.PerClientRPS)
	}
	return nil
}

func (t TLS) validate() error {
	if t.Cert == "" {
		return errors.New("tls.cert: required when a tls block is present")
	}
	if t.Key == "" {
		return errors.New("tls.key: required when a tls block is present")
	}
	return nil
}

func (c *Config) validatePools() error {
	if len(c.Pools) == 0 {
		return errors.New("pools: at least one pool is required")
	}

	// sorting to make sure map iteration is not random
	for _, name := range c.sortedPoolNames() {
		pool := c.Pools[name]
		if len(pool) == 0 {
			return fmt.Errorf("pools.%s: must contain at least one backend", name)
		}

		seen := make(map[string]int, len(pool))
		for i, b := range pool {
			field := fmt.Sprintf("pools.%s[%d]", name, i)

			if b.Addr == "" {
				return fmt.Errorf("%s.addr: required", field)
			}

			// A backend must name a real host: ":8080", host = true
			if err := validateHostPort(b.Addr, true); err != nil {
				return fmt.Errorf("%s.addr: %w", field, err)
			}
			if first, dup := seen[b.Addr]; dup {
				return fmt.Errorf("%s.addr: duplicate address %q (already used at index %d)",
					field, b.Addr, first)
			}
			seen[b.Addr] = i

			if b.Weight < 1 {
				return fmt.Errorf("%s.weight: must be at least 1, got %d", field, b.Weight)
			}
		}
	}
	return nil
}

// reject settings that belong to other mode
func (c *Config) validateModeKeys() error {
	if c.Mode == ModeL4 {
		if c.Routes != nil {
			return errors.New("routes: not allowed in l4 mode (routing required l7)")
		}
		if c.TLS != nil {
			return errors.New("tls: not allowed in l4 mode (TLS termination requires l7)")
		}
		if len(c.TrustedProxies) > 0 {
			return errors.New("trusted_proxies: only supported in l7 mode (l4 does not read headers)")
		}
		if len(c.Pools) != 1 {
			return fmt.Errorf("pools: l4 mode requires exactly one pool, got %d", len(c.Pools))
		}
	}
	if c.Mode == ModeL7 && len(c.Routes) == 0 {
		return errors.New("routes: at least one route is required in l7 mode")
	}
	if c.Mode == ModeL7 && c.MaxConnections > 0 {
		return errors.New("max_connections: only supported in l4 mode (l7 closes idle clients with its read/idle timeouts)")
	}
	return nil
}

func (c *Config) validateRoutes() error {
	for i, r := range c.Routes {
		field := fmt.Sprintf("routes[%d]", i)

		if r.Pool == "" {
			return fmt.Errorf("%s.pool: required", field)
		}
		if _, ok := c.Pools[r.Pool]; !ok {
			return fmt.Errorf("%s.pool: no pool named %q (known pools: %v)",
				field, r.Pool, c.sortedPoolNames())
		}
		// An empty path_prefix is allowed: it is the catch-all rule.
	}
	return nil
}

func (c *Config) sortedPoolNames() []string {
	names := make([]string, 0, len(c.Pools))
	for name := range c.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
