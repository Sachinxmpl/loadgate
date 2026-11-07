package config

import (
	"strings"
	"testing"
	"time"
)

// minimalConfig - sets only the keys that have no default
const minimalConfig = `
mode: l4
listen: "127.0.0.1:8080"
pools:
  default:
    - addr: "127.0.0.1:9001"
`

func TestParse_MinimalConfig(t *testing.T) {
	c, err := Parse(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatalf("minimal config should be valid: %v", err)
	}

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"balancer", c.Balancer, AlgRoundRobin},
		{"timeouts.dial", c.Timeouts.Dial.Std(), 2 * time.Second},
		{"timeouts.read", c.Timeouts.Read.Std(), 30 * time.Second},
		{"timeouts.write", c.Timeouts.Write.Std(), 30 * time.Second},
		{"timeouts.idle", c.Timeouts.Idle.Std(), 60 * time.Second},
		{"timeouts.request", c.Timeouts.Request.Std(), 30 * time.Second},
		{"timeouts.response_header", c.Timeouts.ResponseHeader.Std(), 10 * time.Second},
		{"timeouts.drain", c.Timeouts.Drain.Std(), 15 * time.Second},
		{"health.active.interval", c.Health.Active.Interval.Std(), 2 * time.Second},
		{"health.active.timeout", c.Health.Active.Timeout.Std(), 500 * time.Millisecond},
		{"health.active.rise", c.Health.Active.Rise, 2},
		{"health.passive.fall", c.Health.Passive.Fall, 3},
		{"health.passive.cooldown", c.Health.Passive.Cooldown.Std(), 10 * time.Second},
		{"pools.default[0].weight", c.Pools["default"][0].Weight, 1},
		{"rate_limit.global_rps", c.RateLimit.GlobalRPS, 0},
		{"rate_limit.per_client_rps", c.RateLimit.PerClientRPS, 0},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.field, ch.got, ch.want)
		}
	}

	if c.TLS != nil {
		t.Errorf("tls = %+v, want nil", c.TLS)
	}
	if c.Routes != nil {
		t.Errorf("routes = %v, want nil", c.Routes)
	}
}

// A wrong yaml tag compiles fine and silently yields a zero value, so every key
// is set to a distinctive value and read back.
func TestParse_FullConfig(t *testing.T) {
	const src = `
mode: l7
listen: ":8443"
balancer: consistent_hash
timeouts:
  dial: 1s
  read: 10s
  write: 11s
  idle: 12s
  request: 13s
  response_header: 5s
  drain: 14s
health:
  active:
    interval: 3s
    timeout: 250ms
    rise: 5
  passive:
    fall: 7
    cooldown: 30s
rate_limit:
  global_rps: 5000
  per_client_rps: 100
routes:
  - match:
      path_prefix: /api
    pool: api
  - match:
      path_prefix: ""
    pool: web
pools:
  api:
    - addr: "10.0.0.1:9001"
      weight: 3
    - addr: "10.0.0.2:9001"
  web:
    - addr: "10.0.0.3:9002"
      weight: 2
`
	c, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"mode", c.Mode, ModeL7},
		{"listen", c.Listen, ":8443"},
		{"balancer", c.Balancer, AlgConsistentHash},
		{"timeouts.dial", c.Timeouts.Dial.Std(), 1 * time.Second},
		{"timeouts.read", c.Timeouts.Read.Std(), 10 * time.Second},
		{"timeouts.write", c.Timeouts.Write.Std(), 11 * time.Second},
		{"timeouts.idle", c.Timeouts.Idle.Std(), 12 * time.Second},
		{"timeouts.request", c.Timeouts.Request.Std(), 13 * time.Second},
		{"timeouts.response_header", c.Timeouts.ResponseHeader.Std(), 5 * time.Second},
		{"timeouts.drain", c.Timeouts.Drain.Std(), 14 * time.Second},
		{"health.active.interval", c.Health.Active.Interval.Std(), 3 * time.Second},
		{"health.active.timeout", c.Health.Active.Timeout.Std(), 250 * time.Millisecond},
		{"health.active.rise", c.Health.Active.Rise, 5},
		{"health.passive.fall", c.Health.Passive.Fall, 7},
		{"health.passive.cooldown", c.Health.Passive.Cooldown.Std(), 30 * time.Second},
		{"rate_limit.global_rps", c.RateLimit.GlobalRPS, 5000},
		{"rate_limit.per_client_rps", c.RateLimit.PerClientRPS, 100},
		{"pools.api[0].weight", c.Pools["api"][0].Weight, 3},
		{"pools.api[1].weight", c.Pools["api"][1].Weight, 1},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.field, ch.got, ch.want)
		}
	}

	if len(c.Routes) != 2 {
		t.Fatalf("len(routes) = %d, want 2", len(c.Routes))
	}
	if c.Routes[0].Match.PathPrefix != "/api" || c.Routes[0].Pool != "api" {
		t.Errorf("routes[0] = %+v", c.Routes[0])
	}
	if c.Routes[1].Match.PathPrefix != "" || c.Routes[1].Pool != "web" {
		t.Errorf("routes[1] = %+v", c.Routes[1])
	}
}

// Keeps the shipped example honest against schema changes.
func TestLoad_ExampleConfig(t *testing.T) {
	if _, err := Load("../../config.example.yaml"); err != nil {
		t.Fatalf("config.example.yaml must be valid: %v", err)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("testdata/does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "does-not-exist.yaml") {
		t.Errorf("error = %q, want it to name the file", err)
	}
}

// Each row asserts both that the config is rejected and that the message names
// what is wrong.
func TestParse_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "empty file",
			src:     "",
			wantErr: "empty",
		},
		{
			name: "unknown key",
			src: `
mode: l4
listen: ":8080"
balancr: round_robin
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "balancr",
		},
		{
			name: "missing mode",
			src: `
listen: ":8080"
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "mode",
		},
		{
			name: "unknown mode",
			src: `
mode: tcp
listen: ":8080"
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "mode",
		},
		{
			name: "missing listen",
			src: `
mode: l4
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "listen",
		},
		{
			name: "listen without port",
			src: `
mode: l4
listen: "127.0.0.1"
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "listen",
		},
		{
			name: "unknown balancer",
			src: `
mode: l4
listen: ":8080"
balancer: magic
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "balancer",
		},
		{
			name: "malformed duration",
			src: `
mode: l4
listen: ":8080"
timeouts:
  dial: 2 seconds
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "duration",
		},
		{
			name: "duration as bare number",
			src: `
mode: l4
listen: ":8080"
timeouts:
  dial: 2
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "duration",
		},
		{
			name: "negative timeout",
			src: `
mode: l4
listen: ":8080"
timeouts:
  dial: -1s
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "timeouts.dial",
		},
		{
			name: "negative max_connections",
			src: `
mode: l4
listen: ":8080"
max_connections: -1
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "max_connections",
		},
		{
			name: "max_connections in l7 mode",
			src: `
mode: l7
listen: ":8080"
max_connections: 100
routes:
  - match: { path_prefix: "/" }
    pool: default
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "max_connections: only supported in l4 mode",
		},
		{
			name: "bad trusted_proxies entry",
			src: `
mode: l7
listen: ":8080"
trusted_proxies: ["10.0.0.0/33"]
routes:
  - match: { path_prefix: "/" }
    pool: default
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: `invalid CIDR "10.0.0.0/33"`,
		},
		{
			name: "trusted_proxies in l4 mode",
			src: `
mode: l4
listen: ":8080"
trusted_proxies: ["10.0.0.0/8"]
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "trusted_proxies: only supported in l7 mode",
		},
		{
			name: "tls in l4 mode",
			src: `
mode: l4
listen: ":8080"
tls:
  cert: cert.pem
  key: key.pem
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "tls: not allowed in l4 mode",
		},
		{
			name: "probe timeout exceeds interval",
			src: `
mode: l4
listen: ":8080"
health:
  active:
    interval: 1s
    timeout: 2s
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "health.active.timeout",
		},
		{
			name: "rise below one",
			src: `
mode: l4
listen: ":8080"
health:
  active:
    rise: -1
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "health.active.rise",
		},
		{
			name: "fall below one",
			src: `
mode: l4
listen: ":8080"
health:
  passive:
    fall: -1
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "health.passive.fall",
		},
		{
			name: "negative cooldown",
			src: `
mode: l4
listen: ":8080"
health:
  passive:
    cooldown: -5s
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "health.passive.cooldown",
		},
		{
			name: "negative global rate limit",
			src: `
mode: l4
listen: ":8080"
rate_limit:
  global_rps: -1
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "rate_limit.global_rps",
		},
		{
			name: "negative per-client rate limit",
			src: `
mode: l4
listen: ":8080"
rate_limit:
  per_client_rps: -1
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "rate_limit.per_client_rps",
		},
		{
			name: "no pools",
			src: `
mode: l4
listen: ":8080"
`,
			wantErr: "pools",
		},
		{
			name: "empty pool",
			src: `
mode: l4
listen: ":8080"
pools:
  default: []
`,
			wantErr: "pools.default",
		},
		{
			name: "backend without port",
			src: `
mode: l4
listen: ":8080"
pools:
  default:
    - addr: "127.0.0.1"
`,
			wantErr: "pools.default[0].addr",
		},
		{
			name: "backend without host",
			src: `
mode: l4
listen: ":8080"
pools:
  default:
    - addr: ":9001"
`,
			wantErr: "host is required",
		},
		{
			name: "backend missing addr",
			src: `
mode: l4
listen: ":8080"
pools:
  default:
    - weight: 2
`,
			wantErr: "pools.default[0].addr",
		},
		{
			name: "duplicate backend address",
			src: `
mode: l4
listen: ":8080"
pools:
  default:
    - addr: "127.0.0.1:9001"
    - addr: "127.0.0.1:9001"
`,
			wantErr: "duplicate",
		},
		{
			name: "negative weight",
			src: `
mode: l4
listen: ":8080"
pools:
  default:
    - addr: "127.0.0.1:9001"
      weight: -2
`,
			wantErr: "weight",
		},
		{
			name: "routes in l4 mode",
			src: `
mode: l4
listen: ":8080"
routes:
  - match:
      path_prefix: /
    pool: default
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "routes",
		},
		{
			name: "empty routes list in l4 mode",
			src: `
mode: l4
listen: ":8080"
routes: []
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "routes",
		},
		{
			name: "l7 without routes",
			src: `
mode: l7
listen: ":8080"
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "routes",
		},
		{
			name: "route without pool",
			src: `
mode: l7
listen: ":8080"
routes:
  - match:
      path_prefix: /
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "routes[0].pool",
		},
		{
			name: "route points at unknown pool",
			src: `
mode: l7
listen: ":8080"
routes:
  - match:
      path_prefix: /
    pool: nope
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "nope",
		},
		{
			name: "tls without key",
			src: `
mode: l7
listen: ":8080"
tls:
  cert: /tmp/x.crt
routes:
  - match: { path_prefix: "/" }
    pool: default
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "tls.key",
		},
		{
			name: "tls without cert",
			src: `
mode: l7
listen: ":8080"
tls:
  key: /tmp/x.key
routes:
  - match: { path_prefix: "/" }
    pool: default
pools:
  default:
    - addr: "127.0.0.1:9001"
`,
			wantErr: "tls.cert",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.src))
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestParse_TrustedProxies(t *testing.T) {
	c, err := Parse(strings.NewReader(`
mode: l7
listen: ":8080"
trusted_proxies: ["10.0.0.0/8", "192.0.2.5", "2001:db8::/32"]
routes:
  - match: { path_prefix: "/" }
    pool: default
pools:
  default:
    - addr: "127.0.0.1:9001"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := map[string]bool{
		"10.1.2.3":    true,  // inside 10.0.0.0/8
		"192.0.2.5":   true,  // a bare IP means just that address
		"192.0.2.6":   false, // ...and not its neighbour
		"2001:db8::1": true,
		"8.8.8.8":     false,
		"not-an-ip":   false,
	}
	for ip, want := range cases {
		if got := c.IsTrustedProxy(ip); got != want {
			t.Errorf("IsTrustedProxy(%q) = %v, want %v", ip, got, want)
		}
	}
}
