# Native UDE official-source pins

This file records the primary Microsoft contracts used by the native UDE
implementation and transaction model. Links are pinned to immutable source
commits so a later documentation edit cannot silently change the reviewed
release basis. The corresponding Microsoft Learn pages remain useful for
navigation, but they are not the immutable evidence references.

Pins captured on 2026-08-15:

- `MicrosoftDocs/windows-driver-docs`:
  `5bf16a2a190814adbda0826aba1daf74faa1d45c`
- `MicrosoftDocs/windows-driver-docs-ddi`:
  `7515063cea4c9e98db6a92986c5b4ddb0463fd16`
- `MicrosoftDocs/sdk-api`:
  `4502fff176b3b56beddb6a63c9f980377b11ba9b`

## UDE ownership, completion, and power

- [Write a UDE client driver](https://github.com/MicrosoftDocs/windows-driver-docs/blob/5bf16a2a190814adbda0826aba1daf74faa1d45c/windows-driver-docs-pr/usbcon/writing-a-ude-client-driver.md)
  is the authority for class-extension ownership of the associated endpoint
  queue state, the client's forwarded-I/O purge obligation, START reopening,
  and separate-DPC URB completion. It is the basis for treating PURGE as the
  upstream admission boundary while joining driver-owned callbacks and
  forwarded operations without calling WDF queue-state mutation APIs.
- [Asynchronous link-power exit completion](https://github.com/MicrosoftDocs/windows-driver-docs-ddi/blob/7515063cea4c9e98db6a92986c5b4ddb0463fd16/wdk-ddi-src/content/udecxusbdevice/nf-udecxusbdevice-udecxusbdevicelinkpowerexitcomplete.md)
  requires PASSIVE_LEVEL completion after the client has finished its low-power
  transition. This is the basis for the preallocated passive D0-exit worker and
  its completion-as-final-object-access rule.
- [WdfWorkItemFlush](https://github.com/MicrosoftDocs/windows-driver-docs-ddi/blob/7515063cea4c9e98db6a92986c5b4ddb0463fd16/wdk-ddi-src/content/wdfworkitem/nf-wdfworkitem-wdfworkitemflush.md)
  waits for queued and already-running callbacks and is PASSIVE_LEVEL only.
- [WDF object cleanup](https://github.com/MicrosoftDocs/windows-driver-docs-ddi/blob/7515063cea4c9e98db6a92986c5b4ddb0463fd16/wdk-ddi-src/content/wdfobject/nc-wdfobject-evt_wdf_object_context_cleanup.md)
  defines child-before-parent cleanup and the work-item callback lifetime fence.
  Together, these two contracts require flushing device work before consuming
  a UDE device handle and allow an endpoint-parented purge worker to finish its
  counted callback before endpoint cleanup.

## Driver-package transaction

- [SetupCopyOEMInfW](https://github.com/MicrosoftDocs/sdk-api/blob/4502fff176b3b56beddb6a63c9f980377b11ba9b/sdk-api-src/content/setupapi/nf-setupapi-setupcopyoeminfw.md)
  supplies the add-only stage operation and the documented
  `SP_COPY_NOOVERWRITE`/`ERROR_FILE_EXISTS` receipt behavior.
- [DiInstallDevice](https://github.com/MicrosoftDocs/sdk-api/blob/4502fff176b3b56beddb6a63c9f980377b11ba9b/sdk-api-src/content/newdev/nf-newdev-diinstalldevice.md)
  binds an explicitly selected, already preinstalled driver to the exact
  present devnode and returns an authoritative reboot requirement.
- [DiUninstallDevice](https://github.com/MicrosoftDocs/sdk-api/blob/4502fff176b3b56beddb6a63c9f980377b11ba9b/sdk-api-src/content/newdev/nf-newdev-diuninstalldevice.md)
  removes the selected devnode and returns a reboot requirement that must remain
  durable until a later boot proves the requested removal settled.
- [SetupUninstallOEMInfW](https://github.com/MicrosoftDocs/sdk-api/blob/4502fff176b3b56beddb6a63c9f980377b11ba9b/sdk-api-src/content/setupapi/nf-setupapi-setupuninstalloeminfw.md)
  removes one exact published package. The transaction uses flags zero and
  never force-deletes a package still used by a device.
- [SetupDiCreateDeviceInfoW](https://github.com/MicrosoftDocs/sdk-api/blob/4502fff176b3b56beddb6a63c9f980377b11ba9b/sdk-api-src/content/setupapi/nf-setupapi-setupdicreatedeviceinfow.md)
  and [SetupDiGetDeviceInstanceIdW](https://github.com/MicrosoftDocs/sdk-api/blob/4502fff176b3b56beddb6a63c9f980377b11ba9b/sdk-api-src/content/setupapi/nf-setupapi-setupdigetdeviceinstanceidw.md)
  establish that a generated root instance identity is available before device
  registration. The install journal therefore persists that exact receipt
  before registration can leave a partially created root.

## Broker rollback material

- [CryptProtectData](https://github.com/MicrosoftDocs/sdk-api/blob/4502fff176b3b56beddb6a63c9f980377b11ba9b/sdk-api-src/content/dpapi/nf-dpapi-cryptprotectdata.md)
  defines machine-scoped protected rollback blobs. The journal additionally
  relies on protected directories, exact ACLs, per-transaction entropy, hashes,
  and no plaintext secret fields; DPAPI alone is not the access-control layer.
- [ReplaceFileW](https://github.com/MicrosoftDocs/sdk-api/blob/4502fff176b3b56beddb6a63c9f980377b11ba9b/sdk-api-src/content/winbase/nf-winbase-replacefilew.md)
  supplies the one-name image replacement primitive used after durable capture
  of the prior broker image. Journal records and backup files are separately
  flushed and read back before any service, key, image, or ownership mutation.

These sources define API behavior, not the repository's complete safety proof.
The release proof also requires the checked-in state-machine contracts, injected
cut-point models, immutable package manifest, live lifecycle/Verifier matrix,
and the absence of any unresolved recovery journal.
