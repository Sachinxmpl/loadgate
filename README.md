# LoadGate
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![CI](https://github.com/Sachinxmpl/loadgate/actions/workflows/ci.yml/badge.svg)](https://github.com/Sachinxmpl/loadgate/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/Sachinxmpl/loadgate)](https://goreportcard.com/report/github.com/Sachinxmpl/loadgate)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](#contributing)

LoadGate is a modern L4/L7 reverse proxy and load balancer written in Go.

**Sub-0.5 ms** proxy overhead, linear scaling to **10,000** concurrent connections under **200 MB**, and automatic failover that never drops a request — every number [measured](docs/benchmark.md).

## Features

- **L4 and L7 modes** — raw TCP relay or full HTTP(S) reverse proxy, selected per instance.
- **Four algorithms** — round-robin, smooth weighted round-robin, least-connections, and consistent hashing (150 vnodes).
- **Two-plane health checking** — passive eviction on real-traffic failures, active probing for readmission (and, with `health.active.fall`, eviction of backends that fail their probes).
- **Fail-open** — if every backend in a pool is evicted, traffic is sent to all of them anyway instead of refusing everyone; a backend that has recovered serves at once, before the prober readmits it.
- **Automatic failover** — L7 retries failed idempotent requests and L4 retries refused connections on another healthy backend.
- **TLS termination** — HTTPS with hot certificate rotation; swap certs on reload, no restart.
- **Hot config reload** — `SIGHUP` validate-before-swap: a broken config can never take down a running server. Changing `mode`, `listen`, or turning TLS on/off is refused; those need a restart.
- **Rate limiting** — global and per-client token buckets, reject-don't-queue, LRU-bounded.
- **Observability** — Prometheus metrics on a separate port, provisioned Grafana dashboard, pprof profiling.

## Installation

**Go install** (requires Go 1.26+):

```bash
go install github.com/Sachinxmpl/loadgate/cmd/loadgate@latest
```

**From source:**

```bash
git clone https://github.com/Sachinxmpl/loadgate.git
cd loadgate
make build          # -> bin/loadgate
```

**Docker** — the repository ships a Compose stack (proxy + demo backends + Prometheus + Grafana):

```bash
docker compose up --build
```

## Quickstart

Write a config (`config.yaml`):

```yaml
mode: l7
listen: "127.0.0.1:8080"
balancer: round_robin
routes:
  - match: { path_prefix: "/" }
    pool: web
pools:
  web:
    - addr: "127.0.0.1:9001"
    - addr: "127.0.0.1:9002"
```

Validate it, then run:

```bash
loadgate check -c config.yaml    # validate and exit (non-zero on error)
loadgate run   -c config.yaml    # serve until interrupted
```

Send traffic, then reload without dropping a single connection:

```bash
curl http://127.0.0.1:8080/
kill -HUP $(pgrep loadgate)       # re-reads config; keeps the old one if the new is invalid
```

## Configuration

LoadGate is configured by a single YAML file. Top-level fields:

| Field | Type | Default | Meaning |
|-------|------|---------|---------|
| `mode` | string | *required* | `l4` (TCP relay) or `l7` (HTTP proxy) |
| `listen` | string | *required* | address to bind, e.g. `0.0.0.0:8080` |
| `balancer` | string | `round_robin` | `round_robin`, `weighted_round_robin`, `least_connections`, or `consistent_hash` |
| `tls` | object | *disabled* | L7 only. `{ cert, key }` paths; presence enables HTTPS |
| `timeouts` | object | dial `2s`, read/write/request `30s`, response_header `10s`, idle `60s`, drain `15s` | connection and request deadlines (L4 uses only `dial` and `drain`) |
| `health` | object | active `{ interval 2s, timeout 500ms, rise 2, path "", fall 0 }`, passive `{ fall 3, cooldown 10s, fail_on_5xx false }` | probing/readmission and eviction thresholds. `active.path` (e.g. `/health`) makes the probe an HTTP GET that must answer 2xx; empty = TCP connect only. In L7 mode, setting `passive.fail_on_5xx: true` counts upstream 5xx responses toward passive `fall`. |
| `rate_limit` | object | `0` (unlimited) | `global_rps`, `per_client_rps` |
| `max_connections` | int | `0` (unlimited) | L4 only (L7 relies on its read/idle timeouts). Connections over the limit are closed immediately. Keep it below ~⅙ of the FD limit (`ulimit -n`) |
| `trusted_proxies` | list | `[]` (trust no one) | L7 only. IPs/CIDRs of proxies in front of LoadGate; their `X-Forwarded-For` is kept, from anyone else it is replaced by the client's IP |
| `routes` | list | *required for L7* | `{ match: { path_prefix }, pool }`, longest prefix wins |
| `pools` | map | *required* | named lists of backends: `{ addr, weight }` (`weight` defaults `1`) |

<details>
<summary><b>Full annotated example</b></summary>

```yaml
mode: l7
listen: "0.0.0.0:8080"
balancer: least_connections

tls:                           # optional; presence enables HTTPS
  cert: /etc/loadgate/cert.pem
  key:  /etc/loadgate/key.pem

timeouts:
  dial: 300ms
  read: 30s
  write: 30s
  idle: 60s
  request: 30s
  response_header: 10s         # max wait for a backend to start answering
  drain: 15s                   # how long shutdown waits for in-flight requests

health:
  active:  { interval: 2s, timeout: 500ms, rise: 2 }   # probing / readmission
  passive: { fall: 3, cooldown: 10s }                  # eviction

rate_limit:
  global_rps: 0                # 0 = unlimited
  per_client_rps: 0

routes:                        # L7 only; longest path prefix wins
  - match: { path_prefix: "/api/" }
    pool: api
  - match: { path_prefix: "/" }
    pool: web

pools:
  api:
    - { addr: "10.0.0.1:8080", weight: 3 }
    - { addr: "10.0.0.2:8080", weight: 1 }
  web:
    - { addr: "10.0.0.3:8080" }
```

</details>

See [`config.example.yaml`](config.example.yaml) (L4) and [`configl7.example.yaml`](configl7.example.yaml) (L7) for ready-to-run examples.

## Usage

```
loadgate check -c <file>   validate a config file and exit
loadgate run   -c <file>   serve until interrupted
kill -HUP <pid>              hot-reload the config (validate-before-swap)
```

<details>
<summary><b><code>run</code> flags</b></summary>

| Flag | Default | Purpose |
|------|---------|---------|
| `-c` | `config.yaml` | config file path |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `-log-format` | `json` | `json` or `text` |
| `-metrics-addr` | `127.0.0.1:9095` | Prometheus metrics endpoint |
| `-debug-addr` | *(disabled)* | pprof debug port, e.g. `127.0.0.1:6060` |

</details>

## Observability

- **Metrics** — Prometheus format on `-metrics-addr` (default `127.0.0.1:9095/metrics`), with bounded cardinality (labelled by status *class* and backend, never by raw code, path, or client IP). Request counts and duration histograms, active connections, backend up/down, health transitions, rate-limit and connection-limit rejections, and reload outcomes.
- **Dashboard** — a Grafana dashboard is auto-provisioned in the Docker stack (`http://localhost:3000`); it shows a backend being killed, evicted, and readmitted live.
- **Profiling** — pprof endpoints on the private `-debug-addr` port. Capture a full CPU/heap/goroutine set with `bash test/bench/profile.sh`.

## Benchmarks

Measured on a 13th-gen Intel i5 (12 cores), all traffic over loopback, open-loop constant-rate load. Full method and raw data in [`docs/benchmark.md`](docs/benchmark.md) and [`docs/profiling.md`](docs/profiling.md).

| Experiment | Result |
|------------|--------|
| **E1** — proxy overhead per request | **+0.42 ms** (L7), **+0.19 ms** (L4) at the median |
| **E2** — connection scaling | **10,000** live connections: 3 goroutines each, **< 200 MB**, zero failures |
| **E3** — least-conn vs round-robin (one slow backend) | mean latency **10 ms vs 55 ms** |
| **E4** — consistent-hash remap (drop 1 of 10) | **10%** of keys move vs **90%** for naive modulo |
| **E5** — value of the health loop (sick backend) | mean latency **8 ms vs 106 ms** |

Under load the proxy spends its CPU on network syscalls and scheduling, holds under 4 MB of live heap, and runs on less than one core — the profile of a proxy that moves data rather than computing.

Reproduce everything:

```bash
make bench                     # E1–E5 -> test/bench/results/
bash test/chaos/run.sh         # chaos harness (kills backends under load)
go run test/chaos/verdict.go   # grade the chaos run: PASS/FAIL
```

## Development

```bash
make build        # build the binary
make test         # unit tests with the race detector
make lint         # golangci-lint
make bench        # run the five benchmark experiments
make ci           # fmt-check + vet + test (what CI runs)
```

Resilience is verified two ways: a **chaos harness** kills and restarts backends under sustained load and asserts fast failover with no goroutine leak, and **`goleak`** guards every package against leaked goroutines in CI.

## Contributing

Contributions are welcome. To propose a change:

1. Open an issue describing the problem or feature.
2. Fork and create a branch (`git checkout -b feature/my-change`).
3. Make your change with tests; ensure `make ci` passes (fmt, vet, race tests).
4. Open a pull request describing the change and, for anything performance-related, the benchmark before/after.

Keep the dependency budget small (standard library first) and, in the spirit of this project, **back new behaviour with a test or a benchmark**.

## Documentation

- [Final report](docs/project-report/final-report.pdf) — the full project report (design, results, evaluation)
- [Benchmarks](docs/benchmark.md) — the five experiments and their results
- [Profiling](docs/profiling.md) — where CPU and memory go under load
- [Chaos testing](docs/chaos-testing.md) — the failover / no-leak harness
- [Load simulation](docs/load-testing.md) — driving traffic by hand

## License

Released under the [MIT License](LICENSE).
