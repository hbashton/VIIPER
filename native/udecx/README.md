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
- ABI, lifecycle, descriptor, cancellation, and fault tests live beside the Go
  broker packages and in the native-driver CI gates.

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
  -Iterations 10
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
