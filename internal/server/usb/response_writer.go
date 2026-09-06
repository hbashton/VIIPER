package usb

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/usbip"
)

const isoPacketDescriptorSize = 16

// responseWriter is the sole owner of a USB/IP connection's response stream.
// Existing callers build a complete response in endpoint-owned memory before
// calling write. The dormant writeLateRetSubmit path is the sole opt-in
// exception: it permits bounded final preparation and terminal completion
// while socket ownership is held. Existing report encoding, ISO descriptor
// construction, and device work remain outside the send lock.
type responseWriter struct {
	mu      sync.Mutex
	dst     io.Writer
	batcher *batchingWriter

	responseQueueWait durationHistogram
	sendLockWait      durationHistogram
	socketWrite       durationHistogram
}

// lateResponseResult is the final USB/IP result selected only after a caller
// owns responseWriter.mu. The all-zero value is intentionally Pending: a
// retained job may retry without emitting or completing anything.
type lateResponseResult uint8

const (
	lateResponsePending lateResponseResult = iota
	lateResponseData
	lateResponseSuccess
	lateResponseStall
)

// lateResponseSelection is returned by lateResponsePrepareFunc while response
// serialization is owned. For Data, ActualLength is both the response payload
// length and the USB/IP actual_length and must fit the caller's early maximum
// payload bound. Success has no response payload; its ActualLength may instead
// acknowledge an already-validated host-to-device transfer, and zero is a
// successful ZLP. Pending and Stall require an ActualLength of zero.
type lateResponseSelection struct {
	Result       lateResponseResult
	ActualLength uint32
}

// lateResponsePrepareFunc performs final result selection under
// responseWriter.mu. Payload has the exact length and capacity of the caller's
// validated early maximum and is cleared before every invocation. The callback
// must be fixed-cost, must not perform I/O or call responseWriter recursively,
// and must not retain Payload or publish an irreversible effect. Returning
// Pending causes no write, flush, or completion callback.
//
// A named function type keeps this synchronous boundary available to hot-path
// callers without requiring an interface conversion or heap allocation.
type lateResponsePrepareFunc func(payload []byte) (lateResponseSelection, error)

// lateResponseCompleteFunc receives the terminal delivery outcome under
// responseWriter.mu after writeFull and the mandatory flush have finished.
// It is called exactly once for a structurally valid non-Pending selection and
// must be fixed-cost, perform no I/O, and not call responseWriter recursively.
type lateResponseCompleteFunc func(delivered bool) error

// USBIPResponseDiagnostics contains aggregate serialization-stage latency for
// one attached USB/IP response stream.
type USBIPResponseDiagnostics struct {
	ResponseQueueWait DurationHistogramSnapshot
	SendLockWait      DurationHistogramSnapshot
	SocketWrite       DurationHistogramSnapshot
}

func (w *responseWriter) snapshot() USBIPResponseDiagnostics {
	return USBIPResponseDiagnostics{
		ResponseQueueWait: w.responseQueueWait.snapshot(),
		SendLockWait:      w.sendLockWait.snapshot(),
		SocketWrite:       w.socketWrite.snapshot(),
	}
}

func newResponseWriter(dst io.Writer, batcher *batchingWriter) *responseWriter {
	return &responseWriter{dst: dst, batcher: batcher}
}

func (w *responseWriter) write(packet []byte, flush bool, readyAt time.Time) error {
	_, err := w.writeIf(packet, flush, readyAt, nil)
	return err
}

// writeIf acquires send ownership and then calls claim immediately before the
// first byte can be emitted. Endpoint workers use this hand-off to make unlink
// ordering exact without holding a queue lock during socket I/O: either unlink
// cancels the logical response first, or the response owns the stream first
// and RET_UNLINK necessarily follows it.
func (w *responseWriter) writeIf(
	packet []byte,
	flush bool,
	readyAt time.Time,
	claim func() bool,
) (bool, error) {
	return w.writeIfThen(packet, flush, readyAt, claim, nil)
}

// writeIfThen is writeIf with an ownership-completion callback. afterWrite is
// invoked while send ownership is still held, after writeFull (and any
// required flush) has finished but before another response can validate
// state. The callback must be fixed-cost and must not perform I/O. This lets a
// successful interrupt presentation advance its device version before a
// waiting control GET_REPORT is allowed to validate a snapshot.
func (w *responseWriter) writeIfThen(
	packet []byte,
	flush bool,
	readyAt time.Time,
	claim func() bool,
	afterWrite func(success bool),
) (bool, error) {
	lockStarted := time.Now()
	if !readyAt.IsZero() {
		w.responseQueueWait.record(lockStarted.Sub(readyAt))
	}
	w.mu.Lock()
	lockedAt := time.Now()
	w.sendLockWait.record(lockedAt.Sub(lockStarted))
	if claim != nil && !claim() {
		if afterWrite != nil {
			afterWrite(false)
		}
		w.mu.Unlock()
		return false, nil
	}

	writeStarted := time.Now()
	err := writeFull(w.dst, packet)
	if err == nil && flush && w.batcher != nil {
		err = w.batcher.Flush()
	}
	writeCompleted := time.Now()
	if afterWrite != nil {
		afterWrite(err == nil)
	}
	w.mu.Unlock()

	w.socketWrite.record(writeCompleted.Sub(writeStarted))
	if err != nil {
		return true, fmt.Errorf("write USB/IP response: %w", err)
	}
	return true, nil
}

// writeLateRetSubmit is an opt-in serializer primitive for a retained caller
// that cannot choose a RET_SUBMIT result before it owns the response stream.
// Existing response paths do not call it. maxPayload is an early, validated
// upper bound; allocation and clearing happen before send ownership so the
// preparation callback receives one exact, zeroed payload window.
//
// A Pending selection returns the reusable scratch at length zero with
// written=false and performs no socket write, flush, or completion. A valid
// terminal selection returns the exact packet image with written=true even if
// delivery or completion fails. Neither another late preparation nor an
// existing write/writeIf/writeIfThen call can run until the mandatory flush
// and terminal completion callback have both finished.
//
// Preparation and completion panics are contained as errors. A preparation
// panic is pre-admission and therefore has no completion callback. A completion
// panic occurs after the terminal write attempt and is returned alongside any
// delivery error. In all cases send ownership is released.
func (w *responseWriter) writeLateRetSubmit(
	scratch []byte,
	seq uint32,
	maxPayload int,
	readyAt time.Time,
	prepare lateResponsePrepareFunc,
	complete lateResponseCompleteFunc,
) ([]byte, bool, error) {
	if prepare == nil {
		return scratch[:0], false, fmt.Errorf(
			"prepare late USB/IP response: nil callback")
	}
	if complete == nil {
		return scratch[:0], false, fmt.Errorf(
			"complete late USB/IP response: nil callback")
	}
	if maxPayload < 0 || maxPayload > maximumTransferSize ||
		uint64(maxPayload) > uint64(^uint32(0)) ||
		maxPayload > int(^uint(0)>>1)-retSubmitHeaderSize {
		return scratch[:0], false, fmt.Errorf(
			"prepare late USB/IP response: invalid maximum payload %d",
			maxPayload)
	}

	reservedLength := retSubmitHeaderSize + maxPayload
	if cap(scratch) < reservedLength {
		scratch = make([]byte, reservedLength)
	} else {
		// Do not return caller-owned capacity beyond the validated response
		// window. Besides preventing stale tail bytes from remaining visible
		// through the returned packet, this keeps clearing work bounded by the
		// early maximum rather than by an arbitrarily oversized backing array.
		scratch = scratch[:reservedLength:reservedLength]
		clear(scratch)
	}
	payload := scratch[retSubmitHeaderSize:reservedLength:reservedLength]

	lockStarted := time.Now()
	if !readyAt.IsZero() {
		w.responseQueueWait.record(lockStarted.Sub(readyAt))
	}
	w.mu.Lock()
	lockedAt := time.Now()
	w.sendLockWait.record(lockedAt.Sub(lockStarted))

	var socketWriteDuration time.Duration
	recordSocketWrite := false
	defer func() {
		w.mu.Unlock()
		if recordSocketWrite {
			w.socketWrite.record(socketWriteDuration)
		}
	}()

	selection, preparationErr := invokeLateResponsePreparation(
		prepare, payload)
	if preparationErr != nil {
		clear(payload)
		return scratch[:0], false, fmt.Errorf(
			"prepare late USB/IP response: %w", preparationErr)
	}

	status := int32(0)
	payloadLength := 0
	switch selection.Result {
	case lateResponsePending:
		if selection.ActualLength != 0 {
			clear(payload)
			return scratch[:0], false, fmt.Errorf(
				"prepare late USB/IP response: Pending actual length %d",
				selection.ActualLength)
		}
		clear(payload)
		return scratch[:0], false, nil
	case lateResponseData:
		if uint64(selection.ActualLength) > uint64(maxPayload) {
			clear(payload)
			return scratch[:0], false, fmt.Errorf(
				"prepare late USB/IP response: Data actual length %d exceeds maximum payload %d",
				selection.ActualLength, maxPayload)
		}
		payloadLength = int(selection.ActualLength)
	case lateResponseSuccess:
		// Success carries no response bytes. A nonzero ActualLength may
		// acknowledge an already-validated host-to-device transfer.
	case lateResponseStall:
		if selection.ActualLength != 0 {
			clear(payload)
			return scratch[:0], false, fmt.Errorf(
				"prepare late USB/IP response: Stall actual length %d",
				selection.ActualLength)
		}
		status = errPipe
	default:
		clear(payload)
		return scratch[:0], false, fmt.Errorf(
			"prepare late USB/IP response: invalid result %d",
			selection.Result)
	}

	clear(payload[payloadLength:])
	packet := scratch[:retSubmitHeaderSize+payloadLength]
	binary.BigEndian.PutUint32(packet[0:4], usbip.RetSubmitCode)
	binary.BigEndian.PutUint32(packet[4:8], seq)
	// devid, direction, endpoint, start_frame, and error_count remain zero.
	binary.BigEndian.PutUint32(packet[20:24], uint32(status))
	binary.BigEndian.PutUint32(packet[24:28], selection.ActualLength)
	binary.BigEndian.PutUint32(packet[32:36], ^uint32(0))

	writeStarted := time.Now()
	deliveryErr := writeFull(w.dst, packet)
	if deliveryErr == nil && w.batcher != nil {
		deliveryErr = w.batcher.Flush()
	}
	writeCompleted := time.Now()
	socketWriteDuration = writeCompleted.Sub(writeStarted)
	recordSocketWrite = true

	completionErr := invokeLateResponseCompletion(
		complete, deliveryErr == nil)
	switch {
	case deliveryErr != nil && completionErr != nil:
		return packet, true, fmt.Errorf(
			"write USB/IP response: %w; complete late USB/IP response: %w",
			deliveryErr, completionErr)
	case deliveryErr != nil:
		return packet, true, fmt.Errorf(
			"write USB/IP response: %w", deliveryErr)
	case completionErr != nil:
		return packet, true, fmt.Errorf(
			"complete late USB/IP response: %w", completionErr)
	default:
		return packet, true, nil
	}
}

func invokeLateResponsePreparation(
	prepare lateResponsePrepareFunc,
	payload []byte,
) (selection lateResponseSelection, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = lateResponseCallbackPanic("preparation", recovered)
		}
	}()
	return prepare(payload)
}

func invokeLateResponseCompletion(
	complete lateResponseCompleteFunc,
	delivered bool,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = lateResponseCallbackPanic("completion", recovered)
		}
	}()
	return complete(delivered)
}

func lateResponseCallbackPanic(stage string, recovered any) error {
	if recoveredErr, ok := recovered.(error); ok {
		return fmt.Errorf("%s callback panic: %w", stage, recoveredErr)
	}
	return fmt.Errorf("%s callback panic: %v", stage, recovered)
}

func writeFull(w io.Writer, packet []byte) error {
	for len(packet) > 0 {
		n, err := w.Write(packet)
		if n > 0 {
			packet = packet[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// buildRetSubmitPacket serializes one complete RET_SUBMIT into dst. The
// returned slice remains owned by the caller and must not be reused until
// responseWriter.write returns.
func buildRetSubmitPacket(
	dst []byte,
	seq uint32,
	status int32,
	actualLen uint32,
	respData []byte,
	isoPackets []usbip.IsoPacketDescriptor,
	isIso bool,
) []byte {
	total := retSubmitHeaderSize + len(respData)
	if isIso {
		total += len(isoPackets) * isoPacketDescriptorSize
	}
	if cap(dst) < total {
		dst = make([]byte, total)
	} else {
		dst = dst[:total]
		clear(dst)
	}

	binary.BigEndian.PutUint32(dst[0:4], usbip.RetSubmitCode)
	binary.BigEndian.PutUint32(dst[4:8], seq)
	// devid, direction, and endpoint are zero in USB/IP return headers.
	binary.BigEndian.PutUint32(dst[20:24], uint32(status))
	binary.BigEndian.PutUint32(dst[24:28], actualLen)
	packetCount := int32(-1)
	if isIso {
		packetCount = int32(len(isoPackets))
	}
	binary.BigEndian.PutUint32(dst[32:36], uint32(packetCount))

	offset := retSubmitHeaderSize
	copy(dst[offset:], respData)
	offset += len(respData)
	if isIso {
		for _, packet := range isoPackets {
			binary.BigEndian.PutUint32(dst[offset:offset+4], packet.Offset)
			binary.BigEndian.PutUint32(dst[offset+4:offset+8], packet.Length)
			binary.BigEndian.PutUint32(dst[offset+8:offset+12], packet.ActualLength)
			binary.BigEndian.PutUint32(dst[offset+12:offset+16], uint32(packet.Status))
			offset += isoPacketDescriptorSize
		}
	}
	return dst
}

// buildPreparedRetSubmitPacket reserves an exact, zeroed interrupt-IN payload
// without asking a source to materialize bytes. The caller gives the payload
// slice to PreparedSource only after response serialization is owned.
func buildPreparedRetSubmitPacket(dst []byte, seq uint32, payloadSize int) []byte {
	if payloadSize < 0 {
		payloadSize = 0
	}
	dst = buildRetSubmitPacket(
		dst, seq, 0, uint32(payloadSize), nil, nil, false,
	)
	total := retSubmitHeaderSize + payloadSize
	if cap(dst) < total {
		grown := make([]byte, total)
		copy(grown, dst)
		dst = grown
	} else {
		dst = dst[:total]
		clear(dst[retSubmitHeaderSize:])
	}
	return dst
}

func buildRetUnlinkPacket(dst []byte, seq uint32, status int32) []byte {
	if cap(dst) < retSubmitHeaderSize {
		dst = make([]byte, retSubmitHeaderSize)
	} else {
		dst = dst[:retSubmitHeaderSize]
		clear(dst)
	}
	binary.BigEndian.PutUint32(dst[0:4], usbip.RetUnlinkCode)
	binary.BigEndian.PutUint32(dst[4:8], seq)
	binary.BigEndian.PutUint32(dst[20:24], uint32(status))
	return dst
}
