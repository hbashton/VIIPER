# E2E Latency Benchmarks

## Transport-neutral A/B landing zone

`_testing/e2e/latency` is the source-only evidence and policy layer for comparing
the installed USB/IP path with a future transport. It does not import, install,
or require a native driver. Its deterministic probe tests run on an ordinary Go
development machine:

```powershell
go test ./_testing/e2e/latency
```

This is currently infrastructure, not a checked-in live latency result. A fake
probe passing these tests proves that scheduling, timestamp validation,
aggregation, and policy work without a device; it does not prove the latency of
USB/IP or a candidate transport.

### Evidence boundaries

Every capture declares one of these profiles, and the strict parser requires
exactly its ordered timestamps:

| Profile | Required monotonic stages | Intended use |
| --- | --- | --- |
| `api-to-consumer` | client published, broker received, transport admitted, virtual pad observed | Existing live API-write-to-SDL comparison |
| `physical-to-consumer` | physical input read, then every `api-to-consumer` stage | Later DS4Windows-to-game qualification |
| `recovery-to-consumer` | recovery triggered, transport ready, client published, broker received, transport admitted, virtual pad observed | Explicit reconnect/re-enumeration exercises |

The current live measurement boundary is `api-to-consumer`. It must not publish
or synthesize a `physical-input-read` timestamp. That broader profile becomes
valid only after a physical-reader adapter supplies a real stamp from the same
machine-wide monotonic clock. Similarly, an API-to-consumer result must not be
described as physical-controller latency.

The clock name, identity, and frequency are part of each transport's provenance.
Raw ticks are retained. The harness rejects a clock mismatch, absent or
out-of-order stage, overflow, lifecycle generation change on a healthy sample,
or recovery sample without an explicitly triggered advancing generation. It
computes end-to-end and adjacent-stage durations itself; probe implementations
cannot submit a precomputed latency.

Because the two transport descriptors must identify the same machine-wide
stage clock, successful captures must also advance globally in recorded block
order. A locally well-formed timeline whose first tick overlaps or precedes the
prior capture's final tick is rejected as reordered or regressing evidence.

Consumer events have a separate stale-edge fence. The workload identifies that
event clock, and every sample records a fence immediately before publication
plus the exact observed control, transition, and event timestamp. The observed
event must be newer than its fence, and every fence must be newer than the
preceding observation globally across both transport arms and all blocks. The
event clock is admission evidence only; it is never subtracted from the stage
clock.

### Comparison contract

Live adapters implement only the `Probe` interface:

1. `Describe` returns the exact runtime implementation, version, build identity,
   clock, sorted artifact SHA-256 identities, and relevant configuration.
2. `Capture` performs one requested press or release and returns raw stage and
   lifecycle evidence.

The baseline descriptor must be named `usbip` and match the canonical release
baseline version `0.9.7.7`; the candidate name must be different.
The runner records both descriptors before measurement and checks them again
afterward so runtime drift fails the artifact. The common session also records
the VIIPER source revision and dirty bit, Go/OS/architecture, hostname, harness
contract version, and run label. The shared workload binds both arms to the
same controller type and VID/PID, control, authenticated encrypted API endpoint,
and exact observer name, version, and binary SHA-256.

Admission deep-copies every artifact and configuration slice returned by
`Describe`; the probe cannot mutate a shared backing array to rewrite that
snapshot. A second freshly copied descriptor for each arm is retained in the
JSON after capture. Provenance failures and drift verdicts are recomputed from
those before/after records by both `ParseReport` and `RequirePass`.

One cycle uses four counterbalanced transport blocks. `ABBA` runs USB/IP,
candidate, candidate, USB/IP; `BAAB` reverses that orientation. Odd cycle
indices must use ABBA and even indices BAAB. The declared
sample pairs are divided between each transport's two blocks, with continuous
per-transport sequence numbers. Each block receives exactly 16 separate
unmeasured warmup pairs.

To avoid repeatedly landing on one phase of a 1 ms service interval, every
request carries a 2 ms base dwell plus the deterministic offsets `0, 125000,
250000, 375000, 500000, 625000, 750000, 875000` ns. The plan retains the vector
and the SHA-256 of its comma-separated decimal representation. A live probe may
sleep before the measured boundary; it must not add this dwell to the measured
interval.

The eight offsets, their order and hash, the 2 ms base dwell, the 16 warmup
pairs, and the four-block orientation are canonical release methodology. A
different vector with a matching self-generated hash, a zero/changed dwell, a
zero/changed warmup, or a cycle/orientation mismatch is rejected rather than
accepted as a different release workload.

The completion time is captured immediately after `Probe.Capture` returns and
compared with the effective context deadline using the same monotonic `time`
domain that created it. A canceled parent/context, clock regression, or return
at or after the deadline remains a failed capture even if the probe returns a
nil error and otherwise valid evidence.

### Statistics and policy

Raw press and release samples remain in the JSON evidence. The report computes
nearest-rank p50, p90, p95, p99, p99.9, maximum, arithmetic mean, and population
jitter for press, release, and combined end-to-end latency. It also computes
each adjacent stage distribution and candidate-minus-USB/IP deltas and ratios.

Healthy and recovery samples never share a distribution:

- `healthy` means one stable, nonzero transport generation from capture start to
  virtual observation. It is enabled by default and fails on capture/warmup
  errors or insufficient press/release counts.
- `recovery` requires an explicit disruption reason and generation advance. It
  is collected and reported separately. The plan binds one exact expected
  scenario/reason across both arms. Within each transport block, every sample's
  generation-before must equal the preceding generation-after; healthy samples
  never advance it and recovery samples do. Reused, skipped-backward, or
  mismatched scenario evidence is rejected. Recovery remains diagnostic by
  default.

The default healthy candidate ceilings are 4 ms p95, 8 ms p99, and 20 ms max.
Same-machine non-regression allowances are 1 ms p95, 2 ms p99, and 5 ms max over
USB/IP. A policy can additionally require a lower mean, p95, or p99. Those
strict-improvement switches should be used in a multi-cycle balanced matrix;
one passing cycle is descriptive evidence for that run, not a universal
superiority claim.

These defaults are the minimum release policy, not editable suggestions inside
an artifact. Release parsing rejects a dirty-source allowance, fewer than 256
pairs, tolerated capture failures, disabled absolute/comparative gates, or any
numeric ceiling/regression allowance looser than the defaults. Higher sample
counts, lower limits, and strict-improvement requirements are accepted as
stronger policies.

If recovery is enabled as a release gate, it inherits the same conservative
canonical 256-pair, no-failure, absolute, and comparative floor; every value may
only be tightened. No independently reviewed recovery budget exists yet, so the
harness does not invent looser numbers. Enabling recovery without its expected
scenario or without complete evidence cannot produce a passing artifact.

`ParseReport` rejects unknown or trailing JSON and recomputes sample durations,
stage spans, aggregates, comparisons, provenance failures, and verdict from the
raw ticks and post-run snapshots. `RequirePass` performs the same strict
round-trip/recomputation rather than trusting an in-memory verdict field.
The default policy also rejects a dirty source tree. Retain the full JSON and
the exact external trace or marker artifacts used by a live probe; this source-
only layer does not claim that JSON alone is tamper-proof.

### Remaining live-adapter work

The source-bound Windows USB/IP probe core now binds an authenticated, freshly
opened DualSense V5 stream to an event-driven SDL observation and supplies the
four truthful `api-to-consumer` stages. Broker receipt and successful terminal
USB/IP presentation commit are opt-in hooks compiled only with
`-tags viiper_latency`; normal builds inline empty hooks and probe construction
fails immediately. All stage timestamps use the exact QPC identity of the
single in-process server/client harness, while SDL's event clock remains a
separate stale-edge fence.

The probe does not start VIIPER, create a bus/device, attach USB/IP, initialize
SDL, or claim that an external process shares its QPC identity. A live wrapper
must still perform provenance preflight, create an isolated in-process server,
prove the exact virtual-controller identity, give the probe exclusive ownership
of a new V5 stream starting at sequence zero, and give the SDL observer
exclusive main-thread event-queue ownership. The concrete SDL consumer is
available only in a Windows CGO build with the same latency tag.

An opt-in server method, `InputLatencyImportGeneration`, now exposes the exact
active import lease already assigned by the USB/IP server for one bus/device
identity. The lease advances on every successful reimport and disappears on
disconnect. A wrapper can therefore use it as the probe lifecycle source
without inventing a generation or changing the production hot path. The method
does not exist in an ordinary release build.

There is still no executable live wrapper or USB/IP-only report mode. `Runner`
requires both the `usbip` baseline and a distinctly named candidate, so passing
the same USB/IP implementation twice would be false evidence rather than a
baseline workaround. Consequently there is no valid current command that emits
the declared live report, and no current `0.9.7.7` live baseline has been
captured by this landing-zone change.

The eventual no-install procedure must use a clean source revision and a single
Windows CGO process built with both `release` and `viiper_latency`; a normal
`release` VIIPER lacks the broker/terminal hooks. That process must use unused
loopback ports and an
authenticated stream, snapshot the SDL gamepad set, create and auto-attach one
isolated V5 virtual DualSense through the already installed `0.9.7.7` driver,
bind the newly observed controller and returned USB/IP import lease to the same
bus/device, run the canonical schedule, finish neutral, detach only the returned
USB/IP port, remove its device and bus, and prove both the port and PnP identity
are gone before accepting the artifact. A virtual-controller arrival is
machine-global, so this cannot be described as non-disruptive while games,
streaming software, remappers, or other controller consumers are active.

The future backend adapter must use the identical workload, observer, clock,
controller identity checks, and runtime provenance. Only after the live wrapper
runs both adapters from the same current revision should their artifacts be
compared.

## Aggregate Go benchmark

The script `viiper/_testing/e2e/scripts/lat_bench.go` runs (or parses) end‑to‑end input latency benchmarks and produces enriched output (table, markdown, or JSON).

It groups repeated cycles when `-count > 1` and uses the single press E2E measurement (`E2E-InputDelay`) as the 100% baseline.

## Output

| Column          | Meaning                                                                                   |
| --------------- | ----------------------------------------------------------------------------------------- |
| Benchmark       | Name of the sub benchmark                                                                 |
| Count           | Iterations performed (from Go bench output; affected by `-benchtime`)                     |
| ns/op           | Nanoseconds per operation (direct Go benchmark figure)                                    |
| % of Full       | Relative to `E2E-InputDelay` (single press baseline)                                      |
| Client Share %  | Portion attributed to the (go) client write phase (for E2E rows)                          |
| Latency Share % | Remainder attributed to transport + virtual device/host stack + tight device polling loop |

`E2E-PressAndRelease` includes both press and release cycles, so it is expected to be ~2× the single press and thus can exceed 100% in `% of Full`.

## Scope / Methodology

- All benchmarks included here are executed against a VIIPER server on the same host (localhost).  
  They therefore measure in-process client emission plus local USBIP stack + emulated device processing only.  
  Remote/network USBIP attachment will add network RTT and jitter which is intentionally excluded from these baseline figures.
- Benchmarks use a single emulated Xbox360 controller device.  
  Other devices might produce slightly different results depending on USB report size and VIIPER-InputState size.
- Benchmarks use a single button press, which is enough as clients/VIIPER always produce a full report of the devices state.  

## Benchtime Mode

Runs use a fixed-iteration benchtime (e.g. `-benchtime=1000x`, `-benchtime=10000x`) rather than time-based (e.g. `2s`).  

## Running

From repository root:

```bash
cd testing/e2e
# Single run, 1000 fixed iterations per sub benchmark
go run ./scripts/lat_bench.go -benchtime=1000x -count=1 -format markdown
```

Results (Arch Linux / SteamDeck Kernel / Steam Deck LCD / Go 1.25+, 10k iterations):

| Benchmark                   | Count | ns/op  | % of Full | Client Share % | Latency Share % |
| --------------------------- | ----- | ------ | --------- | -------------- | --------------- |
| 1_Go-Client-Write           | 10000 | 10668  | 11.98     | 100.00         | 0.00            |
| 2_InputDelay-Without-Client | 10000 | 74154  | 83.25     | 0.00           | 100.00          |
| 3_E2E-InputDelay            | 10000 | 89078  | 100.00    | 11.98          | 88.02           |
| 4_E2E-PressAndRelease       | 10000 | 184870 | 207.54    | 11.54          | 88.46           |

Example output (Windows / AMD Ryzen 9 3900X / Go 1.25+, 10k iterations):

| Benchmark                   | Count | ns/op  | % of Full | Client Share % | Latency Share % |
| --------------------------- | ----- | ------ | --------- | -------------- | --------------- |
| 1_Go-Client-Write           | 10000 | 27933  | 16.60     | 100.00         | 0.00            |
| 2_InputDelay-Without-Client | 10000 | 133724 | 79.45     | 0.00           | 100.00          |
| 3_E2E-InputDelay            | 10000 | 168307 | 100.00    | 16.60          | 83.40           |
| 4_E2E-PressAndRelease       | 10000 | 331439 | 196.93    | 16.86          | 83.14           |

Variability across repeated measurement runs has been negligible.  
Use a larger `-count` if you want to increase the number of runs.

## Notes

- Memory statistics from Go benchmarks are intentionally omitted.
- `% of Full` falls back to the largest ns/op if the baseline row is missing.
- All benchmarking must run with parallelism 1 in underlying benches.
- Benchmarks use a tight polling loop using SDL3 to detect input state changes on the emulated device.
- Benchmarks must be run without an already running VIIPER server instance.
