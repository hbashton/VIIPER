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
  exact Hardware Dev Center CAB structure; it does not pretend that an
  unsigned CI artifact is a production driver.
- `tools/Test-ViiperUdeSignedPackage.ps1` validates the Microsoft-returned
  driver and catalog against kernel signing policy.
- `tools/Invoke-ViiperUdeLiveValidation.ps1` hash-binds that verified package
  to the installed service image and root devnode, then exercises every
  production controller through the real UdeCx host, direct interrupt-input
  path, generation teardown, and driver fault counters. It never installs or
  changes a driver.
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
retained and completed by KMDF's manual-queue ready notification; reset, purge,
D0 exit, and device reset invalidate it behind the same admission barriers used
by the direct producer. This removes the old lost-rendezvous window and never
requires an extra feeder update to wake an already-posted host poll.
`InputReportsSubmitted` counts accepted state publications and
`InputReportsCompleted` counts host polls served from that cache, so a stable
state may produce more completions than submissions by design.

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
