// Package xbox360 provides an Xbox 360 controller device implementation.
package xbox360

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type Xbox360 struct {
	input                     *inputpresentation.FixedReportScheduler[InputState]
	inputSignal               chan struct{}
	inputLifecycleMu          sync.Mutex
	producerMu                sync.Mutex
	producerActive            bool
	compatProducerUsed        bool
	retiredCompatibilityLease inputpresentation.FixedReportProducerLease
	resyncMu                  sync.Mutex
	pendingResync             InputState
	pendingResyncAt           time.Time
	hasPendingResync          bool
	maximumOrderedAge         time.Duration
	rumbleDispatchMu          sync.Mutex
	rumbleMu                  sync.Mutex
	rumbleFunc                func(XRumbleState)
	rumbleState               XRumbleState
	rumbleSeen                bool
	descriptor                usb.Descriptor
}

var _ inputpresentation.Source = (*Xbox360)(nil)
var _ inputpresentation.AdmissionSource = (*Xbox360)(nil)

type Xbox360CreateOptions struct {
	SubType                       *uint8 `json:"subType"`
	MaximumOrderedAgeMilliseconds *int64 `json:"maximumOrderedAgeMilliseconds"`
}

// New returns a new Xbox360 device.
func New(o *device.CreateOptions) (*Xbox360, error) {
	var args Xbox360CreateOptions
	if o != nil && o.DeviceSpecific != "" {
		if err := json.Unmarshal([]byte(o.DeviceSpecific), &args); err != nil {
			return nil, fmt.Errorf("invalid JSON payload: %w", err)
		}
	}

	var maximumOrderedAge time.Duration
	if args.MaximumOrderedAgeMilliseconds != nil {
		milliseconds := *args.MaximumOrderedAgeMilliseconds
		if milliseconds < 1 || milliseconds > 60_000 {
			return nil, errors.New("maximumOrderedAgeMilliseconds must be from 1 through 60000")
		}
		maximumOrderedAge = time.Duration(milliseconds) * time.Millisecond
	}

	neutral := *NewInputState()
	encode := func(state *InputState, destination []byte) int {
		return state.BuildReportInto(destination)
	}
	var input *inputpresentation.FixedReportScheduler[InputState]
	var err error
	if maximumOrderedAge > 0 {
		input, err = inputpresentation.NewFixedReportSchedulerWithMaximumOrderedAge(
			20, neutral, encode, xbox360InputTransition,
			maximumOrderedAge, time.Now())
	} else {
		input, err = inputpresentation.NewFixedReportSchedulerWithOverflowFault(
			20, neutral, encode, xbox360InputTransition, time.Now())
	}
	if err != nil {
		return nil, fmt.Errorf("create input scheduler: %w", err)
	}
	d := &Xbox360{
		descriptor:        MakeDescriptor(),
		input:             input,
		maximumOrderedAge: maximumOrderedAge,
	}
	if o != nil {
		if o.IDVendor != nil {
			d.descriptor.Device.IDVendor = *o.IDVendor
		}
		if o.IDProduct != nil {
			d.descriptor.Device.IDProduct = *o.IDProduct
		}
		if args.SubType != nil {
			d.descriptor.Interfaces[0].ClassDescriptors[0].Payload[2] = *args.SubType
		}
	}
	d.inputSignal = make(chan struct{}, 1)
	d.inputSignal <- struct{}{}
	return d, nil
}

// SetRumbleCallback sets a callback that will be invoked when rumble commands arrive.
func (x *Xbox360) SetRumbleCallback(f func(XRumbleState)) {
	x.rumbleDispatchMu.Lock()
	defer x.rumbleDispatchMu.Unlock()

	x.rumbleMu.Lock()
	x.rumbleFunc = f
	latest := x.rumbleState
	replay := f != nil && x.rumbleSeen
	x.rumbleMu.Unlock()

	if replay {
		f(latest)
	}
}

// UpdateInputState updates the device's current input state (thread-safe).
func (x *Xbox360) UpdateInputState(state InputState) bool {
	x.producerMu.Lock()
	defer x.producerMu.Unlock()
	if x.producerActive {
		// The library compatibility surface and raw stream are alternative
		// producers. Interleaving them would make producer retirement ambiguous.
		return false
	}
	x.compatProducerUsed = true
	_, disposition := x.publishInputStateWithLease(
		x.input.ProducerLease(), state, time.Now())
	return disposition.Accepted()
}

func (x *Xbox360) acquireInputProducer() (
	inputpresentation.FixedReportProducerLease, bool,
) {
	x.producerMu.Lock()
	if x.producerActive {
		x.producerMu.Unlock()
		return inputpresentation.FixedReportProducerLease{}, false
	}
	retiredCompatibilityProducer := false
	x.inputLifecycleMu.Lock()
	lease := x.input.ProducerLease()
	if x.compatProducerUsed {
		compatibilityLease := lease
		var retired bool
		lease, retired = x.input.RetireProducerLease(lease, time.Now())
		if !retired {
			x.inputLifecycleMu.Unlock()
			x.producerMu.Unlock()
			return inputpresentation.FixedReportProducerLease{}, false
		}
		x.clearInputResynchronization()
		x.retiredCompatibilityLease = compatibilityLease
		retiredCompatibilityProducer = true
	}
	x.producerActive = true
	x.compatProducerUsed = false
	x.inputLifecycleMu.Unlock()
	x.producerMu.Unlock()
	if retiredCompatibilityProducer {
		x.signalInput()
	}
	return lease, true
}

func (x *Xbox360) releaseInputProducer() {
	x.producerMu.Lock()
	if !x.producerActive {
		x.producerMu.Unlock()
		return
	}
	x.inputLifecycleMu.Lock()
	x.clearInputResynchronization()
	// Every Xbox360 scheduler mutation is serialized by inputLifecycleMu. The
	// lease read and retirement therefore identify one exact current epoch even
	// if USB reset or final admission raced with producer disconnect.
	currentLease := x.input.ProducerLease()
	_, retired := x.input.RetireProducerLease(currentLease, time.Now())
	if retired {
		x.retiredCompatibilityLease =
			inputpresentation.FixedReportProducerLease{}
	}
	x.inputLifecycleMu.Unlock()
	x.producerActive = false
	x.producerMu.Unlock()
	if retired {
		x.signalInput()
	}
}

func (x *Xbox360) publishInputStateWithLease(
	lease inputpresentation.FixedReportProducerLease, state InputState,
	receivedAt time.Time,
) (inputpresentation.FixedReportProducerLease,
	inputpresentation.FixedReportPublishDisposition) {
	x.inputLifecycleMu.Lock()
	defer x.inputLifecycleMu.Unlock()
	disposition := x.input.PublishWithLease(lease, state, receivedAt)
	if disposition.Accepted() {
		x.signalInput()
		return lease, disposition
	}

	currentLease := x.input.ProducerLease()
	if lease == x.retiredCompatibilityLease {
		return currentLease, disposition
	}
	switch disposition {
	case inputpresentation.FixedReportPublishFaultedOverflow,
		inputpresentation.FixedReportPublishRejectedNeutralPending:
		x.stageInputResynchronization(state, receivedAt)
		x.signalInput()
		return currentLease, disposition
	case inputpresentation.FixedReportPublishRejectedStaleProducer,
		inputpresentation.FixedReportPublishRejectedResynchronizationRequired:
		snapshot := x.input.Snapshot()
		if snapshot.MandatoryNeutral || snapshot.Resynchronization {
			x.stageInputResynchronization(state, receivedAt)
			x.signalInput()
			return currentLease, disposition
		}
		// A stale lease with no fault state is a USB presentation boundary.
		// Adopt its successor for the next stream frame, but never replay this
		// frame: it may have been captured before CLEAR_HALT/SET_INTERFACE or
		// SET_CONFIGURATION retired the old generation.
		return currentLease, disposition
	default:
		return currentLease, disposition
	}
}

func (x *Xbox360) signalInput() {
	select {
	case x.inputSignal <- struct{}{}:
	default:
	}
}

func (x *Xbox360) stageInputResynchronization(state InputState,
	receivedAt time.Time) {
	x.resyncMu.Lock()
	if !x.hasPendingResync || !receivedAt.Before(x.pendingResyncAt) {
		x.pendingResync = state
		x.pendingResyncAt = receivedAt
		x.hasPendingResync = true
	}
	x.resyncMu.Unlock()
}

func (x *Xbox360) clearInputResynchronization() {
	x.resyncMu.Lock()
	x.pendingResync = InputState{}
	x.pendingResyncAt = time.Time{}
	x.hasPendingResync = false
	x.resyncMu.Unlock()
}

// tryPublishInputResynchronizationLocked requires inputLifecycleMu. It is
// called only by the presentation owner after a mandatory neutral commits, so
// a producer callback cannot race a direct resynchronization or clear a newer
// staged snapshot.
func (x *Xbox360) tryPublishInputResynchronizationLocked() bool {
	x.resyncMu.Lock()
	defer x.resyncMu.Unlock()
	if !x.hasPendingResync {
		return false
	}
	disposition := x.input.Resynchronize(x.input.ProducerLease(),
		x.pendingResync, x.pendingResyncAt)
	if disposition !=
		inputpresentation.FixedReportPublishAcceptedResynchronization {
		return false
	}
	x.pendingResync = InputState{}
	x.pendingResyncAt = time.Time{}
	x.hasPendingResync = false
	x.signalInput()
	return true
}

// HandleTransfer implements interrupt IN/OUT for Xbox360.
func (x *Xbox360) HandleTransfer(ctx context.Context, ep uint32, dir uint32, out []byte) []byte {
	if dir == usbip.DirIn {
		switch ep {
		case 1: // 0x81 - main input reports
			select {
			case <-ctx.Done():
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return nil
				}
			case <-x.inputSignal:
			}

			return x.buildInputReport()
		default:
			return nil
		}
	}
	if dir == usbip.DirOut && ep == 1 {
		// Host->Device output reports used by the wired Xbox 360 controller include
		// an 8-byte rumble packet: [0]=ReportID(0x00), [1]=Len(0x08), [2]=Reserved/Status(0x00),
		// [3]=Left (low-frequency/large) motor 0-255, [4]=Right (high-frequency/small) motor 0-255,
		// [5..7]=Reserved (often 0x00).
		// Some other outbound reports (e.g. LED control) use different IDs/lengths; we ignore those here.
		if len(out) >= 8 && out[0] == 0x00 && out[1] == 0x08 {
			x.emitRumble(XRumbleState{
				LeftMotor:  out[3], // big / low-frequency motor
				RightMotor: out[4], // small / high-frequency motor
			})
		}
	}
	return nil
}

func xbox360InputTransition(previous, next InputState) bool {
	return previous.Buttons != next.Buttons ||
		(previous.LT == 0) != (next.LT == 0) ||
		(previous.RT == 0) != (next.RT == 0)
}

func (x *Xbox360) buildInputReport() []byte {
	report := make([]byte, 20)
	claim := x.ClaimInputPresentation(report, time.Now())
	if !claim.Valid() || !x.CanAdmitInputPresentation(claim, time.Now()) {
		if claim.Valid() {
			x.ResolveInputPresentation(claim,
				inputpresentation.OutcomeDefer, time.Now())
		}
		return nil
	}
	x.ResolveInputPresentation(claim, inputpresentation.OutcomeCommit, time.Now())
	return report
}

// BuildInputReportInto is the allocation-free compatibility surface used by
// tests and non-claiming transports. Production USB/IP uses Source directly.
func (x *Xbox360) BuildInputReportInto(destination []byte) int {
	claim := x.ClaimInputPresentation(destination, time.Now())
	if !claim.Valid() {
		return 0
	}
	if !x.CanAdmitInputPresentation(claim, time.Now()) {
		x.ResolveInputPresentation(claim, inputpresentation.OutcomeDefer,
			time.Now())
		return 0
	}
	x.ResolveInputPresentation(claim, inputpresentation.OutcomeCommit, time.Now())
	return claim.Size
}

func (x *Xbox360) ClaimInputPresentation(destination []byte,
	selectedAt time.Time) inputpresentation.Claim {
	x.inputLifecycleMu.Lock()
	claim := x.input.ClaimInputPresentation(destination, selectedAt)
	x.inputLifecycleMu.Unlock()
	return claim
}

// OwnsInputPresentationEndpoint confines the semantic input journal to the
// controller's main 0x81 endpoint. Auxiliary 0x82/0x83/0x84 endpoints have
// independent protocols and must not consume controller transitions.
func (x *Xbox360) OwnsInputPresentationEndpoint(endpoint uint8) bool {
	return endpoint == 1
}

func (x *Xbox360) InputPresentationGeneration() uint64 {
	return x.input.Generation()
}

func (x *Xbox360) ResolveInputPresentation(claim inputpresentation.Claim,
	outcome inputpresentation.Outcome, completedAt time.Time) bool {
	x.inputLifecycleMu.Lock()
	resolved := x.input.ResolveInputPresentation(claim, outcome, completedAt)
	if resolved && outcome == inputpresentation.OutcomeCommit {
		x.tryPublishInputResynchronizationLocked()
	}
	x.inputLifecycleMu.Unlock()
	return resolved
}

// CanAdmitInputPresentation revalidates the scheduler-owned claim at the
// transport's final write boundary. Compatibility mode still revokes stale
// tokens and retired generations; an explicitly configured maximum age also
// faults stale ordered history before USB/IP exposes bytes.
func (x *Xbox360) CanAdmitInputPresentation(claim inputpresentation.Claim,
	admittedAt time.Time) bool {
	x.inputLifecycleMu.Lock()
	accepted := x.input.CanAdmitInputPresentation(claim, admittedAt)
	x.inputLifecycleMu.Unlock()
	return accepted
}

func (x *Xbox360) RetireInputPresentationGeneration(generation uint64,
	retiredAt time.Time) bool {
	x.inputLifecycleMu.Lock()
	retired := x.input.RetireInputPresentationGeneration(generation, retiredAt)
	if retired {
		// A staged snapshot belongs to the producer/presentation lifecycle in
		// which it was captured. Leaving it outside the scheduler generation
		// fence could let a later, unrelated fault resynchronize old state.
		x.clearInputResynchronization()
	}
	x.inputLifecycleMu.Unlock()
	return retired
}

func (x *Xbox360) InputSchedulerSnapshot() inputpresentation.FixedReportSchedulerSnapshot {
	return x.input.Snapshot()
}

func (x *Xbox360) emitRumble(rumble XRumbleState) {
	x.rumbleDispatchMu.Lock()
	defer x.rumbleDispatchMu.Unlock()

	x.rumbleMu.Lock()
	x.rumbleState = rumble
	x.rumbleSeen = true
	rumbleFunc := x.rumbleFunc
	x.rumbleMu.Unlock()

	if rumbleFunc != nil {
		rumbleFunc(rumble)
	}
}

func MakeDescriptor() usb.Descriptor {
	return usb.Descriptor{
		Device: usb.DeviceDescriptor{
			BcdUSB:             0x0200,
			BDeviceClass:       0xff,
			BDeviceSubClass:    0xff,
			BDeviceProtocol:    0xff,
			BMaxPacketSize0:    0x08,
			IDVendor:           0x045e,
			IDProduct:          0x028e,
			BcdDevice:          0x0114,
			IManufacturer:      0x01,
			IProduct:           0x02,
			ISerialNumber:      0x03,
			BNumConfigurations: 0x01,
			Speed:              2, // Full speed
		},
		Interfaces: []usb.InterfaceConfig{
			// Interface 0: ff/5d/01 with 2 interrupt endpoints
			{
				Descriptor: usb.InterfaceDescriptor{
					BInterfaceNumber:   0x00,
					BAlternateSetting:  0x00,
					BNumEndpoints:      0x02,
					BInterfaceClass:    0xff,
					BInterfaceSubClass: 0x5d,
					BInterfaceProtocol: 0x01,
					IInterface:         0x00,
				},
				ClassDescriptors: []usb.ClassSpecificDescriptor{
					{
						DescriptorType: 0x21,
						Payload:        usb.Data{0x00, 0x01, 0x01, 0x25, 0x81, 0x14, 0x00, 0x00, 0x00, 0x00, 0x13, 0x01, 0x08, 0x00, 0x00},
					},
				},
				Endpoints: []usb.EndpointDescriptor{
					// Full-speed interrupt bInterval=1 advertises a 1 ms maximum
					// input service cadence. The USB/IP scheduler still presents only
					// the newest feeder state, so idle pads do not create a busy loop.
					{BEndpointAddress: 0x81, BMAttributes: 0x03, WMaxPacketSize: 0x0020, BInterval: 0x01},
					{BEndpointAddress: 0x01, BMAttributes: 0x03, WMaxPacketSize: 0x0020, BInterval: 0x08},
				},
			},
			// Interface 1: ff/5d/03 with 4 interrupt endpoints
			{
				Descriptor: usb.InterfaceDescriptor{
					BInterfaceNumber:   0x01,
					BAlternateSetting:  0x00,
					BNumEndpoints:      0x04,
					BInterfaceClass:    0xff,
					BInterfaceSubClass: 0x5d,
					BInterfaceProtocol: 0x03,
					IInterface:         0x00,
				},
				ClassDescriptors: []usb.ClassSpecificDescriptor{
					{
						DescriptorType: 0x21,
						Payload:        usb.Data{0x00, 0x01, 0x01, 0x01, 0x82, 0x40, 0x01, 0x02, 0x20, 0x16, 0x83, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x16, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
					},
				},
				Endpoints: []usb.EndpointDescriptor{
					{BEndpointAddress: 0x82, BMAttributes: 0x03, WMaxPacketSize: 0x0020, BInterval: 0x02},
					{BEndpointAddress: 0x02, BMAttributes: 0x03, WMaxPacketSize: 0x0020, BInterval: 0x04},
					{BEndpointAddress: 0x83, BMAttributes: 0x03, WMaxPacketSize: 0x0020, BInterval: 0x40},
					{BEndpointAddress: 0x03, BMAttributes: 0x03, WMaxPacketSize: 0x0020, BInterval: 0x10},
				},
			},
			// Interface 2: ff/5d/02 with 1 interrupt endpoint
			{
				Descriptor: usb.InterfaceDescriptor{
					BInterfaceNumber:   0x02,
					BAlternateSetting:  0x00,
					BNumEndpoints:      0x01,
					BInterfaceClass:    0xff,
					BInterfaceSubClass: 0x5d,
					BInterfaceProtocol: 0x02,
					IInterface:         0x00,
				},
				ClassDescriptors: []usb.ClassSpecificDescriptor{
					{
						DescriptorType: 0x21,
						Payload:        usb.Data{0x00, 0x01, 0x01, 0x22, 0x84, 0x07, 0x00},
					},
				},
				Endpoints: []usb.EndpointDescriptor{
					{
						BEndpointAddress: 0x84,
						BMAttributes:     0x03,
						WMaxPacketSize:   0x0020,
						BInterval:        0x10,
					},
				},
			},
			// Interface 3: ff/fd/13 with vendor-specific descriptor
			{
				Descriptor: usb.InterfaceDescriptor{
					BInterfaceNumber:   0x03,
					BAlternateSetting:  0x00,
					BNumEndpoints:      0x00,
					BInterfaceClass:    0xff,
					BInterfaceSubClass: 0xfd,
					BInterfaceProtocol: 0x13,
					IInterface:         0x04,
				},
				ClassDescriptors: []usb.ClassSpecificDescriptor{
					{DescriptorType: 0x41, Payload: usb.Data{0x00, 0x01, 0x01, 0x03}},
				},
			},
		},
		Strings: map[uint8]string{
			0: "\u0409", // LangID: en-US (0x0409)
			1: "©Microsoft Corporation",
			2: "VIIPER Controller", //"Controller",
			3: "296013F",
		},
	}
}

func (x *Xbox360) GetDescriptor() *usb.Descriptor {
	return &x.descriptor
}

func (x *Xbox360) GetDeviceSpecificArgs() map[string]any {
	args := map[string]any{
		"subType": x.descriptor.Interfaces[0].ClassDescriptors[0].Payload[2],
	}
	if x.maximumOrderedAge > 0 {
		args["maximumOrderedAgeMilliseconds"] =
			x.maximumOrderedAge.Milliseconds()
	}
	return args
}

func (x *Xbox360) HandleControl(bmRequestType, bRequest uint8, wValue, wIndex, wLength uint16, _ []byte) ([]byte, bool) {
	if bmRequestType == 0xC1 && bRequest == 0x01 && wValue == 0x0100 {
		subType := x.descriptor.Interfaces[0].ClassDescriptors[0].Payload[2]
		var extra [6]byte
		switch subType {
		case 0x01: // standard gamepad: vibration motor capabilities
			extra = [6]byte{0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00}
		default: // drums, guitars, etc.: all extended bytes declared capable
			extra = [6]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
		}
		return []byte{
			0x00, 0x14, // report ID, size
			0xFF, 0xFF, // all buttons supported
			0xFF,       // LT
			0xFF,       // RT
			0xFF, 0x7F, // LX
			0xFF, 0x7F, // LY
			0xFF, 0x7F, // RX
			0xFF, 0x7F, // RY
			extra[0], extra[1], extra[2], extra[3], extra[4], extra[5],
		}, true
	}
	return nil, false
}
