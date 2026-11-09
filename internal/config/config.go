package config

import (
	"crypto/tls"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Mode string

const (
	ModeL4 Mode = "l4"
	ModeL7 Mode = "l7"
)

type Algorithm string

const (
	AlgRoundRobin     Algorithm = "round_robin"
	AlgWeightedRR     Algorithm = "weighted_round_robin"
	AlgLeastConns     Algorithm = "least_connections"
	AlgConsistentHash Algorithm = "consistent_hash"
)

type Duration time.Duration

func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

func (d Duration) String() string {
	return time.Duration(d).String()
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be string such as %q: %w", "2s", err)
	}
	if s == "" {
		return fmt.Errorf("duration must be a non-empty string")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// CIDR is an IP range from the config, written as "10.0.0.0/8" or as a bare IP ("192.0.2.5"), which means just that one address
type CIDR struct {
	netip.Prefix
}

func (c *CIDR) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("must be a string such as %q: %w", "10.0.0.0/8", err)
	}
	if !strings.Contains(s, "/") {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("invalid IP %q", s)
		}
		c.Prefix = netip.PrefixFrom(addr, addr.BitLen())
		return nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return fmt.Errorf("invalid CIDR %q", s)
	}
	c.Prefix = p.Masked()
	return nil
}

// Whole configuration
type Config struct {
	Mode      Mode      `yaml:"mode"`
	Listen    string    `yaml:"listen"`
	Balancer  Algorithm `yaml:"balancer"`
	TLS       *TLS      `yaml:"tls"`
	Timeouts  Timeouts  `yaml:"timeouts"`
	Health    Health    `yaml:"health"`
	RateLimit RateLimit `yaml:"rate_limit"`

	// L4 only. 0 means unlimited. L7 relies on its read/idle timeouts
	MaxConnections int `yaml:"max_connections"`

	// L7 only. Empty = trust no one
	TrustedProxies []CIDR `yaml:"trusted_proxies"`

	Routes []Route `yaml:"routes"`

	Pools map[string][]Backend `yaml:"pools"`

	TLSCert *tls.Certificate `yaml:"-"`
}

type TLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

type Timeouts struct {
	Dial           Duration `yaml:"dial"`
	Read           Duration `yaml:"read"`
	Write          Duration `yaml:"write"`
	Idle           Duration `yaml:"idle"`
	Request        Duration `yaml:"request"`
	ResponseHeader Duration `yaml:"response_header"`
	Drain          Duration `yaml:"drain"`
}

type Health struct {
	Active  ActiveHealth  `yaml:"active"`
	Passive PassiveHealth `yaml:"passive"`
}

type ActiveHealth struct {
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
	Rise     int      `yaml:"rise"`

	// HTTP path to GET, eg "/health", a 2xx is a pass
	// Empty -> only check TCP connect succeeds
	Path string `yaml:"path"`

	// Failed probes in a row that evict a healthy backend
	// 0 -> probes never evict a backend, only passive health does
	Fall int `yaml:"fall"`
}

type PassiveHealth struct {
	Fall     int      `yaml:"fall"`
	Cooldown Duration `yaml:"cooldown"`

	// l7 only, count 5xx responses as failures for passive health
	FailOn5xx bool `yaml:"fail_on_5xx"`
}

type RateLimit struct {
	GlobalRPS    int `yaml:"global_rps"`
	PerClientRPS int `yaml:"per_client_rps"`
}

type Route struct {
	Match RouteMatch `yaml:"match"`
	Pool  string     `yaml:"pool"`
}

type RouteMatch struct {
	PathPrefix string `yaml:"path_prefix"`
}

type Backend struct {
	Addr   string `yaml:"addr"`
	Weight int    `yaml:"weight"`
}

// Returns every backend across every pool, in a deterministic order (pools sorted by name, backends in file order).
func (c *Config) AllBackends() []Backend {
	names := make([]string, 0, len(c.Pools))
	for name := range c.Pools {
		names = append(names, name)
	}
	sort.Strings(names)

	count := 0
	for _, backends := range c.Pools {
		count += len(backends)
	}

	out := make([]Backend, 0, count)
	for _, name := range names {
		out = append(out, c.Pools[name]...)
	}
	return out
}

// Returns backends serving in l4 mode.
// return on first iteration -> validation confirms there is exactly one entry
func (c *Config) L4Pool() []Backend {
	for _, backends := range c.Pools {
		return backends
	}
	return nil
}

// Returns the address of every backend across every pool
func (c *Config) BackendAddrs() []string {
	backends := c.AllBackends()
	addrs := make([]string, len(backends))

	for i, be := range backends {
		addrs[i] = be.Addr
	}

	return addrs
}

// IsTrustedProxy reports whether ip is inside one of trusted_proxies
func (c *Config) IsTrustedProxy(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range c.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
