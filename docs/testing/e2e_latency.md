# Controller-to-game latency

VIIPER has several latency tools. They answer different questions and
must not be presented as interchangeable evidence.

- `_testing/e2e/scripts/lat_bench.go` formats Go benchmark averages. It is a
  useful developer diagnostic, but `ns/op` does not preserve individual tail
  samples, transition loss, or duplication. It is not the native release gate.
- `_testing/e2e/scripts/Invoke-ViiperE2ELatencyGate.ps1` is the opt-in Windows
  production gate. It records every press and release observed through SDL,
  compares authenticated USB/IP and native UDE runs, and emits a strict JSON
  evidence artifact plus a source-controlled sequential-file WPR trace.
- `_testing/e2e/scripts/Invoke-ViiperE2ELatencyMatrix.ps1` is the release
  entry point. It runs the complete gate once at Normal and once at High
  process priority, then binds both raw JSON/ETL/decoded-marker sets into one
  hash manifest.

No live latency result is checked into this document. A passing result exists
only when the production command below succeeds on the stated machine and its
source-bound artifacts are retained.

### Evidence boundary

This is an exact-source native-path, production-authentic API-to-consumer gate.
The USB/IP comparator is deliberately labeled
`version-probed-functional-baseline-not-source-bound`: the wrapper proves the
supported 0.9.7.7 command and functional port contract, not the source revision
of that third-party installed driver. The Go
test starts `cmd.Server` in process at the clean `HEAD` under test and uses the
repository's Go client over real localhost TCP. Beyond that process boundary it
uses the installed USB/IP or native UDE transport, the actual Windows controller
stack, and the source-bound SDL DLL. Authentication, API framing, controller
serialization, transport delivery, HID consumption, SDL event delivery, and
consumer wake-up are therefore live rather than mocked.

It is not a packaged-executable, service/task-hosted broker, DS4Windows, physical
controller, display, or game-engine-frame test. The signed-package/broker live
gates and any DS4Windows or physical-input qualification remain separate
evidence. A pass here must not be relabeled as a pass for those boundaries.

## What the production gate measures

For each controller below, the gate creates the device through the authenticated
VIIPER API, opens an authenticated controller stream, writes alternating south
button states, and waits for the corresponding game-facing SDL transition:

| API controller | Expected SDL type | VID:PID | South button |
| --- | --- | --- | --- |
| `xbox360` | Xbox 360 | `045e:028e` | A |
| `dualshock4` | PS4 | `054c:09cc` | Cross |
| `dualsensegamepadv5` | PS5 | `054c:0ce6` | Cross |

Each controller uses a fresh server, bus, device, stream, and exact SDL binding
for four counterbalanced blocks: USB/IP, native UDE, native UDE, USB/IP (ABBA).
Sixteen unrecorded press/release pairs warm the complete path at the start of
every block. The declared sample count is then split as evenly as possible
between the two blocks for each transport; `-Samples 256` therefore records 128
pairs in each block and aggregates 256 press plus 256 release samples per
transport. ABBA makes both the first/last positions USB/IP and both middle
positions native, reducing one-way warm-up and monotonic-drift bias without
discarding per-block source identity.

The v2 JSON retains every raw sample and publishes nearest-rank p50, p90, p95,
p99, p99.9, and max values plus population jitter. Its provenance includes the
host name, Windows product/display/build identity, CPU model, logical processor
count, token elevation, and the measured process priority class. Reports from
different machine identities cannot be combined into the priority matrix.

All four blocks use the same API address, credential, bus/device position,
input sequence, warm-up count, one-second event timeout, and deterministic
unmeasured dwell schedule. Xbox success cannot certify either PlayStation path.
Missing, ambiguous, or misidentified DualShock 4 or DualSense enumeration—or a
failure in any ABBA block—fails the whole suite.

A fixed 2 ms dwell could repeatedly land writes at the same phase of a 1 ms HID
service interval. The gate instead retains a 2 ms minimum state dwell and adds
the source-bound offsets `0, 125000, 250000, 375000, 500000, 625000, 750000,
875000` ns. Press and release edges index that vector deterministically by
sequence number, and block 2 resumes the same per-transport sequence. The
cumulative offsets visit all eight 125 us phases of one millisecond; all
controllers and transports receive the identical pattern. Sleeps occur before
the measured `WriteBinary` interval, never inside it, and do not busy-wait.

The artifact records the vector and SHA-256
`21eee9ea71984343ebd21221df8272553d6ab369a5740a1c796380cd468abcd9` of its
comma-separated base-10 nanosecond representation. The parser recomputes that
hash and rejects a changed vector or scheduling workload. This is a reproducible
phase-control policy, not a claim that Windows wakes at each requested
nanosecond; WPR retains the scheduler evidence for investigating overshoot.

The interval starts immediately before `DeviceStream.WriteBinary` and ends when
the exact SDL gamepad/button event returns to the waiting consumer. It therefore
includes authenticated client framing, localhost TCP delivery, VIIPER device
processing, the selected virtual USB transport, Windows controller input, SDL's
event path, and consumer wake-up. It does not claim display, engine-frame, or
network latency.

Raw `QueryPerformanceCounter` ticks bracket every interval and are converted
with the once-recorded `QueryPerformanceFrequency`; that conversion is the
canonical `latency_ns`. The strict parser recomputes it exactly and rejects an
overflow, clock regression, cross-sample QPC regression, or JSON latency that
does not match its retained ticks. Go's monotonic clock is used only for wait
deadlines. Microsoft recommends QPC for sub-microsecond interval and latency
measurement. SDL's event timestamp is retained independently to reject absent,
stale, or regressing events. The harness never subtracts the SDL clock from the
QPC clock.

The observer uses `SDL_WaitEventTimeout`, not a tight state loop. It observes
the complete unmeasured dwell, then drains exact queued button events, checks
the current state, captures QPC, and places an `SDL_GetTicksNS` fence directly
beside the input write. An event must be strictly newer than that fence; an
older or same-tick event is rejected rather than misattributed to the write.
SDL and the authenticated TCP write expose no shared atomic operation, so this
is a stale-edge exclusion/admission proof, not a claim of cryptographic causal
identity across the irreducible final function-call boundary. Unexpected
same-state edges from the exact device are counted as duplicates while the wait
continues. A missing expected edge increments the appropriate miss counter and
terminates that transport/controller run. The final quiet window is also an SDL
event wait, so late release duplicates are not hidden and no measurement-side
busy poll consumes a CPU core.

The source-bound SDL build enables its Windows RawInput backend before
initialization. SDL's default Xbox backend exposes only a logical `XInput#N`
path; RawInput retains the exact HID device-interface path needed to bind the
observed controller to Windows PnP ancestry. This makes the Xbox arm an SDL
RawInput consumer-path measurement, not an XInput API polling measurement. A
logical XInput path or failure to enable RawInput fails closed rather than
falling back to VID/PID-only identity.

## Source and device binding

The PowerShell entry point fails closed before measurement unless all of the
following are true:

- `HEAD` equals the caller-supplied 40- or 64-digit source revision;
- the tracked and untracked source tree is clean and every submodule is at its
  recorded revision;
- the native package passes the existing production Microsoft-signature and
  submission-manifest gate;
- the installed `ViiperUde.sys` hash matches that verified package and the one
  VIIPER root devnode reports a Microsoft signer;
- the live Go harness is linked with the clean `HEAD` as
  `nativeSourceRevision`, and the build identity negotiated from the loaded
  kernel image exactly matches the verified manifest identity (the installed
  file hash alone is not presented as loaded-image proof);
- the SDL DLL hash matches the caller-supplied source-build hash;
- the DLL actually loaded by the Go test is that exact absolute SDL path and
  hash;
- the USB/IP prerequisite check accepts the repository's supported runtime;
- both servers reject an unauthenticated ping, while the authenticated ping
  reports the requested live transport and a ready backend;
- `DeviceAdd` returns the expected controller type, VID, PID, bus, device ID,
  and (for USB/IP) exact auto-attached import port;
- all baseline SDL gamepads remain present and exactly one stable new SDL ID is
  created; its path, GUID, real type, VID, and PID must match the API device;
- the SDL HID interface resolves to an exact present Windows PnP instance and
  container identity plus a cardinality-consistent ancestor chain. A
  native run must terminate at service `ViiperUde`/hardware ID
  `ROOT\VIIPER\UDE`; USB/IP must terminate at service `usbip2_ude`/INF hardware
  ID `ROOT\USBIP_WIN2\UDE` (the OS-assigned devnode instance is commonly
  `ROOT\USB\####` and is recorded separately). The gate follows the unified
  `DEVPKEY_Device_Parent` relation through that anchor to `HTREE\ROOT\0`; a
  truncated/cyclic chain or a second matching anchor is rejected.

The gate does not install, update, stop, replace, or remove a driver or service.
Run it on a disposable test machine with the verified production package and
supported USB/IP runtime already installed. No VIIPER process or service may
already own the API ports or native broker handle.

## Statistics and pass policy

The artifact retains every sample's sequence, transition, monotonic latency,
SDL event/pre-write-fence timestamps, raw QPC start/end/pre-marker ticks, and canonical
TraceLogging marker ID. It reports press,
release, and combined distributions for each controller/transport:

- p50, p95, and p99 use the nearest-rank definition (`ceil(p * N)`, one based);
- max is the largest individual interval;
- jitter is the population standard deviation of the individual intervals;
- misses and duplicates are separate press/release counters.

At least 256 complete press samples and 256 complete release samples, aggregated
from both counterbalanced blocks, are required for every controller and
transport. A timeout, write/event error,
insufficient count, non-monotonic SDL event clock, any miss, or any duplicate
fails the artifact. Native press, release, and combined distributions must each
remain at or below the reviewed native limits: 4 ms p95, 8 ms p99, and 20 ms
maximum.

The JSON also reports native-minus-USB/IP deltas and native/USB-IP ratios for
p50, p95, p99, max, and jitter. A same-machine non-regression policy additionally
requires native p95, p99, and maximum to be no more than 1 ms, 2 ms, and 5 ms
above the corresponding USB/IP values for press, release, and combined samples.
Those absolute deltas are engineering acceptance limits set at one quarter of
the corresponding 4/8/20 ms native ceilings. They avoid unstable ratios when a
USB/IP baseline is very small; they are policy, not a claim about observed
transport performance. The gate does not say native is lower latency unless the
retained live artifact actually shows negative native-minus-USB/IP deltas.

The parser rejects unknown fields, trailing JSON, weakened absolute or
same-machine limits, a non-ABBA schedule, mixed transport proof, workload drift
between controllers, reordered or missing press/release samples, and block,
aggregate, comparison, or verdict fields that do not exactly recompute from the
individual records.

## Running the production gate

Prerequisites are an elevated Windows PowerShell session, an exact clean
checkout, Go 1.26 or newer, CGO with a working C toolchain, CMake, the
source-built SDL submodule, WPR, USB/IP win2
0.9.7.7, and an already installed Microsoft-signed VIIPER UDE package matching
its submission manifest.

The SDL wrapper currently links the multi-configuration Debug output. Build and
record that exact binary before running the gate:

```powershell
cmake -S .\_testing\e2e\deps\SDL -B .\_testing\e2e\deps\SDL\build -A x64
cmake --build .\_testing\e2e\deps\SDL\build --config Debug
$sdlHash = (Get-FileHash .\_testing\e2e\deps\SDL\build\Debug\SDL3.dll -Algorithm SHA256).Hash
```

Choose an existing evidence directory outside the checkout. Existing files are
never overwritten. `-Samples` is the total pair count per
controller/transport and is bounded to 256–10,000. The release matrix defaults
to 10,000 and produces independent Normal/High JSON, ETL, and decoded-marker
artifacts.

```powershell
$revision = (git rev-parse HEAD).Trim()
$gitExe = 'C:\Program Files\Git\cmd\git.exe'
$goExe = 'C:\Go\bin\go.exe'

.\_testing\e2e\scripts\Invoke-ViiperE2ELatencyMatrix.ps1 `
  -SignedPackageDirectory C:\ViiperUde\MicrosoftSigned `
  -SubmissionManifestPath C:\ViiperUde\ViiperUde.cab.sha256.json `
  -ExpectedSourceRevision $revision `
  -SDLBinarySHA256 $sdlHash `
  -EvidenceDirectory C:\ViiperEvidence `
  -GitExecutable $gitExe `
  -GoExecutable $goExe `
  -Samples 10000
```

For a single diagnostic run, call `Invoke-ViiperE2ELatencyGate.ps1` directly
with `-PriorityClass Normal` or `-PriorityClass High` and new `-OutputPath` and
`-WprTracePath` values. Both wrappers require absolute, non-reparse Git and Go
executable paths; the system WPR image is pinned automatically. Their paths and
SHA-256 values are retained in the v2 provenance. A single run is not the
release priority matrix.

The wrapper verifies and uses the checked-in `ViiperLatency.wprp` in sequential
file mode, names the recording instance, rejects any reported event/buffer
loss, and saves the trace on both pass and test failure. The profile includes
context-switch, ready-thread, sampled-profile, DPC, interrupt, and WDF evidence
needed to investigate a tail. A fixed TraceLogging provider captures another
QPC value after the measured end and then emits each marker. The wrapper decodes
the ETL oldest-first and requires exact chronological, one-to-one marker and
QPC/timestamp/latency payload equality with the strictly parsed JSON; missing,
duplicate, reordered, extra, or undecodable markers fail closed.
The exact decoded marker set is retained beside the JSON as
`<OutputPath>.etl-markers.json`, and the production wrapper invokes the same Go
strict parser/recomputation verifier used by deterministic tests on the JSON,
decoded-marker, and ETL evidence pair.
The ETL remains corroborating scheduler evidence, not a substitute for SDL's
consumer timestamp.

Directly setting the live-test environment variable is intentionally
insufficient. The Go test also requires the preflight marker, expected source
and SDL revisions, loaded SDL path/hash, verified package-manifest hash,
installed-driver hash, sample count, and a new absolute output path.

## Aggregate developer diagnostic

For a non-gating average-only diagnostic, run from the repository root. Use the
encrypted rows when comparing transports so the API/controller stream mode is
the same:

```powershell
$env:VIIPER_E2E_TRANSPORT = 'usbip'
go run .\_testing\e2e\scripts\lat_bench.go `
  -pkg .\_testing\e2e -encryption encrypted -benchtime 1000x -count 5 -format markdown

$env:VIIPER_E2E_TRANSPORT = 'native-ude'
go run .\_testing\e2e\scripts\lat_bench.go `
  -pkg .\_testing\e2e -encryption encrypted -benchtime 1000x -count 5 -format markdown
```

Go benchmark `ns/op` is an aggregate timing result. Do not infer p95/p99,
misses, duplicates, or a live release pass from it.

## Method references

- [Go `testing` benchmarks](https://pkg.go.dev/testing) document `B.Loop`, the
  benchmark timer, and aggregate metric semantics.
- [Go monotonic time](https://pkg.go.dev/time#hdr-Monotonic_Clocks) documents why
  `time.Since(start)` is robust against wall-clock adjustment.
- [SDL gamepad button events](https://wiki.libsdl.org/SDL3/SDL_GamepadButtonEvent)
  define the nanosecond event timestamp, device ID, button, and edge.
- [`SDL_WaitEventTimeout`](https://wiki.libsdl.org/SDL3/SDL_WaitEventTimeout) is
  the blocking event-consumer primitive used by the observer.
- [`SDL_HINT_JOYSTICK_RAWINPUT`](https://wiki.libsdl.org/SDL3/SDL_HINT_JOYSTICK_RAWINPUT)
  documents that RawInput is disabled by default, handles XInput-capable
  devices, and must be enabled before SDL initialization.
- [Microsoft high-resolution timestamp guidance](https://learn.microsoft.com/en-us/windows/win32/sysinfo/acquiring-high-resolution-time-stamps)
  recommends QPC for interval and latency measurements.
- [Microsoft `CM_Get_Parent` and unified-parent guidance](https://learn.microsoft.com/en-us/windows/win32/api/cfgmgr32/nf-cfgmgr32-cm_get_parent)
  identifies `DEVPKEY_Device_Parent` as the Windows Vista-and-later device-tree
  parent relation used by the identity proof.
- [Microsoft WPR command-line guidance](https://learn.microsoft.com/en-us/windows-hardware/test/wpt/wpr-command-line-options)
  documents named instances, memory/file modes, profiles, start, and stop.
- [Microsoft WPR logging-mode guidance](https://learn.microsoft.com/en-us/windows-hardware/test/wpt/logging-mode)
  distinguishes sequential file logging from bounded circular memory logging.
- [Microsoft `Get-WinEvent` guidance](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.diagnostics/get-winevent)
  documents ETL `Path`, `ProviderName` filtering, and oldest-first decoding.
- [Microsoft TraceLogging capture guidance](https://learn.microsoft.com/en-us/windows-hardware/drivers/devtest/capture-and-view-tracelogging-data)
  documents collecting self-describing providers with WPR/WPA.
- [ViGEmBus](https://github.com/nefarius/ViGEmBus/tree/d986e1d93708ec9b11049542fa6027272cce716c)
  is the virtual-controller lifecycle and replay-method reference. Its design
  motivates testing through an unmodified game-consumer API; no ViGEm latency
  number is copied or claimed here.
