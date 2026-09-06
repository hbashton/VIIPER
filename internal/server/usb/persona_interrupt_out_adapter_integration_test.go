package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	usbdesc "github.com/Alia5/VIIPER/usb"
)

type personaInterruptOutFailWriter struct{ err error }

func (writer personaInterruptOutFailWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

type personaInterruptOutIntegrationParticipant struct {
	mu             sync.Mutex
	preflightCalls int
	executeCalls   int
}

func (participant *personaInterruptOutIntegrationParticipant) PreflightControllerDownstreamPacket(
	xboxone.ControllerDownstreamPacketExecution,
) error {
	participant.mu.Lock()
	participant.preflightCalls++
	participant.mu.Unlock()
	return nil
}

func (participant *personaInterruptOutIntegrationParticipant) ExecuteControllerDownstreamPacket(
	xboxone.ControllerDownstreamPacketExecution,
	time.Time,
) error {
	participant.mu.Lock()
	participant.executeCalls++
	participant.mu.Unlock()
	return nil
}

func (*personaInterruptOutIntegrationParticipant) CancelControllerDownstreamPacketAndDrain(
	xboxone.ControllerDownstreamPacketExecution,
	time.Time,
) error {
	return nil
}

func (participant *personaInterruptOutIntegrationParticipant) counts() (int, int) {
	participant.mu.Lock()
	defer participant.mu.Unlock()
	return participant.preflightCalls, participant.executeCalls
}

type personaInterruptOutIntegrationDevice struct {
	adapter *xboxone.DormantControllerPersonaInterruptOutAdapter
	desc    usbdesc.Descriptor

	mu          sync.Mutex
	legacyCalls int
}

func (device *personaInterruptOutIntegrationDevice) HandleTransfer(
	context.Context,
	uint32,
	uint32,
	[]byte,
) []byte {
	device.mu.Lock()
	device.legacyCalls++
	device.mu.Unlock()
	return nil
}

func (device *personaInterruptOutIntegrationDevice) GetDescriptor() *usbdesc.Descriptor {
	return &device.desc
}

func (*personaInterruptOutIntegrationDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (device *personaInterruptOutIntegrationDevice) ClaimInterruptOutTransaction(
	request usbdesc.InterruptOutTransactionRequest,
) (usbdesc.InterruptOutTransactionClaim, error) {
	return device.adapter.ClaimInterruptOutTransaction(request)
}

func (device *personaInterruptOutIntegrationDevice) AdmitInterruptOutTransaction(
	claim usbdesc.InterruptOutTransactionClaim,
) error {
	return device.adapter.AdmitInterruptOutTransaction(claim)
}

func (device *personaInterruptOutIntegrationDevice) CompleteInterruptOutTransaction(
	claim usbdesc.InterruptOutTransactionClaim,
	outcome usbdesc.InterruptOutTransactionOutcome,
) error {
	return device.adapter.CompleteInterruptOutTransaction(claim, outcome)
}

func newPersonaInterruptOutIntegrationDevice(
	t *testing.T,
) (*personaInterruptOutIntegrationDevice,
	*personaInterruptOutIntegrationParticipant) {
	t.Helper()
	profile, err := xboxone.NewUnregisteredControllerProfile(
		xboxone.ControllerIdentity{
			VendorID: 0xf00d, ProductID: 0xbeef, DeviceReleaseBCD: 0x0102,
			DeviceID: 0x0000fffb01020304,
			Firmware: xboxone.FirmwareVersion{
				Major: 1, Minor: 2, Build: 3, Revision: 4,
			},
			HardwareMajor: 5, HardwareMinor: 6,
		},
		xboxone.ControllerUSBConfig{
			MaxPower2mA: 0x32, OUTIntervalMS: 4, INIntervalMS: 8,
		})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := profile.BindExternallyCompiledMetadata([]byte{0xaa})
	if err != nil {
		t.Fatal(err)
	}
	engineValue, err := xboxone.NewControllerPersonaEngine(
		xboxone.ControllerPersonaConfig{
			Profile: profile, Metadata: metadata,
			CurrentInput:      xboxone.GamepadInputReportV1{},
			CurrentStatus:     xboxone.NewWiredNoBatteryStatus(false),
			PoweringOffStatus: xboxone.NewWiredNoBatteryStatus(true),
		}, 0)
	if err != nil {
		t.Fatal(err)
	}
	engine := &engineValue
	for _, setup := range [][8]byte{
		{0x00, 0x05, 0x01, 0x00, 0, 0, 0, 0},
		{0x00, 0x09, 0x01, 0x00, 0, 0, 0, 0},
	} {
		claim, claimErr := engine.ClaimUSBControl(setup[:])
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		destination := make([]byte, claim.Size())
		if claimErr = engine.AdmitAndCopy(claim, destination, 0); claimErr != nil {
			t.Fatal(claimErr)
		}
		if claimErr = engine.Resolve(
			claim, xboxone.ControllerPersonaDelivered, 0); claimErr != nil {
			t.Fatal(claimErr)
		}
	}
	participant := &personaInterruptOutIntegrationParticipant{}
	owner, err := xboxone.NewControllerPersonaDownstreamPacketBatchOwner(
		engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := xboxone.NewDormantControllerPersonaInterruptOutAdapter(
		owner, time.Now().Add(-time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	return &personaInterruptOutIntegrationDevice{
		adapter: adapter,
		desc: usbdesc.Descriptor{
			Device: usbdesc.DeviceDescriptor{BNumConfigurations: 1},
		},
	}, participant
}

func TestPersonaInterruptOutAdapterServerCompletionOnlyPublishesWorkerTicket(
	t *testing.T,
) {
	device, participant := newPersonaInterruptOutIntegrationDevice(t)
	wire := make([]byte, xboxone.DirectMotorMessageSize)
	err := xboxone.EncodeDirectMotorMessageInto(wire, 0x31,
		xboxone.RumbleBodyV1{
			Enabled:       xboxone.MotorLeftVibration | xboxone.MotorRightImpulse,
			LeftVibration: 23, RightImpulse: 47, Duration: 8,
		})
	if err != nil {
		t.Fatal(err)
	}

	response, handled, err := processTransactionalInterruptOutSubmission(
		newResponseWriter(io.Discard, nil), device, nil, 91, 1, wire)
	if err != nil || !handled {
		t.Fatalf("process = (handled %t, err %v)", handled, err)
	}
	if got := int32(binary.BigEndian.Uint32(response[20:24])); got != 0 {
		t.Fatalf("RET_SUBMIT status = %d", got)
	}
	if got := binary.BigEndian.Uint32(response[24:28]); got != uint32(len(wire)) {
		t.Fatalf("RET_SUBMIT actual length = %d, want %d", got, len(wire))
	}
	if preflight, execute := participant.counts(); preflight != 2 || execute != 0 {
		t.Fatalf("response-writer calls = preflight %d execute %d",
			preflight, execute)
	}
	ticket, present := device.adapter.PendingWork()
	if !present {
		t.Fatal("server completion did not publish retained work")
	}
	now := time.Now()
	if err := device.adapter.RunPending(
		ticket, now.Add(time.Second), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, execute := participant.counts(); execute != 1 {
		t.Fatalf("worker execute calls = %d", execute)
	}
	device.mu.Lock()
	legacyCalls := device.legacyCalls
	device.mu.Unlock()
	if legacyCalls != 0 {
		t.Fatalf("legacy transfer calls = %d", legacyCalls)
	}
}

func TestPersonaInterruptOutAdapterServerDeliveryFailureQueuesNoEffectRetirement(
	t *testing.T,
) {
	device, participant := newPersonaInterruptOutIntegrationDevice(t)
	wire := make([]byte, xboxone.DirectMotorMessageSize)
	if err := xboxone.EncodeDirectMotorMessageInto(wire, 0x41,
		xboxone.RumbleBodyV1{
			Enabled:     xboxone.MotorLeftImpulse,
			LeftImpulse: 31, Duration: 4,
		}); err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("forced integration delivery failure")
	_, handled, err := processTransactionalInterruptOutSubmission(
		newResponseWriter(personaInterruptOutFailWriter{err: writeErr}, nil),
		device, nil, 92, 1, wire)
	if !handled || !errors.Is(err, writeErr) {
		t.Fatalf("process = (handled %t, err %v)", handled, err)
	}
	if _, execute := participant.counts(); execute != 0 {
		t.Fatalf("response failure executed %d effects", execute)
	}
	ticket, present := device.adapter.PendingWork()
	if !present {
		t.Fatal("delivery failure did not retain retirement work")
	}
	now := time.Now()
	err = device.adapter.RunPending(
		ticket, now.Add(time.Second), now.Add(2*time.Second))
	if !errors.Is(err, xboxone.ErrControllerPersonaInterruptOutRetryUnsupported) ||
		!errors.Is(err, xboxone.ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("RunPending = %v", err)
	}
	if _, execute := participant.counts(); execute != 0 {
		t.Fatalf("retirement worker executed %d effects", execute)
	}
}
