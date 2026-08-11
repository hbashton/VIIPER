# VIIPER native UdeCx bus

This directory contains the Windows native USB device-emulation layer. It is
developed on `feature/native-udecx-bus`; it does not alter the supported USB/IP
implementation on `main` while the native path is incomplete.

Directory contract:

- `include/` is the stable C ABI shared by the driver and Go broker.
- `driver/` is the KMDF/UdeCx controller driver.
- `package/` contains INF and installation metadata.
- `tools/ViiperUdeCtl.cpp` installs, verifies, or removes the exact root
  controller as a driver-store transaction. Installation requires the
  source-revision submission manifest, verifies the catalog signature and
  four-part `DriverVer`, rejects same-version replacement and implicit
  downgrade, records the prior published INF, negotiates the broker ABI after
  start, and restores the prior binding on failure. Removal backs up every
  exact signed VIIPER package before deleting only exact owned devnodes and
  packages; unrelated driver-store entries are never force-deleted.
- `tools/Test-ViiperUdeCtlTransaction.ps1` deterministically guards the
  transaction, rollback, ownership, downgrade, and structured-reboot source
  contracts. Passing a compiled tool through `-BinaryPath` also runs its pure
  parser/version self-test without changing driver state.
- `tools/New-ViiperUdeAttestationPackage.ps1` creates and hash-verifies the
  exact controlled-test Hardware Dev Center CAB structure and requires an
  explicit testing-only acknowledgement. Microsoft currently restricts
  attestation to testing scenarios; production release requires HLK/WHCP.
- `tools/Test-ViiperUdeSignedPackage.ps1` validates the Microsoft-returned
  driver and catalog against kernel signing policy, proves that INF and SYS are
  members of that exact catalog, distinguishes testing-only attestation from
  production HLK/WHCP signatures, and binds the returned INF/PDB to the
  reviewed source-revision manifest. The PDB stays in that certification
  evidence artifact; the installable runtime bundle contains only the
  validated INF/SYS/CAT plus the pinned manifest.
- `tools/Invoke-ViiperUdeLiveValidation.ps1` hash-binds that verified package
  to the installed service image and root devnode, then exercises every
  production controller through the real UdeCx host, direct interrupt-input
  path, generation teardown, and driver fault counters. It never installs or
  changes a driver.
- `tools/Invoke-ViiperUdePerformanceValidation.ps1` wraps that exact signed
  live gate in a uniquely named, bounded-memory Windows Performance Recorder
  session. It preserves an ETL on success or workload failure without stopping
  another recorder instance, enabling CPU sampled/precise, ready-thread,
  context-switch, WDF DPC, interrupt, and ISR analysis before performance code
  is changed.
- `tools/Enable-ViiperUdeVerifierForNextBoot.ps1` stages Microsoft standard
  Driver Verifier checks for `ViiperUde.sys` for exactly one boot. It refuses
  daily-use machines unless the disposable-machine acknowledgement is given,
  refuses to replace another driver's verifier configuration, and never
  restarts the machine.
- `tools/ViiperUdeMediaProbe.cpp` is a dependency-free CoreAudio live probe.
  It snapshots active endpoints before a virtual PlayStation controller is
  created, opens exactly the new render/capture pair concurrently through
  WASAPI, and lets the signed-driver test require real ISO traffic and bytes in
  both directions rather than treating endpoint enumeration as media success.
- `tools/ViiperUdeInputProbe.cpp` follows Microsoft's HIDClass discovery and
  continuous `ReadFile`/`WriteFile` contracts. It snapshots existing HID
  collections, opens only the newly enumerated matching gamepad, timestamps
  unique state markers with the system-wide performance counter, and writes a
  versioned feedback marker containing rumble, LEDs, and adaptive-trigger
  state. The signed live gate therefore measures the complete
  publisher-to-Windows-HID path and proves the reverse HIDClass-to-device
  path instead of trusting internal queue approximations.
- ABI, lifecycle, descriptor, cancellation, and fault tests live beside the Go
  broker packages and in the native-driver CI gates.

The interrupt-IN path follows ViGEmBus's useful pending-read principle without
copying its target-specific implementation. Each endpoint owns a preallocated,
sequence-checked latest-state cache. A report arriving before a Windows poll is
retained and completed after KMDF's manual-queue ready notification crosses a
preallocated passive work-item boundary; the notification itself can run
synchronously on UdeCx's submitter thread and therefore never completes the URB.
One token permits exactly one later cached completion, so the successor poll is
left parked for the next producer instead of replaying the cache in a busy loop.
Reset, purge, D0 exit, and device reset invalidate both the cache and token
behind the same admission barriers used by the direct producer. This removes
the old lost-rendezvous window and never requires an extra feeder update to wake
the first already-posted host poll.
The Go publisher likewise owns one descriptor-sized buffer per active endpoint.
Every production HID engine encodes directly into that buffer, which is reused
only after the overlapped IOCTL has completed and the kernel has copied the
report. Allocation gates enforce zero heap allocations in those report
encoders; USB/IP and third-party device engines retain their existing ownership
contract through the optional interface.
DualSense and DualShock 4 microphone engines use the same optional
caller-buffer rule for native isochronous IN. The broker invokes them only at
the endpoint's reserved service time, and they write directly into the current
URB packet region without a second timer or packet allocation. Nominal-only URB
capacity never causes an adaptive long packet to be consumed and truncated.
`InputReportsSubmitted` counts accepted state publications and
`InputReportsCompleted` counts host polls served from them. Multiple publications
can coalesce into one latest state before Windows polls, but one publication can
never manufacture multiple completions.

The design and release gates are in
`docs/architecture/native-udecx.md`. The Microsoft signing boundary is in
`docs/architecture/native-udecx-signing.md`.

Production installation is intentionally available only through the signed
package orchestrator, which binds the broker/helper/manifest hashes and keeps
the driver rollback snapshot alive through authenticated broker health. An
operator can run the same read-only production preflight without mutation:

```powershell
$manifest = 'C:\ViiperUde\ViiperUde.cab.sha256.json'
$deadline = [DateTimeOffset]::UtcNow.AddMinutes(4).ToUnixTimeMilliseconds()
.\ViiperUdeCtl.exe verify C:\ViiperUde\Signed\ViiperUde.inf `
  --manifest $manifest `
  --manifest-sha256 (Get-FileHash -Algorithm SHA256 -LiteralPath $manifest).Hash `
  --source-revision 0123456789abcdef0123456789abcdef01234567 `
  --validation-mode production `
  --transaction-deadline-unix-ms $deadline
```

The only forced selection available to an operator is an intentional downgrade
guarded by the exact currently installed version, for example
`--allow-controlled-downgrade 0.2.0.0`. Rollback may internally force the exact
previously captured signed INF because returning to that known state is the
transaction's recovery operation. Exit `0` means verified success, `3010`
means verified installation/removal requires a restart, `4` is a preflight
rejection, and `3` means rollback itself failed. Every command emits one final
key/value result line including `rebootRequired` and rollback status.

Production uninstall is similarly owned by the signed installer. It calls
`viiper uninstall` with the packaged `ViiperUdeCtl.exe`, the installer-bound
helper SHA-256, and the target-user SID. The broker is only stopped while the
helper transaction runs; its SCM registration, credential, and managed files
remain available for exact restart unless removal succeeds. For a direct
operator inspection of the helper boundary, use a cooperative deadline:

```powershell
$deadline = [DateTimeOffset]::UtcNow.AddMinutes(4).ToUnixTimeMilliseconds()
.\ViiperUdeCtl.exe remove --transaction-deadline-unix-ms $deadline
```

Only exit `0` or `3010` authorizes exact broker/credential/file cleanup. A
preflight rejection or verified no-reboot `rollback=succeeded` preserves the
prior broker run-state; a reboot-pending or unverified rollback leaves it
stopped for explicit reconciliation.
The production outer command never removes legacy tasks, Run registrations, or
USB/IP state.

After a Microsoft-signed native driver package has been installed and verified,
the developer-only standalone registration can persist the preview transport:

```powershell
$env:VIIPER_DEVELOPER_STANDALONE = '1'
.\viiper.exe install --transport native-ude
```

This skips the USB/IP runtime prerequisite and records
`server --transport native-ude` in the startup command. It does not install or
trust an unsigned kernel driver. The default remains `usbip` until the signed
live-driver gates in the architecture document pass.

On a disposable elevated test machine, validate an already-installed
Microsoft-signed package with:

```powershell
.\native\udecx\tools\Invoke-ViiperUdeLiveValidation.ps1 `
  -SignedPackageDirectory C:\ViiperUde\MicrosoftSigned `
  -SubmissionManifestPath C:\ViiperUde\ViiperUde.cab.sha256.json `
  -ExpectedSourceRevision 0123456789abcdef0123456789abcdef01234567 `
  -SignatureValidationMode Production `
  -Iterations 10 `
  -MediaDurationSeconds 30 `
  -MediaProbePath .\native\udecx\x64\Release\ViiperUdeMediaProbe.exe `
  -InputProbePath .\native\udecx\x64\Release\ViiperUdeInputProbe.exe `
  -ProbeManifestPath .\native\udecx\x64\Release\ViiperUdeLiveProbes.manifest.json
```

The command refuses an unsigned package, a package/service hash mismatch, a
non-Microsoft root devnode, a dirty driver session, or any increase in invalid
messages, queue exhaustion, notification overflow, late completion, or cleanup
retry counters. Production mode also requires this repository to be an exact,
clean checkout of `-ExpectedSourceRevision` and runs the Go harness with module,
workspace, environment, and toolchain overrides disabled. After validating
each controller and repeated generation
rollover independently, it enumerates the complete production controller set,
publishes input, and removes every child concurrently. A subprocess then exits
without running cleanup; the driver must remove its child, drain pending URBs,
release exclusive ownership, and accept a fresh session. Normal CI never opts
into this live test.
When `-MediaProbePath` is supplied, the first DualShock 4 and DualSense
generation must also create one new render/capture endpoint pair. Simultaneous
WASAPI render/capture must preserve the controller's declared format and frame
cadence, keep the render buffer nonempty, preserve monotonic capture clocks,
report no capture discontinuity/timestamp flags, and increase native ISO,
host-to-device, and device-to-host byte counters. The baseline snapshot prevents
a connected physical controller from being mistaken for the virtual device.
When `-InputProbePath` is supplied, the first DualShock 4, DualSense, and
DualSense Edge generations each publish 256 alternating stick markers. QPC is
sampled immediately before publication and when a continuous HID `ReadFile`
observes the matching report. The release gate requires p95 <= 4 ms,
p99 <= 8 ms, and maximum <= 20 ms, including user-mode scheduling, the native
IOCTL, UdeCx, and HIDClass. These are measured long-tail limits, not claims
derived from the nominal USB polling interval. The same newly enumerated HID
collection must then accept a full-length overlapped `WriteFile`; exact
DualShock 4 rumble/lightbar data or exact DualSense rumble, lightbar, player
LED, and left/right adaptive-trigger data must arrive at the corresponding
VIIPER device callback, and the driver's completion and host-to-device byte
counters must advance.
On Windows 10 2004 or newer, `-RestartRootDevice -DisposableTestMachine`
restarts the exact signed root devnode with a live DualSense child and input
publisher. The invalidated owner must terminate, the restarted controller must
return with zero children and pending requests, and a fresh exclusive session
must re-enumerate, service input, and tear down cleanly. No wildcard or
hardware-ID-wide PnP operation is used.

The Driver Verifier pass is a separate, explicit disposable-machine gate:

```powershell
.\native\udecx\tools\Enable-ViiperUdeVerifierForNextBoot.ps1 `
  -SignedPackageDirectory C:\ViiperUde\MicrosoftSigned `
  -SubmissionManifestPath C:\ViiperUde\ViiperUde.cab.sha256.json `
  -ExpectedSourceRevision 0123456789abcdef0123456789abcdef01234567 `
  -SignatureValidationMode Production `
  -DisposableTestMachine
# Restart once, then:
.\native\udecx\tools\Invoke-ViiperUdeLiveValidation.ps1 `
  -SignedPackageDirectory C:\ViiperUde\MicrosoftSigned `
  -SubmissionManifestPath C:\ViiperUde\ViiperUde.cab.sha256.json `
  -ExpectedSourceRevision 0123456789abcdef0123456789abcdef01234567 `
  -SignatureValidationMode Production `
  -Iterations 3 `
  -ReleaseGate `
  -RequireDriverVerifier `
  -RestartRootDevice `
  -DisposableTestMachine `
  -MediaDurationSeconds 180 `
  -MediaProbePath .\native\udecx\x64\Release\ViiperUdeMediaProbe.exe `
  -InputProbePath .\native\udecx\x64\Release\ViiperUdeInputProbe.exe `
  -ProbeManifestPath .\native\udecx\x64\Release\ViiperUdeLiveProbes.manifest.json
```

`-ReleaseGate` is fail-closed: it requires a 64-bit Windows 11 client with
Secure Boot, a production Microsoft signature, Driver Verifier `/standard`
including KMDF verification, three lifecycle generations, both independent
source/hash-bound probes, an active root-device restart, the disposable-machine
acknowledgement, and a three-minute clean duplex media run for each PlayStation
controller, including DualSense Edge. Omitting any one of those inputs cannot
print a production-pass result.

Microsoft warns that Driver Verifier can intentionally bugcheck a machine;
this workflow is never run by ordinary CI, an installer, or DS4Windows.

For evidence-based CPU and scheduler analysis, run the same signed workload
inside WPR's bounded `GeneralProfile.Verbose` memory profile. The verbose form
is required because the light form records scheduler events but omits the
CSwitch, ReadyThread, and sampled-profile stacks needed to attribute latency:

```powershell
.\native\udecx\tools\Invoke-ViiperUdePerformanceValidation.ps1 `
  -SignedPackageDirectory C:\ViiperUde\MicrosoftSigned `
  -SubmissionManifestPath C:\ViiperUde\ViiperUde.cab.sha256.json `
  -ExpectedSourceRevision 0123456789abcdef0123456789abcdef01234567 `
  -SignatureValidationMode Production `
  -OutputPath C:\ViiperUde\Traces\native-ude.etl `
  -MediaProbePath .\native\udecx\x64\Release\ViiperUdeMediaProbe.exe `
  -InputProbePath .\native\udecx\x64\Release\ViiperUdeInputProbe.exe `
  -ProbeManifestPath .\native\udecx\x64\Release\ViiperUdeLiveProbes.manifest.json
```

Open the ETL in Windows Performance Analyzer and inspect CPU Usage (Sampled),
CPU Usage (Precise), scheduler stacks, and DPC/ISR module activity. A non-empty
ETL is evidence capture, not a performance pass: acceptance still requires WPA
analysis against the architecture thresholds. The adjacent `.evidence.json`
hash-binds the ETL, signed-package manifest, exact source revision, and both
probes. The script rejects dropped events, never uses WPR file mode (which
Microsoft documents as unbounded), and never mutates an unnamed or foreign
recording session.
