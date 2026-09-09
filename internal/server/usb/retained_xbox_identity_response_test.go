package usb

import (
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/stretchr/testify/require"
)

const retainedXboxMaximumStringSize = 254

type retainedXboxIdentityLocalExecutor struct{}

func (*retainedXboxIdentityLocalExecutor) Execute(
	xboxone.ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (*retainedXboxIdentityLocalExecutor) ResetAndDrain(time.Time) error {
	return nil
}

func (*retainedXboxIdentityLocalExecutor) ResetNeutral(
	xboxone.ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (*retainedXboxIdentityLocalExecutor) CancelAndDrain(time.Time) error {
	return nil
}

func (*retainedXboxIdentityLocalExecutor) DisconnectNeutral(
	xboxone.ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func newRetainedXboxMaximumStringAdapter(
	t *testing.T,
) (*xboxone.DormantRetainedUSBAdapter, []byte) {
	t.Helper()
	identity := xboxone.ControllerIdentity{
		VendorID: 0xf00d, ProductID: 0xbeef, DeviceReleaseBCD: 0x0102,
		DeviceID: 0x0000fffb01020304,
		Firmware: xboxone.FirmwareVersion{
			Major: 1, Minor: 2, Build: 3, Revision: 4,
		},
		HardwareMajor: 5, HardwareMinor: 6,
	}
	profile, err := xboxone.NewUnregisteredControllerProfile(
		identity,
		xboxone.ControllerUSBConfig{
			MaxPower2mA: 0x32, OUTIntervalMS: 4, INIntervalMS: 8,
		})
	require.NoError(t, err)
	metadata, err := profile.BindExternallyCompiledMetadata([]byte{0xa5})
	require.NoError(t, err)
	product := strings.Repeat("\u0800", 126)
	authorization, err := xboxone.NewAuthorizedControllerPersonaConfig(
		xboxone.ControllerPersonaConfig{
			Profile: profile, Metadata: metadata,
			CurrentInput:      xboxone.GamepadInputReportV1{},
			CurrentStatus:     xboxone.NewWiredNoBatteryStatus(false),
			PoweringOffStatus: xboxone.NewWiredNoBatteryStatus(true),
		},
		xboxone.ControllerUSBIdentityStrings{
			Manufacturer: "Synthetic Test Lab",
			Product:      product,
			Serial:       "0000fffb01020304A1B2C3D4E5F60708",
		},
		xboxone.ControllerIdentityAuthorizationGranted)
	require.NoError(t, err)
	engine, err := xboxone.NewAuthorizedControllerPersonaEngine(
		authorization, 0)
	require.NoError(t, err)

	const authorityID uint64 = 0x58424f5841555448
	const deviceID uint64 = 0x58424f5844455649
	adapter, err := xboxone.NewDormantRetainedUSBAdapter(
		engine, authorityID, deviceID,
		&retainedXboxIdentityLocalExecutor{}, 100*time.Millisecond)
	require.NoError(t, err)
	lease := retainedusb.ImportLease{
		AuthorityID:       authorityID,
		DeviceID:          deviceID,
		OwnerID:           adapter.Identity(),
		ImportToken:       1,
		SessionGeneration: 17,
	}
	bound, err := adapter.BindImport(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, retainedusb.ImportBindBound, bound.State)
	require.Equal(t, lease, bound.Lease)
	t.Cleanup(func() {
		result, cleanupErr := adapter.CancelAndDrain(
			lease, retainedusb.ImportCloseExplicitDetach,
			time.Now().Add(time.Second))
		if cleanupErr != nil || result.State != retainedusb.ImportDrainDrained {
			t.Errorf("retained Xbox adapter cleanup = (%+v, %v)",
				result, cleanupErr)
		}
	})

	wire := make([]byte, retainedXboxMaximumStringSize)
	wire[0] = retainedXboxMaximumStringSize
	wire[1] = 0x03
	for offset := 2; offset < len(wire); offset += 2 {
		binary.LittleEndian.PutUint16(wire[offset:offset+2], 0x0800)
	}
	return adapter, wire
}

func retainedXboxProductStringEnvelope(
	sequence uint32,
	transferLength uint16,
	setupLength uint16,
) retainedSubmissionEnvelope {
	var setup [8]byte
	setup[0] = 0x80 // device-to-host, standard, device
	setup[1] = 0x06 // GET_DESCRIPTOR
	binary.LittleEndian.PutUint16(setup[2:4], 0x0302)
	binary.LittleEndian.PutUint16(
		setup[4:6], xboxone.USBEnglishUnitedStatesLanguageID)
	binary.LittleEndian.PutUint16(setup[6:8], setupLength)
	return retainedSubmissionEnvelope{
		lane: retainedusb.LaneControl, direction: retainedusb.DirectionIn,
		sequence: sequence, transferLength: uint32(transferLength),
		setup: setup, bindingGeneration: 1,
	}
}

func newRetainedXboxIdentityScheduler(
	t *testing.T,
	adapter *xboxone.DormantRetainedUSBAdapter,
	destination io.Writer,
) *retainedSubmissionScheduler {
	t.Helper()
	scheduler, err := newRetainedSubmissionScheduler(
		context.Background(), 17, adapter,
		newResponseWriter(destination, nil), false)
	require.NoError(t, err)
	t.Cleanup(func() {
		if cleanupErr := scheduler.close(); cleanupErr != nil {
			t.Errorf("retained Xbox scheduler cleanup: %v", cleanupErr)
		}
	})
	return scheduler
}

func assertRetainedXboxResponseAndRequestGeometry(
	t *testing.T,
	adapter *xboxone.DormantRetainedUSBAdapter,
	scheduler *retainedSubmissionScheduler,
) {
	t.Helper()
	limits := adapter.Limits()
	require.Equal(t, [3]uint8{
		1, retainedusb.MaximumQueueDepth, 1,
	}, limits.QueueDepth)
	require.Equal(t, uint32(64), limits.BufferedRequestBytes)
	require.Zero(t, limits.MaximumControlOut)
	require.Equal(t, uint32(retainedXboxMaximumStringSize),
		limits.MaximumControlResponse)
	require.Equal(t, uint32(64), limits.MaximumInterruptIn)
	require.Equal(t, uint32(64), limits.MaximumInterruptOut)

	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	require.Equal(t, retainedXboxMaximumStringSize,
		scheduler.maximumResponseLength)
	require.Len(t, scheduler.responseScratch,
		retSubmitHeaderSize+retainedXboxMaximumStringSize)
	require.Equal(t, retSubmitHeaderSize+retainedXboxMaximumStringSize,
		cap(scheduler.responseScratch))
	controlIndex, _ := retainedusb.LaneControl.Index()
	inIndex, _ := retainedusb.LaneInterruptIn.Index()
	outIndex, _ := retainedusb.LaneInterruptOut.Index()
	for _, index := range []int{controlIndex, outIndex} {
		require.Len(t, scheduler.lanes[index].slots, 1)
		require.Len(t, scheduler.lanes[index].order, 1)
		require.Len(t, scheduler.lanes[index].free, 1)
	}
	require.Len(t, scheduler.lanes[inIndex].slots,
		retainedusb.MaximumQueueDepth)
	require.Len(t, scheduler.lanes[inIndex].order,
		retainedusb.MaximumQueueDepth)
	require.Len(t, scheduler.lanes[inIndex].free,
		retainedusb.MaximumQueueDepth)
	require.Zero(t, len(scheduler.lanes[controlIndex].payloadSlab))
	require.Zero(t, cap(scheduler.lanes[controlIndex].payloadSlab))
	require.Zero(t, len(scheduler.lanes[inIndex].payloadSlab))
	require.Zero(t, cap(scheduler.lanes[inIndex].payloadSlab))
	require.Len(t, scheduler.lanes[outIndex].payloadSlab, 64)
	require.Equal(t, 64, cap(scheduler.lanes[outIndex].payloadSlab))
	require.Zero(t, cap(scheduler.lanes[controlIndex].slots[0].payload))
	require.Zero(t, cap(scheduler.lanes[inIndex].slots[0].payload))
	require.Equal(t, 64,
		cap(scheduler.lanes[outIndex].slots[0].payload))
}

func TestDormantRetainedXboxQueuesWindowsInterruptInPipelineWithoutENOSPC(
	t *testing.T,
) {
	adapter, _ := newRetainedXboxMaximumStringAdapter(t)
	scheduler := newRetainedXboxIdentityScheduler(t, adapter, io.Discard)

	for offset := uint32(0); offset < retainedusb.MaximumQueueDepth; offset++ {
		envelope := retainedInterruptInEnvelope(9000 + offset)
		require.NoError(t, scheduler.enqueue(envelope),
			"interrupt-IN pipeline entry %d", offset)
	}

	// The bound is intentional: a hostile or broken host cannot grow the
	// request ledger without limit. Windows' ordinary pipeline fits completely
	// and therefore never receives the ENOSPC response that resets the GIP pipe.
	require.ErrorIs(t,
		scheduler.enqueue(retainedInterruptInEnvelope(
			9000+retainedusb.MaximumQueueDepth)),
		errRetainedSubmissionQueueFull)

	inIndex, _ := retainedusb.LaneInterruptIn.Index()
	scheduler.mu.Lock()
	require.Equal(t, retainedusb.MaximumQueueDepth,
		scheduler.lanes[inIndex].orderCount)
	require.Equal(t, 0, scheduler.lanes[inIndex].freeCount)
	scheduler.mu.Unlock()
}

func TestDormantRetainedXboxAuthorizedMaximumStringEndToEnd(t *testing.T) {
	adapter, want := newRetainedXboxMaximumStringAdapter(t)
	recorder := newRecordingWriter()
	scheduler := newRetainedXboxIdentityScheduler(t, adapter, recorder)
	assertRetainedXboxResponseAndRequestGeometry(t, adapter, scheduler)

	require.NoError(t, scheduler.enqueue(retainedXboxProductStringEnvelope(
		1001, retainedXboxMaximumStringSize, retainedXboxMaximumStringSize)))
	progressed, _, err := scheduler.serviceRound(time.Now())
	require.NoError(t, err)
	require.True(t, progressed)
	writes := recorder.waitForWrites(t, 1)
	require.Len(t, writes[0].packet,
		retSubmitHeaderSize+retainedXboxMaximumStringSize)
	require.Zero(t, int32(binary.BigEndian.Uint32(writes[0].packet[20:24])))
	require.Equal(t, uint32(retainedXboxMaximumStringSize),
		binary.BigEndian.Uint32(writes[0].packet[24:28]))
	require.Equal(t, want, writes[0].packet[retSubmitHeaderSize:])

	const truncatedLength = 17
	require.NoError(t, scheduler.enqueue(retainedXboxProductStringEnvelope(
		1002, truncatedLength, truncatedLength)))
	progressed, _, err = scheduler.serviceRound(time.Now())
	require.NoError(t, err)
	require.True(t, progressed)
	writes = recorder.waitForWrites(t, 2)
	require.Len(t, writes[1].packet, retSubmitHeaderSize+truncatedLength)
	require.Equal(t, uint32(truncatedLength),
		binary.BigEndian.Uint32(writes[1].packet[24:28]))
	require.Equal(t, want[:truncatedLength],
		writes[1].packet[retSubmitHeaderSize:])

	// An inconsistent USB/IP transfer shorter than setup wLength is not host
	// truncation. The adapter rejects it before a response claim can escape.
	require.NoError(t, scheduler.enqueue(retainedXboxProductStringEnvelope(
		1003, retainedXboxMaximumStringSize-1,
		retainedXboxMaximumStringSize)))
	progressed, _, err = scheduler.serviceRound(time.Now())
	require.Error(t, err)
	require.False(t, progressed)

	// Host wLength is a requested maximum, not the descriptor's actual size.
	// Self-consistent requests above the owner's 254-byte response capacity are
	// lawful and return the complete 254-byte descriptor.
	for index, requestedLength := range []uint16{255, ^uint16(0)} {
		require.NoError(t, scheduler.enqueue(retainedXboxProductStringEnvelope(
			1004+uint32(index), requestedLength, requestedLength)))
		progressed, _, err = scheduler.serviceRound(time.Now())
		require.NoError(t, err)
		require.True(t, progressed)
		writes = recorder.waitForWrites(t, 3+index)
		require.Len(t, writes[2+index].packet,
			retSubmitHeaderSize+retainedXboxMaximumStringSize)
		require.Equal(t, uint32(retainedXboxMaximumStringSize),
			binary.BigEndian.Uint32(writes[2+index].packet[24:28]))
		require.Equal(t, want,
			writes[2+index].packet[retSubmitHeaderSize:])
	}
	recorder.mu.Lock()
	writeCount := len(recorder.writes)
	recorder.mu.Unlock()
	require.Equal(t, 4, writeCount)
	scheduler.mu.Lock()
	empty := scheduler.emptyLocked()
	scheduler.mu.Unlock()
	require.True(t, empty)
}

func TestDormantRetainedXboxMaximumStringWarmPathAllocatesZero(t *testing.T) {
	adapter, _ := newRetainedXboxMaximumStringAdapter(t)
	scheduler := newRetainedXboxIdentityScheduler(t, adapter, io.Discard)
	assertRetainedXboxResponseAndRequestGeometry(t, adapter, scheduler)
	var sequence uint32 = 2000
	run := func() {
		sequence++
		if err := scheduler.enqueue(retainedXboxProductStringEnvelope(
			sequence, ^uint16(0), ^uint16(0))); err != nil {
			panic(err)
		}
		progressed, _, err := scheduler.serviceRound(time.Now())
		if err != nil || !progressed {
			panic("retained Xbox maximum string did not complete")
		}
	}
	run()
	allocations := testing.AllocsPerRun(1000, run)
	require.Zero(t, allocations)
}
