// Package xboxone contains transport-neutral Xbox controller semantics and
// narrowly scoped, capture-pinned GIP message-body codecs.
//
// This package does not register a USB device, describe an Xbox persona,
// implement GIP framing, or perform authentication. Those boundaries remain
// fail-closed until an owned Windows capture and conformance oracle exist.
package xboxone
