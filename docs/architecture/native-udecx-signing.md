# Native UDE driver signing and release contract

The native VIIPER bus is a kernel driver. Shipping an Authenticode-signed EXE
does not make the driver loadable on modern Secure Boot Windows. A production
package must be signed by Microsoft through Hardware Dev Center.

## Supported release paths

### Desktop preview: attestation signing

Attestation signing is the shortest supported path for a Windows 10/11 Desktop
preview. It is not Windows Certified, cannot be distributed to retail users by
Windows Update, and does not support Windows Server 2016 or later.

1. Build the exact x64 Release driver, INF, PDB, and catalog.
2. Run `native/udecx/tools/New-ViiperUdeAttestationPackage.ps1` with explicit
   paths to those four artifacts. The script validates the INF contract,
   creates the required non-root `ViiperUde` folder in the CAB, re-extracts the
   CAB, verifies every SHA-256 hash, and writes a sidecar hash manifest.
3. Sign the CAB with a SHA-256 code-signing certificate registered to the
   organization's Hardware Dev Center account. Establishing that account and
   submitting attestation packages requires a currently valid EV certificate.
4. Submit the signed CAB in Partner Center with test-signing options disabled.
5. Download Microsoft's returned package and run
   `native/udecx/tools/Test-ViiperUdeSignedPackage.ps1`. It requires valid
   Microsoft kernel-policy signatures on both the SYS and catalog and reruns
   INF verification when the WDK tool is available.
6. Hash-lock only that validated Microsoft-signed package into the installer.

The structural CAB produced by CI is not installable production media. It has
not been EV-signed, submitted to Microsoft, or returned with Microsoft's
signature. CI names it accordingly and never promotes it as a release driver.

### Production certification: HLK/WHCP

HLK/WHCP is the production target. It covers Windows Server and is the route
required for retail Windows Update publication. Run the controller and child
devices through the applicable Device Fundamentals, USB, HID, audio, power,
reliability, and security playlists, submit the resulting HLKX package, and
validate the dashboard-signed result with the same local validation script.

## Package invariants

- The CAB has no files at its root. Its only driver folder is `ViiperUde`.
- The package contains exactly one `ViiperUde.inf`, `ViiperUde.sys`,
  `ViiperUde.pdb`, and `ViiperUde.cat` selected by explicit path.
- The INF targets only `ROOT\VIIPER\UDE`, copies only `ViiperUde.sys`, and
  names only `ViiperUde.cat`.
- The build and submission hash manifests identify the exact reviewed bits.
- Test certificates, test-signing state, or disabled Secure Boot are never a
  release prerequisite.
- The installer refuses an unsigned, test-signed, mismatched, downgraded, or
  non-Microsoft driver package before any driver-store mutation.
- Updating a live kernel package remains a reboot-safe transaction; it is not
  overwritten in place.

## Current release gate

The branch currently proves compilation, static analysis, ABI/lifecycle tests,
fuzzing, race tests, deterministic package structure, and payload hashing. A
native driver is not production-ready until the Microsoft-signed package also
passes Driver Verifier, HLK or the scoped attestation test matrix, repeated
install/update/rollback, process crash, sleep/resume, and multi-controller
media soak on a disposable test machine.

The first repeatable signed-driver gate is
`native/udecx/tools/Invoke-ViiperUdeLiveValidation.ps1`. It validates the
Microsoft-returned package, requires the installed service image to have the
same SHA-256 hash, requires exactly one Microsoft-signed root devnode, and then
runs the opt-in Windows integration test against Xbox 360, DualShock 4,
DualSense, DualSense Edge, and Switch 2 Pro production descriptors. Every
generation must enumerate, complete direct interrupt-input reports, tear down
to zero active devices and pending operations, and leave all protocol/fault
counters unchanged. The script does not install a driver or enable Driver
Verifier. It also kills a subprocess that owns an enumerated DualSense and
requires kernel file cleanup to remove the child, drain pending operations,
release exclusive ownership, and accept a fresh session. Driver Verifier
remains an explicit disposable-machine operation. The companion
`Enable-ViiperUdeVerifierForNextBoot.ps1` script first repeats the signed
package/service hash binding, refuses to replace verifier settings for another
driver, and stages Microsoft's standard checks for `ViiperUde.sys` in
`oneboot` mode. After the restart, live validation with
`-RequireDriverVerifier` refuses to run unless `verifier /query` proves the
driver is actually being verified. Neither script restarts the computer.
The optional `ViiperUdeMediaProbe.exe` gate snapshots active CoreAudio endpoints
before enumeration, requires exactly one newly-active render/capture pair for
DualShock 4 and DualSense, drives both simultaneously through event-mode WASAPI,
and requires the driver's ISO packet, OUT-byte, and IN-byte counters all to
advance. This distinguishes a visible-but-nonfunctional audio endpoint from a
working full-duplex bus and avoids confusing an already-connected physical pad
with the newly-created virtual one.
The opt-in root-restart gate uses Microsoft's `pnputil /restart-device` only
against the one package-verified VIIPER instance ID and only on an explicitly
acknowledged disposable machine. It keeps a DualSense child and direct-input
publisher active across removal, requires the old owner session to terminate,
then hash-preserving PnP restart must expose a clean controller that accepts a
new exclusive owner, re-enumerates the child, services input, and drains to
zero again. Windows 10 1809 remains a supported runtime target, but this
particular automated gate requires Windows 10 2004 because that is when
Microsoft added `pnputil /restart-device`; 1809 power/PnP coverage belongs in
the HLK/DevFund matrix.

## Primary Microsoft references

- [Driver code-signing requirements](https://learn.microsoft.com/windows-hardware/drivers/dashboard/code-signing-reqs)
- [Attestation-sign Windows drivers](https://learn.microsoft.com/windows-hardware/drivers/dashboard/code-signing-attestation)
- [Driver-signing options and best practices](https://learn.microsoft.com/windows-hardware/drivers/dashboard/driver-signing-offerings)
- [Driver Verifier](https://learn.microsoft.com/windows-hardware/drivers/devtest/driver-verifier)
- [Driver Verifier command syntax](https://learn.microsoft.com/windows-hardware/drivers/devtest/verifier-command-line)
- [PnPUtil command syntax](https://learn.microsoft.com/windows-hardware/drivers/devtest/pnputil-command-syntax)
