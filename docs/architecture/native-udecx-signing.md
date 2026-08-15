# Native UDE driver signing and release contract

The native VIIPER bus is a kernel driver. Shipping an Authenticode-signed EXE
does not make the driver loadable on modern Secure Boot Windows. A production
package must be signed by Microsoft through Hardware Dev Center.

## Supported release paths

### Local development: WDK test signing

The manually dispatched native workflow can publish one compact,
seven-day-retention `LocalTest` artifact for the exact source SHA. It contains
the WDK test-signed INF/SYS/PDB/CAT evidence, an exact three-file runtime driver
directory, the source-bound broker/helper and live probes, the exported test
certificate, and a closed SHA-256 lock. Installation requires explicit
disposable-machine acknowledgement, elevation, the exact source revision and
interactive-user SID, and a current boot entry reporting `TESTSIGNING Yes`.
It imports only the artifact-bound certificate and then executes the normal
package-to-service transaction through `viiper.exe native-package-install`;
the helper is never invoked as a standalone mutation. Authenticated ABI 1.13,
capability, package-version, and loaded-kernel identity health must succeed
before the transaction commits.

This route is deliberately non-release (`releaseEligible=false`,
`signingRoute=LocalTest`). It cannot satisfy controlled-attestation or
production validation, is never consumed by release composition, and does not
change the requirement that even test-mode 64-bit drivers carry a valid test
signature.

### Controlled testing: attestation signing

Microsoft now documents attestation signing as **testing-only**. An
attestation-signed package is not Windows Certified and is not a supported
retail release path. It can be used only in Microsoft's documented controlled
testing scenarios (for example, CoDev or Test Registry Key / Surface SSRK), and
it does not support Windows Server 2016 or later. It must never be shipped as
the public VIIPER driver.

1. Build the exact x64 Release driver, INF, PDB, and catalog.
2. Run `native/udecx/tools/New-ViiperUdeAttestationPackage.ps1` with explicit
   paths to those four artifacts and `-AcknowledgeTestingOnly`. The script
   validates the INF contract,
   creates the required non-root `ViiperUde` folder in the CAB, re-extracts the
   CAB, verifies every SHA-256 hash, and writes a sidecar hash manifest bound
   to the required source revision.
3. Sign the CAB with a SHA-256 code-signing certificate registered to the
   organization's Hardware Dev Center account. Establishing that account and
   submitting attestation packages requires a currently valid EV certificate.
4. Submit the signed CAB through the applicable Partner Center testing flow.
5. Download Microsoft's returned package and run
   `native/udecx/tools/Test-ViiperUdeSignedPackage.ps1`. It requires valid
   Microsoft kernel-policy signatures on both the SYS and catalog, proves the
   INF and SYS are members of that exact catalog, requires the testing-only
   attestation EKU, binds the unchanged INF/PDB to the source-revision sidecar,
   and requires both WHQL-aligned and Universal INF verification.
6. Hash-lock only that validated Microsoft-signed package into the installer.

The structural CAB produced by CI is not installable production media. It has
not been EV-signed, submitted to Microsoft, or returned with Microsoft's
signature. Even a Microsoft attestation-signed result remains a controlled-test
artifact under the current Microsoft contract. CI names it accordingly and
never promotes it as a release driver.

### Production certification: HLK/WHCP

HLK/WHCP is the only VIIPER production target. Microsoft recommends HLK-tested,
dashboard-signed drivers for release; WHCP is required for retail Windows
Update publication. Run the controller and child devices through the
applicable Device Fundamentals, USB, HID, audio, power, reliability, and
security playlists, submit the resulting HLKX package, and validate the
dashboard-signed result with the same local validation script in `Production`
mode. That mode rejects the attestation EKU and requires a release-eligible
`HLK/WHCP` evidence manifest bound to the reviewed source revision.

## Package invariants

- The CAB has no files at its root. Its only driver folder is `ViiperUde`.
- The package contains exactly one `ViiperUde.inf`, `ViiperUde.sys`,
  `ViiperUde.pdb`, and `ViiperUde.cat` selected by explicit path.
- The INF targets only `ROOT\VIIPER\UDE`, copies only `ViiperUde.sys`, and
  names only `ViiperUde.cat`.
- The schema-2 submission manifest identifies the exact reviewed bits and the
  SHA-256 build identity derived from source revision, four-part DriverVer,
  ABI 1.13, and the exact capability mask. That same identity is compiled into
  the SYS that the signed catalog seals and is returned by the loaded kernel.
- Returned packages contain only the canonical INF, SYS, PDB, and CAT in one
  directory. The unchanged INF/PDB must match the submission manifest, and
  SignTool must prove INF/SYS membership in the returned Microsoft catalog.
- PDB is certification evidence, not a runtime dependency. The public native
  archive contains exactly the release `viiper.exe` broker,
  `ViiperUdeCtl.exe`, INF, SYS, CAT, and the validated submission manifest.
  Release composition rejects every missing or additional file.
- Local-test, controlled-test, and production signatures are separate
  validation modes; neither a local certificate nor an attestation EKU can
  satisfy the production release gate.
- Test certificates, test-signing state, or disabled Secure Boot are never a
  release prerequisite.
- The production installer refuses an unsigned, test-signed, mismatched,
  downgraded, or non-Microsoft driver package before any driver-store mutation.
- Updating a live kernel package remains a reboot-safe transaction; it is not
  overwritten in place.

## Current release gate

Feature branches, `main`, release tags, and the public release workflow all run
the native compile, static-analysis, ABI/lifecycle, fuzz, race, stamped-INF,
package-transaction, helper rollback/update/removal, and deterministic package
checks. Driver package source changes must strictly increase the four-part
`DriverVer` without regressing its date; a release also compares against the
previous SemVer tag. The `viiper uninstall` command now hash-locks and invokes
the helper's exact root-devnode/Driver Store removal transaction under the
package-then-service lock order, and its deterministic gates cover structured
success, reboot-success, preflight, verified rollback, indeterminate rollback,
and exact owned cleanup. Live uninstall on the Microsoft-signed package remains
part of the external acceptance matrix below rather than an implementation gap.

Production driver acceptance is separate and manual because Microsoft signing
is external. The intake workflow must run from the exact current `main` commit,
downloads one artifact by immutable run ID, artifact ID, and SHA-256 digest,
and validates the Microsoft-returned INF/SYS/PDB/CAT package in literal
`Production` mode. It rejects test signatures and the attestation EKU, verifies
catalog membership, validates the actual returned stamped INF against the
reviewed project, and publishes one source-named accepted artifact.

A tag release must point to the current `main` tip and cannot publish without a
successful production-intake run at that same commit. It downloads only that
accepted artifact plus the current release broker and source-built helper. A
mandatory Windows job signs both broker architectures and the helper with the
configured production Authenticode certificate, requires a trusted timestamp,
the exact certificate SHA-256 fingerprint, and Code Signing EKU, then validates
the exact six-file runtime bundle before and after archiving. Publication
consumes only those signed outputs, discards the certification PDB, and
allowlists every public release asset before checksumming and attesting it. The
test-signed CI artifact is never a release input.

These automation gates do not manufacture certification evidence. A native
driver is not production-ready until the external HLK/WHCP dashboard-signed
package also passes Driver Verifier, the complete HLK matrix, repeated live
install/update/rollback/uninstall, process crash, sleep/resume, and
multi-controller soak on a disposable test machine.

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
- [TESTSIGNING boot configuration](https://learn.microsoft.com/windows-hardware/drivers/install/the-testsigning-boot-configuration-option)
- [Install a test-signed driver package](https://learn.microsoft.com/windows-hardware/drivers/install/how-to-install-test-signed-driver-for-setup-and-boot)
- [Verify a test-signed catalog](https://learn.microsoft.com/windows-hardware/drivers/install/verifying-the-signature-of-a-test-signed-catalog-file)
- [Attestation-sign Windows drivers](https://learn.microsoft.com/windows-hardware/drivers/dashboard/code-signing-attestation)
- [Driver-signing options and best practices](https://learn.microsoft.com/windows-hardware/drivers/dashboard/driver-signing-offerings)
- [Components of a driver package](https://learn.microsoft.com/windows-hardware/drivers/install/components-of-a-driver-package)
- [Catalog files](https://learn.microsoft.com/windows-hardware/drivers/install/catalog-files)
- [Release-signing a driver package catalog](https://learn.microsoft.com/windows-hardware/drivers/install/release-signing-a-driver-package-s-catalog-file)
- [INF Version section](https://learn.microsoft.com/windows-hardware/drivers/install/inf-version-section)
- [SignTool command-line reference](https://learn.microsoft.com/windows-hardware/drivers/devtest/signtool)
- [Windows Hardware Lab Kit](https://learn.microsoft.com/windows-hardware/test/hlk/)
- [Add driver and supplemental content to an HLK package](https://learn.microsoft.com/windows-hardware/test/hlk/user/add-driver-and-supplemental-content-to-your-package)
- [INF DriverVer directive](https://learn.microsoft.com/windows-hardware/drivers/install/inf-driverver-directive)
- [InfVerif `/h`](https://learn.microsoft.com/windows-hardware/drivers/devtest/infverif_h)
- [Driver Verifier](https://learn.microsoft.com/windows-hardware/drivers/devtest/driver-verifier)
- [Driver Verifier command syntax](https://learn.microsoft.com/windows-hardware/drivers/devtest/verifier-command-line)
- [PnPUtil command syntax](https://learn.microsoft.com/windows-hardware/drivers/devtest/pnputil-command-syntax)
