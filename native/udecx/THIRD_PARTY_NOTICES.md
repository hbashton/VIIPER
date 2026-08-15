# Native UDE development references

The VIIPER native UdeCx implementation is original project code. The following
projects are used as protocol and architecture references:

- **ViGEmBus**, BSD 3-Clause: target lifecycle, request ownership, manual queues,
  concurrency, cancellation, and version negotiation.
- **usbip-win2**, BSD 2-Clause: documented UdeCx endpoint lifecycle and proof of
  bidirectional isochronous operation on Windows.
- **Microsoft Windows Driver Samples**, MIT: supported KMDF project and CI
  patterns.

No third-party binary is redistributed by this directory. Any source adapted in
the future must retain its applicable license notice in the affected file and
in packaged notices.
