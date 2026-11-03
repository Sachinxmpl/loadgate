# Benchmarks

Reproducible measurements of LoadGate — what it costs and how it behaves under load. Everything here can be regenerated with `make bench`.

## How the benchmarks work

- **Bare processes over loopback.** The balancer, the backends, and the load generator all run as plain processes on one machine, talking over localhost — no Docker in the path. This measures the balancer's own overhead, not network or container costs.
- **Open-loop load.** The load generator (`test/loadgen`) sends requests at a fixed rate no matter how fast replies come back.
- **Percentiles.** Each run reports p50 (median), p99, and p999 latency plus the success rate. An average hides the slow tail, percentiles show it.
- **Warm-up discarded.** The first few seconds of each run (connection setup, pool fill) are thrown away, so we measure steady state.

Run everything with:

```bash
make bench
```

Results land in `test/bench/results/`.

## Environment

All numbers below were measured on:

```
cpu:   13th Gen Intel(R) Core(TM) i5-13420H
cores: 12
mem:   15Gi
go:    go1.26.5
host:  Linux 7.0.11-76070011-generic x86_64
```

---

## E1 — How much does LoadGate cost?

**Question.** When you put LoadGate in front of a backend, how much latency does it add per request?

**Method.** One backend with a fixed 5 ms delay. Send 1000 requests/second for 30 seconds, three ways: straight to the backend (the baseline), through LoadGate in L7 (HTTP) mode, and through it in L4 (TCP) mode. Then compare.

**Result.**

| target | requests | success | throughput | p50 | p99 | p999 | max |
|--------|---------:|--------:|-----------:|----:|----:|-----:|----:|
| direct     | 28911 | 100% | 964/s | 5.591 ms | 6.464 ms | 7.363 ms | 10.163 ms |
| through L7 | 29011 | 100% | 967/s | 6.013 ms | 7.008 ms | 8.672 ms | 11.469 ms |
| through L4 | 29293 | 100% | 976/s | 5.777 ms | 6.598 ms | 7.750 ms | 10.106 ms |

Overhead added by LoadGate (latency minus the direct baseline):

| mode | added at p50 | added at p99 |
|------|-------------:|-------------:|
| L4 | +0.19 ms | +0.13 ms |
| L7 | +0.42 ms | +0.54 ms |

**What it proves.** On this test workload, LoadGate adds less than 0.5 ms of p50 latency per request. L4 adds 0.19 ms at p50 and 0.13 ms at p99, while L7 adds 0.42 ms at p50 and 0.54 ms at p99. All requests succeeded, and the p999 latency remained within 3 ms of the median in both proxy modes.

---

## E2 — Does goroutine-per-connection hold up?

**Question.** LoadGate gives every connection its own goroutines. Does that model still work with thousands of connections at once, or does it fall apart?

**Method.** Run LoadGate in L4 mode in front of a backend that hangs on every request, so connections stay open. Open a growing number of connections at once — 100 up to 10,000 — and at each level read two numbers straight off the balancer's `/metrics`: its live goroutine count (`go_goroutines`) and its memory use (`process_resident_memory_bytes`). L4 is used here because its byte-relay has no HTTP-level timeout, so the slow backend keeps every connection parked.

**Result.**

| connections | goroutines | memory |
|------------:|-----------:|-------:|
| 0 (idle) | 12    | 12.8 MB  |
| 100      | 312   | 16.9 MB  |
| 500      | 1512  | 25.3 MB  |
| 1000     | 3012  | 32.9 MB  |
| 2000     | 6012  | 51.1 MB  |
| 5000     | 15012 | 107.5 MB |
| 10000    | 30012 | 189.5 MB |

Every connection was established; none failed.

**What it proves.** Goroutine usage scales linearly with the number of active L4 connections at exactly `12 + 3 × connections`in this test. Memory usage increases by approximately 18 KB per connection, reaching 189.5 MB at 10,000 connections. All 10,000 connections were established successfully, with no connection failures.

---

## E3 — When does least-connections beat round-robin?

**Question.** When one backend is slow, does the choice of balancing algorithm actually matter?

**Method.** Three backends: two fast (5 ms) and one slow (150 ms). Send 1000 requests/second for 30 seconds through LoadGate (L7), once with `round_robin` and once with `least_connections`, then compare the latencies. The slow backend is 150 ms (not slower) so it stays under the default 10 s `response_header` timeout and is not treated as failed.

**Result.**

| algorithm | p50 | p90 | p99 | mean |
|-----------|----:|----:|----:|-----:|
| round_robin       | 6.6 ms | 151 ms | 153 ms | 54.7 ms |
| least_connections | 6.2 ms | 6.9 ms | 152 ms | 10.1 ms |

Both served every request successfully at ~960/s.

**What it proves.** With two 5 ms backends and one 150 ms backend, least-connections reduces p90 latency from 151 ms to 6.9 ms and mean latency from 54.7 ms to 10.1 ms compared with round-robin. The slow backend received approximately 1.6% of requests under least-connections, compared with approximately 33% under round-robin. p99 remained approximately 152 ms for both algorithms.

---

## E4 — Is the 1/N remap claim real?

**Question.** When one backend is removed, how many keys does consistent hashing move, compared with naive modulo-N hashing?

**Method.** Map 100,000 keys across 10 backends using LoadGate's consistent-hash ring, then drop each backend in turn and count how many keys change backend. Repeat with naive `hash(key) % N` for comparison. Averaging over all 10 possible single-backend drops removes the luck of which backend is dropped. Pure computation — no network or processes.

**Result.**

| method | keys moved (avg) | per-drop range |
|--------|-----------------:|---------------:|
| consistent_hash | 10.0% | 4.3% – 16.0% |
| modulo_n        | 90.1% | — |

Ideal for 10 backends is 1/N = 10.0%.

**What it proves.** Removing 1 of 10 backends moves an average of 10.0% of keys under consistent hashing, matching the 1/N ideal, versus 90.1% under modulo-N — roughly 9× fewer. The average of exactly 10.0% means only the dropped backend's own keys move; no other keys are disturbed. The per-drop range of 4.3% to 16.0% reflects uneven key distribution at the production vnode count: individual backends own between 4.3% and 16.0% of the key space.

---

## E5 — What does the health loop buy?

**Question.** When a backend is sick — accepting connections but not responding in time — does the health loop keep traffic away from it?

**Method.** Three backends: two healthy (5 ms) and one sick (60 s delay, past the 300 ms `response_header` timeout configured for this experiment, so every request to it times out). Send 1000 requests/second for 30 seconds through LoadGate (L7, round_robin), once with the passive path enabled (`fall = 3`) and once with it disabled (`fall = 1000000`, i.e. active-only). Round-robin is used so the health loop is the only mechanism that can route around the sick backend.

**Result.**

| health | p50 | p90 | p99 | p999 | mean |
|--------|----:|----:|----:|-----:|-----:|
| passive+active | 6.1 ms | 6.7 ms | 10.5 ms | 308 ms | 8.3 ms |
| active-only    | 6.5 ms | 307 ms | 309 ms | 312 ms | 106 ms |

Both served every request successfully at ~965/s.

**What it proves.** With one sick backend and round-robin balancing, enabling the passive path reduces mean latency from 106 ms to 8.3 ms and p90 from 307 ms to 6.7 ms. With the passive path disabled, round-robin sends approximately one-third of requests to the sick backend; each times out after 300 ms and is retried, so a third of all requests are slow for the entire run. With the passive path enabled, the sick backend is evicted after 3 failures and traffic goes only to the healthy backends. In LoadGate the passive path is the only mechanism that evicts a backend — the active prober only readmits — so active-only means no eviction at all. The passive+active p999 of 308 ms reflects brief flapping: the active prober's TCP-only probe succeeds against the sick backend (it accepts connections), readmitting it momentarily before the passive path evicts it again.

## Profiling

Where the CPU and memory actually go under load — CPU, heap, and goroutine profiles captured with pprof — is written up separately in [profiling.md](profiling.md). In short: the proxy spends its CPU on network syscalls and scheduling, holds under 4 MB of live memory, and runs the load on less than one core.

## Limitations

All traffic runs on a single machine over loopback. This measures LoadGate's own overhead — CPU and memory — not real-world network behaviour. On a real network the round-trip time between machines dwarfs the sub-millisecond cost measured here. Read these as "how much does the balancer add," not "how fast is a request in production."
