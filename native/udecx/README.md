# VIIPER native UdeCx bus

This directory contains the Windows native USB device-emulation layer. It is
developed on `feature/native-udecx-bus`; it does not alter the supported USB/IP
implementation on `main` while the native path is incomplete.

Directory contract:

- `include/` is the stable C ABI shared by the driver and Go broker.
- `driver/` is the KMDF/UdeCx controller driver.
- `package/` contains INF and installation metadata.
- `tools/ViiperUdeCtl.cpp` installs, verifies, or removes the exact root
  controller without creating duplicates or leaving a failed devnode behind.
- `tools/New-ViiperUdeAttestationPackage.ps1` creates and hash-verifies the
  exact controlled-test Hardware Dev Center CAB structure and requires an
  explicit testing-only acknowledgement. Microsoft currently restricts
  attestation to testing scenarios; production release requires HLK/WHCP.
- `tools/Test-ViiperUdeSignedPackage.ps1` validates the Microsoft-returned
  driver and catalog against kernel signing policy.
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
  continuous `ReadFile` contract. It snapshots existing HID collections,
  opens only the newly enumerated matching gamepad, and timestamps unique
  state markers with the system-wide performance counter. The signed live
  gate measures the complete publisher-to-Windows-HID path instead of an
  internal queue approximation.
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
  -Iterations 10 `
  -MediaProbePath .\native\udecx\x64\Release\ViiperUdeMediaProbe.exe `
  -InputProbePath .\native\udecx\x64\Release\ViiperUdeInputProbe.exe
```

The command refuses an unsigned package, a package/service hash mismatch, a
non-Microsoft root devnode, a dirty driver session, or any increase in invalid
messages, queue exhaustion, notification overflow, late completion, or cleanup
retry counters. After validating each controller and repeated generation
rollover independently, it enumerates the complete production controller set,
publishes input, and removes every child concurrently. A subprocess then exits
without running cleanup; the driver must remove its child, drain pending URBs,
release exclusive ownership, and accept a fresh session. Normal CI never opts
into this live test.
When `-MediaProbePath` is supplied, the first DualShock 4 and DualSense
generation must also create one new render/capture endpoint pair; three seconds
of simultaneous WASAPI render/capture must increase native ISO, host-to-device,
and device-to-host byte counters. The baseline snapshot prevents a connected
physical controller from being mistaken for the virtual device.
When `-InputProbePath` is supplied, the first DualShock 4, DualSense, and
DualSense Edge generations each publish 256 alternating stick markers. QPC is
sampled immediately before publication and when a continuous HID `ReadFile`
observes the matching report. The release gate requires p95 <= 4 ms,
p99 <= 8 ms, and maximum <= 20 ms, including user-mode scheduling, the native
IOCTL, UdeCx, and HIDClass. These are measured long-tail limits, not claims
derived from the nominal USB polling interval.
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
  -DisposableTestMachine
# Restart once, then:
.\native\udecx\tools\Invoke-ViiperUdeLiveValidation.ps1 `
  -SignedPackageDirectory C:\ViiperUde\MicrosoftSigned `
  -Iterations 10 `
  -RequireDriverVerifier `
  -RestartRootDevice `
  -DisposableTestMachine
```

Microsoft warns that Driver Verifier can intentionally bugcheck a machine;
this workflow is never run by ordinary CI, an installer, or DS4Windows.

For evidence-based CPU and scheduler analysis, run the same signed workload
inside WPR's bounded `GeneralProfile.Light` memory profile:

```powershell
.\native\udecx\tools\Invoke-ViiperUdePerformanceValidation.ps1 `
  -SignedPackageDirectory C:\ViiperUde\MicrosoftSigned `
  -OutputPath C:\ViiperUde\Traces\native-ude.etl `
  -MediaProbePath .\native\udecx\x64\Release\ViiperUdeMediaProbe.exe `
  -InputProbePath .\native\udecx\x64\Release\ViiperUdeInputProbe.exe
```

Open the ETL in Windows Performance Analyzer and inspect CPU Usage (Sampled),
CPU Usage (Precise), and DPC/ISR by module and stack. The script never uses
WPR file mode, which Microsoft documents as unbounded, and never mutates an
unnamed or foreign recording session.
