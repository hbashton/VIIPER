package usb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

func TestResponseWriterEmitsOneContiguousRetSubmit(t *testing.T) {
	recorder := newRecordingWriter()
	writer := newResponseWriter(recorder, nil)
	payload := []byte{0x11, 0x22, 0x33}
	packets := []usbip.IsoPacketDescriptor{{
		Offset: 0, Length: 4, ActualLength: 3, Status: 0,
	}}
	packet := buildRetSubmitPacket(nil, 77, 0, 3, payload, packets, true)

	require.NoError(t, writer.write(packet, true, time.Now()))
	writes := recorder.waitForWrites(t, 1)
	require.Len(t, writes, 1)
	require.Len(t, writes[0].packet, retSubmitHeaderSize+len(payload)+isoPacketDescriptorSize)
	require.Equal(t, uint32(usbip.RetSubmitCode), binary.BigEndian.Uint32(writes[0].packet[0:4]))
	require.Equal(t, uint32(77), binary.BigEndian.Uint32(writes[0].packet[4:8]))
	require.Equal(t, payload, writes[0].packet[retSubmitHeaderSize:retSubmitHeaderSize+len(payload)])
	descriptorOffset := retSubmitHeaderSize + len(payload)
	require.Equal(t, uint32(3), binary.BigEndian.Uint32(
		writes[0].packet[descriptorOffset+8:descriptorOffset+12],
	))
}

type shortWriter struct {
	maximum int
	writes  int
	data    []byte
}

func (w *shortWriter) Write(packet []byte) (int, error) {
	w.writes++
	n := min(len(packet), w.maximum)
	w.data = append(w.data, packet[:n]...)
	return n, nil
}

func TestResponseWriterUsesWriteFullForShortWrites(t *testing.T) {
	destination := &shortWriter{maximum: 7}
	writer := newResponseWriter(destination, nil)
	packet := buildRetUnlinkPacket(nil, 91, errConnReset)

	require.NoError(t, writer.write(packet, true, time.Now()))
	require.Equal(t, packet, destination.data)
	require.Greater(t, destination.writes, 1)
}

func TestLateResponseWriterSelectsExactTerminalPacketUnderLock(t *testing.T) {
	tests := []struct {
		name          string
		selection     lateResponseSelection
		data          []byte
		wantStatus    int32
		wantActual    uint32
		wantPayload   []byte
		wantPacketLen int
	}{
		{
			name: "variable data", selection: lateResponseSelection{
				Result: lateResponseData, ActualLength: 3,
			},
			data: []byte{0x11, 0x22, 0x33}, wantActual: 3,
			wantPayload:   []byte{0x11, 0x22, 0x33},
			wantPacketLen: retSubmitHeaderSize + 3,
		},
		{
			name: "zero-length data", selection: lateResponseSelection{
				Result: lateResponseData,
			},
			wantPacketLen: retSubmitHeaderSize,
		},
		{
			name: "success ZLP", selection: lateResponseSelection{
				Result: lateResponseSuccess,
			},
			wantPacketLen: retSubmitHeaderSize,
		},
		{
			name: "success acknowledges OUT bytes", selection: lateResponseSelection{
				Result: lateResponseSuccess, ActualLength: 7,
			},
			wantActual: 7, wantPacketLen: retSubmitHeaderSize,
		},
		{
			name: "stall", selection: lateResponseSelection{
				Result: lateResponseStall,
			},
			wantStatus: errPipe, wantPacketLen: retSubmitHeaderSize,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := newRecordingWriter()
			writer := newResponseWriter(recorder, nil)
			scratch := bytes.Repeat([]byte{0xa5}, retSubmitHeaderSize+8)
			completionCalls := 0
			completionDelivered := false

			packet, written, err := writer.writeLateRetSubmit(
				scratch, uint32(700+index), 8, time.Now(),
				func(payload []byte) (lateResponseSelection, error) {
					require.Len(t, payload, 8)
					require.Equal(t, 8, cap(payload))
					require.Equal(t, make([]byte, 8), payload,
						"preparation observed stale scratch bytes")
					copy(payload, test.data)
					return test.selection, nil
				},
				func(delivered bool) error {
					completionCalls++
					completionDelivered = delivered
					return nil
				},
			)

			require.NoError(t, err)
			require.True(t, written)
			require.Equal(t, 1, completionCalls)
			require.True(t, completionDelivered)
			require.Len(t, packet, test.wantPacketLen)
			require.Equal(t, uint32(usbip.RetSubmitCode),
				binary.BigEndian.Uint32(packet[0:4]))
			require.Equal(t, uint32(700+index),
				binary.BigEndian.Uint32(packet[4:8]))
			require.Equal(t, test.wantStatus,
				int32(binary.BigEndian.Uint32(packet[20:24])))
			require.Equal(t, test.wantActual,
				binary.BigEndian.Uint32(packet[24:28]))
			require.Equal(t, ^uint32(0),
				binary.BigEndian.Uint32(packet[32:36]))
			if len(test.wantPayload) == 0 {
				require.Empty(t, packet[retSubmitHeaderSize:])
			} else {
				require.Equal(t, test.wantPayload,
					packet[retSubmitHeaderSize:])
			}

			writes := recorder.waitForWrites(t, 1)
			require.Len(t, writes, 1)
			require.Equal(t, packet, writes[0].packet)
		})
	}
}

func TestLateResponseWriterClearsReusedScratchTail(t *testing.T) {
	recorder := newRecordingWriter()
	writer := newResponseWriter(recorder, nil)
	scratch := bytes.Repeat([]byte{0xcc}, retSubmitHeaderSize+8)

	packet, written, err := writer.writeLateRetSubmit(
		scratch, 720, 8, time.Time{},
		func(payload []byte) (lateResponseSelection, error) {
			copy(payload, []byte{1, 2, 3, 4, 5, 6, 7, 8})
			return lateResponseSelection{
				Result: lateResponseData, ActualLength: 8,
			}, nil
		},
		func(bool) error { return nil },
	)
	require.NoError(t, err)
	require.True(t, written)
	require.Equal(t, []byte{1, 2, 3, 4, 5, 6, 7, 8},
		packet[retSubmitHeaderSize:])

	packet, written, err = writer.writeLateRetSubmit(
		packet, 721, 8, time.Time{},
		func(payload []byte) (lateResponseSelection, error) {
			require.Equal(t, make([]byte, 8), payload,
				"reuse did not clear the previous payload")
			payload[0], payload[1] = 0x91, 0x92
			return lateResponseSelection{
				Result: lateResponseData, ActualLength: 2,
			}, nil
		},
		func(bool) error { return nil },
	)
	require.NoError(t, err)
	require.True(t, written)
	require.Equal(t, []byte{0x91, 0x92}, packet[retSubmitHeaderSize:])
	require.Equal(t, make([]byte, 6),
		packet[:cap(packet)][retSubmitHeaderSize+2:],
		"unused payload capacity retained stale bytes")

	writes := recorder.waitForWrites(t, 2)
	require.Len(t, writes[1].packet, retSubmitHeaderSize+2)
	require.Equal(t, []byte{0x91, 0x92},
		writes[1].packet[retSubmitHeaderSize:])
}

func TestLateResponseWriterClampsOversizedScratchCapacity(t *testing.T) {
	recorder := newRecordingWriter()
	writer := newResponseWriter(recorder, nil)
	scratch := bytes.Repeat([]byte{0xcc}, retSubmitHeaderSize+32)

	packet, written, err := writer.writeLateRetSubmit(
		scratch, 722, 4, time.Time{},
		func(payload []byte) (lateResponseSelection, error) {
			payload[0], payload[1] = 0x81, 0x82
			return lateResponseSelection{
				Result: lateResponseData, ActualLength: 2,
			}, nil
		},
		func(bool) error { return nil },
	)
	require.NoError(t, err)
	require.True(t, written)
	require.Equal(t, retSubmitHeaderSize+4, cap(packet),
		"returned packet exposed capacity beyond the validated maximum")
	require.Equal(t, []byte{0x81, 0x82}, packet[retSubmitHeaderSize:])
	require.Equal(t, make([]byte, 2),
		packet[:cap(packet)][retSubmitHeaderSize+2:],
		"unused validated payload capacity retained stale bytes")

	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, packet, writes[0].packet)
}

func TestLateResponseWriterPendingWritesFlushesAndCompletesNothing(t *testing.T) {
	recorder := newRecordingWriter()
	batcher := newBatchingWriter(recorder, 1024, 0, 0)
	t.Cleanup(func() { _ = batcher.Close() })
	_, err := batcher.Write([]byte{0xd1})
	require.NoError(t, err)
	require.Equal(t, 1, batcher.w.Buffered())

	writer := newResponseWriter(batcher, batcher)
	completionCalls := 0
	scratch, written, err := writer.writeLateRetSubmit(
		bytes.Repeat([]byte{0xee}, retSubmitHeaderSize+4),
		730, 4, time.Now(),
		func(payload []byte) (lateResponseSelection, error) {
			copy(payload, []byte{1, 2, 3, 4})
			return lateResponseSelection{Result: lateResponsePending}, nil
		},
		func(bool) error {
			completionCalls++
			return nil
		},
	)

	require.NoError(t, err)
	require.False(t, written)
	require.Empty(t, scratch)
	require.Zero(t, completionCalls)
	require.Equal(t, 1, batcher.w.Buffered(),
		"Pending unexpectedly flushed already-buffered bytes")
	recorder.mu.Lock()
	require.Empty(t, recorder.writes)
	recorder.mu.Unlock()
	require.Equal(t, make([]byte, 4),
		scratch[:cap(scratch)][retSubmitHeaderSize:],
		"Pending retained prepared payload bytes")
}

func TestLateResponseWriterRejectsInvalidPreparationWithoutCompletion(t *testing.T) {
	preparationFailure := errors.New("preparation failed")
	tests := []struct {
		name       string
		selection  lateResponseSelection
		prepareErr error
	}{
		{
			name: "data exceeds bound", selection: lateResponseSelection{
				Result: lateResponseData, ActualLength: 5,
			},
		},
		{
			name: "pending has actual length", selection: lateResponseSelection{
				Result: lateResponsePending, ActualLength: 1,
			},
		},
		{
			name: "stall has actual length", selection: lateResponseSelection{
				Result: lateResponseStall, ActualLength: 1,
			},
		},
		{
			name: "unknown result", selection: lateResponseSelection{
				Result: lateResponseResult(0xff),
			},
		},
		{name: "preparation error", prepareErr: preparationFailure},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := newRecordingWriter()
			writer := newResponseWriter(recorder, nil)
			completionCalls := 0
			scratch, written, err := writer.writeLateRetSubmit(
				nil, 740, 4, time.Time{},
				func(payload []byte) (lateResponseSelection, error) {
					copy(payload, []byte{9, 8, 7, 6})
					return test.selection, test.prepareErr
				},
				func(bool) error {
					completionCalls++
					return nil
				},
			)
			require.Error(t, err)
			if test.prepareErr != nil {
				require.ErrorIs(t, err, preparationFailure)
			}
			require.False(t, written)
			require.Empty(t, scratch)
			require.Zero(t, completionCalls)
			recorder.mu.Lock()
			require.Empty(t, recorder.writes)
			recorder.mu.Unlock()
			require.Equal(t, make([]byte, 4),
				scratch[:cap(scratch)][retSubmitHeaderSize:])
		})
	}

	writer := newResponseWriter(io.Discard, nil)
	prepareCalls := 0
	_, written, err := writer.writeLateRetSubmit(
		nil, 741, -1, time.Time{},
		func([]byte) (lateResponseSelection, error) {
			prepareCalls++
			return lateResponseSelection{Result: lateResponseSuccess}, nil
		},
		func(bool) error { return nil },
	)
	require.Error(t, err)
	require.False(t, written)
	require.Zero(t, prepareCalls)
}

func TestLateResponseWriterRejectsInvalidEnvelopeBeforeSerializerOwnership(t *testing.T) {
	type callResult struct {
		scratch []byte
		written bool
		err     error
	}
	tests := []struct {
		name       string
		maxPayload int
		prepare    lateResponsePrepareFunc
		complete   lateResponseCompleteFunc
	}{
		{
			name: "nil prepare", maxPayload: 0,
			complete: lateResponseAllocationComplete,
		},
		{
			name: "nil complete", maxPayload: 0,
			prepare: lateResponseAllocationPrepare,
		},
		{
			name:       "maximum exceeds server transfer limit",
			maxPayload: maximumTransferSize + 1,
			prepare:    lateResponseAllocationPrepare,
			complete:   lateResponseAllocationComplete,
		},
	}
	if strconv.IntSize > 32 {
		tests = append(tests, struct {
			name       string
			maxPayload int
			prepare    lateResponsePrepareFunc
			complete   lateResponseCompleteFunc
		}{
			name:       "maximum exceeds USB/IP actual-length field",
			maxPayload: int(uint64(^uint32(0)) + 1),
			prepare:    lateResponseAllocationPrepare,
			complete:   lateResponseAllocationComplete,
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := newRecordingWriter()
			writer := newResponseWriter(recorder, nil)
			original := make([]byte, 1, 1)
			writer.mu.Lock()
			done := make(chan callResult, 1)
			go func() {
				scratch, written, err := writer.writeLateRetSubmit(
					original, 745, test.maxPayload, time.Time{},
					test.prepare, test.complete)
				done <- callResult{scratch: scratch, written: written, err: err}
			}()

			var result callResult
			select {
			case result = <-done:
				writer.mu.Unlock()
			case <-time.After(time.Second):
				writer.mu.Unlock()
				t.Fatal("invalid envelope waited for serializer ownership")
			}
			require.Error(t, result.err)
			require.False(t, result.written)
			require.Empty(t, result.scratch)
			require.Equal(t, cap(original), cap(result.scratch),
				"invalid envelope allocated response scratch")
			recorder.mu.Lock()
			require.Empty(t, recorder.writes)
			recorder.mu.Unlock()
		})
	}
}

type failingShortWriter struct {
	data []byte
}

func (w *failingShortWriter) Write(packet []byte) (int, error) {
	n := max(0, len(packet)-1)
	w.data = append(w.data, packet[:n]...)
	return n, io.ErrShortWrite
}

type lateFailWriter struct {
	err error
}

func (w lateFailWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestLateResponseWriterReportsShortWriteAndFlushFailure(t *testing.T) {
	t.Run("short write", func(t *testing.T) {
		destination := &failingShortWriter{}
		writer := newResponseWriter(destination, nil)
		completionCalls := 0
		completionDelivered := true
		_, written, err := writer.writeLateRetSubmit(
			nil, 750, 0, time.Time{},
			func([]byte) (lateResponseSelection, error) {
				return lateResponseSelection{Result: lateResponseSuccess}, nil
			},
			func(delivered bool) error {
				completionCalls++
				completionDelivered = delivered
				return nil
			},
		)
		require.ErrorIs(t, err, io.ErrShortWrite)
		require.True(t, written)
		require.Equal(t, 1, completionCalls)
		require.False(t, completionDelivered)
		require.Len(t, destination.data, retSubmitHeaderSize-1)
	})

	t.Run("mandatory flush", func(t *testing.T) {
		flushFailure := errors.New("late flush failed")
		batcher := newBatchingWriter(
			lateFailWriter{err: flushFailure}, 1024, 0, 0)
		t.Cleanup(func() { _ = batcher.Close() })
		writer := newResponseWriter(batcher, batcher)
		completionCalls := 0
		completionDelivered := true
		_, written, err := writer.writeLateRetSubmit(
			nil, 751, 1, time.Time{},
			func(payload []byte) (lateResponseSelection, error) {
				payload[0] = 0x5a
				return lateResponseSelection{
					Result: lateResponseData, ActualLength: 1,
				}, nil
			},
			func(delivered bool) error {
				completionCalls++
				completionDelivered = delivered
				return nil
			},
		)
		require.ErrorIs(t, err, flushFailure)
		require.True(t, written)
		require.Equal(t, 1, completionCalls)
		require.False(t, completionDelivered)
	})
}

func TestLateResponseWriterContainsCallbackPanicsAndReleasesSerializer(t *testing.T) {
	panicFailure := errors.New("late callback panic")

	t.Run("preparation", func(t *testing.T) {
		recorder := newRecordingWriter()
		writer := newResponseWriter(recorder, nil)
		completionCalls := 0
		_, written, err := writer.writeLateRetSubmit(
			nil, 760, 1, time.Time{},
			func([]byte) (lateResponseSelection, error) {
				panic(panicFailure)
			},
			func(bool) error {
				completionCalls++
				return nil
			},
		)
		require.ErrorIs(t, err, panicFailure)
		require.Contains(t, err.Error(), "preparation callback panic")
		require.False(t, written)
		require.Zero(t, completionCalls)

		legacy := buildRetUnlinkPacket(nil, 761, 0)
		require.NoError(t, writer.write(legacy, true, time.Time{}),
			"preparation panic stranded responseWriter.mu")
		writes := recorder.waitForWrites(t, 1)
		require.Equal(t, legacy, writes[0].packet)
	})

	t.Run("completion", func(t *testing.T) {
		recorder := newRecordingWriter()
		writer := newResponseWriter(recorder, nil)
		_, written, err := writer.writeLateRetSubmit(
			nil, 762, 0, time.Time{},
			func([]byte) (lateResponseSelection, error) {
				return lateResponseSelection{Result: lateResponseSuccess}, nil
			},
			func(bool) error {
				panic(panicFailure)
			},
		)
		require.ErrorIs(t, err, panicFailure)
		require.Contains(t, err.Error(), "completion callback panic")
		require.True(t, written)

		legacy := buildRetUnlinkPacket(nil, 763, 0)
		require.NoError(t, writer.write(legacy, true, time.Time{}),
			"completion panic stranded responseWriter.mu")
		writes := recorder.waitForWrites(t, 2)
		require.Equal(t, uint32(762),
			binary.BigEndian.Uint32(writes[0].packet[4:8]))
		require.Equal(t, legacy, writes[1].packet)
	})
}

type lateResponseGateWriter struct {
	mu      sync.Mutex
	packets [][]byte
	calls   atomic.Uint32
	entered chan struct{}
	release chan struct{}
}

func newLateResponseGateWriter() *lateResponseGateWriter {
	return &lateResponseGateWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *lateResponseGateWriter) Write(packet []byte) (int, error) {
	call := w.calls.Add(1)
	w.mu.Lock()
	w.packets = append(w.packets, append([]byte(nil), packet...))
	w.mu.Unlock()
	if call == 1 {
		close(w.entered)
		<-w.release
	}
	return len(packet), nil
}

func requireStillBlocked(t *testing.T, done <-chan error, message string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s: completed early with %v", message, err)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLateResponseWriterSerializesPreparationFlushAndCompletion(t *testing.T) {
	destination := newLateResponseGateWriter()
	batcher := newBatchingWriter(destination, 1024, 0, 0)
	t.Cleanup(func() { _ = batcher.Close() })
	writer := newResponseWriter(batcher, batcher)

	firstPreparationEntered := make(chan struct{})
	firstPreparationRelease := make(chan struct{})
	firstCompletionEntered := make(chan struct{})
	firstCompletionRelease := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := writer.writeLateRetSubmit(
			nil, 770, 1, time.Time{},
			func(payload []byte) (lateResponseSelection, error) {
				close(firstPreparationEntered)
				<-firstPreparationRelease
				payload[0] = 0x70
				return lateResponseSelection{
					Result: lateResponseData, ActualLength: 1,
				}, nil
			},
			func(bool) error {
				close(firstCompletionEntered)
				<-firstCompletionRelease
				return nil
			},
		)
		firstDone <- err
	}()

	select {
	case <-firstPreparationEntered:
	case <-time.After(time.Second):
		t.Fatal("first late response did not enter preparation")
	}

	var secondPreparations atomic.Uint32
	secondDone := make(chan error, 1)
	go func() {
		_, _, err := writer.writeLateRetSubmit(
			nil, 771, 0, time.Time{},
			func([]byte) (lateResponseSelection, error) {
				secondPreparations.Add(1)
				return lateResponseSelection{Result: lateResponseSuccess}, nil
			},
			func(bool) error { return nil },
		)
		secondDone <- err
	}()
	legacyDone := make(chan error, 1)
	legacy := buildRetUnlinkPacket(nil, 772, 0)
	go func() {
		legacyDone <- writer.write(legacy, true, time.Time{})
	}()

	requireStillBlocked(t, secondDone,
		"competing late preparation bypassed first preparation")
	requireStillBlocked(t, legacyDone,
		"legacy response bypassed first preparation")
	require.Zero(t, secondPreparations.Load())
	require.Zero(t, destination.calls.Load())

	close(firstPreparationRelease)
	select {
	case <-destination.entered:
	case <-time.After(time.Second):
		t.Fatal("first late response did not enter mandatory flush")
	}
	requireStillBlocked(t, secondDone,
		"competing late preparation bypassed mandatory flush")
	requireStillBlocked(t, legacyDone,
		"legacy response bypassed mandatory flush")
	require.Zero(t, secondPreparations.Load())
	require.Equal(t, uint32(1), destination.calls.Load())

	close(destination.release)
	select {
	case <-firstCompletionEntered:
	case <-time.After(time.Second):
		t.Fatal("first late response did not enter completion callback")
	}
	requireStillBlocked(t, secondDone,
		"competing late preparation bypassed terminal completion")
	requireStillBlocked(t, legacyDone,
		"legacy response bypassed terminal completion")
	require.Zero(t, secondPreparations.Load())
	require.Equal(t, uint32(1), destination.calls.Load())

	close(firstCompletionRelease)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	require.NoError(t, <-legacyDone)
	require.Equal(t, uint32(1), secondPreparations.Load())
	require.Equal(t, uint32(3), destination.calls.Load())

	destination.mu.Lock()
	require.Len(t, destination.packets, 3)
	require.Equal(t, uint32(usbip.RetSubmitCode),
		binary.BigEndian.Uint32(destination.packets[0][0:4]))
	require.Equal(t, uint32(770),
		binary.BigEndian.Uint32(destination.packets[0][4:8]))
	destination.mu.Unlock()
}

func lateResponseAllocationPrepare(
	payload []byte,
) (lateResponseSelection, error) {
	payload[0] = 0x5a
	return lateResponseSelection{
		Result: lateResponseData, ActualLength: 1,
	}, nil
}

func lateResponseAllocationComplete(bool) error { return nil }

func TestLateResponseWriterSteadyStateAllocatesZero(t *testing.T) {
	writer := newResponseWriter(io.Discard, nil)
	scratch := make([]byte, retSubmitHeaderSize+8)
	var seq uint32
	allocations := testing.AllocsPerRun(1000, func() {
		seq++
		var written bool
		var err error
		scratch, written, err = writer.writeLateRetSubmit(
			scratch, seq, 8, time.Time{},
			lateResponseAllocationPrepare,
			lateResponseAllocationComplete,
		)
		if err != nil || !written {
			panic("late response allocation run failed")
		}
	})
	require.Zero(t, allocations)
}

func BenchmarkBuildRetSubmitInto(b *testing.B) {
	payload := make([]byte, 64)
	buffer := make([]byte, retSubmitHeaderSize+len(payload))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buffer = buildRetSubmitPacket(
			buffer, uint32(i), 0, uint32(len(payload)), payload, nil, false,
		)
	}
}

func BenchmarkLateResponseWriterInto(b *testing.B) {
	writer := newResponseWriter(io.Discard, nil)
	scratch := make([]byte, retSubmitHeaderSize+64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var written bool
		var err error
		scratch, written, err = writer.writeLateRetSubmit(
			scratch, uint32(i), 64, time.Time{},
			lateResponseAllocationPrepare,
			lateResponseAllocationComplete,
		)
		if err != nil || !written {
			b.Fatal("late response write failed")
		}
	}
}
