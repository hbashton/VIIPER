package xboxone

import (
	"encoding/binary"
	"errors"
	"testing"
)

func testUSBSetup(
	bmRequestType byte,
	bRequest byte,
	wValue uint16,
	wIndex uint16,
	wLength uint16,
) []byte {
	setup := make([]byte, usbSetupPacketSize)
	setup[0] = bmRequestType
	setup[1] = bRequest
	binary.LittleEndian.PutUint16(setup[2:4], wValue)
	binary.LittleEndian.PutUint16(setup[4:6], wIndex)
	binary.LittleEndian.PutUint16(setup[6:8], wLength)
	return setup
}

func newTestUSBControlPlane(t *testing.T) *USBControlPlane {
	t.Helper()
	plane, err := NewUSBControlPlane(testOnlySyntheticControllerProfile(t))
	if err != nil {
		t.Fatalf("NewUSBControlPlane: %v", err)
	}
	return &plane
}

func deliverUSBControl(
	t *testing.T,
	plane *USBControlPlane,
	setup []byte,
) (USBControlClaim, []byte) {
	t.Helper()
	claim, err := plane.Claim(setup)
	if err != nil {
		t.Fatalf("Claim % x: %v", setup, err)
	}
	response := make([]byte, claim.ResponseSize())
	if err := plane.AdmitAndCopy(claim, response); err != nil {
		t.Fatalf("AdmitAndCopy: %v", err)
	}
	if err := plane.Resolve(claim, USBControlDelivered); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return claim, response
}

func TestUSBControlPlaneEnumerationAndVolatileState(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	plane, err := NewUSBControlPlane(profile)
	if err != nil {
		t.Fatalf("NewUSBControlPlane: %v", err)
	}
	if got := plane.Snapshot(); got.State != USBControlDeviceDefault ||
		got.Generation != 1 || got.Address != 0 || got.Configuration != 0 {
		t.Fatalf("initial snapshot = %+v", got)
	}

	claim, response := deliverUSBControl(t, &plane,
		testUSBSetup(0x80, 0x06, 0x0100, 0, 8))
	if claim.ResponseKind() != USBControlResponseDeviceDescriptor || len(response) != 8 {
		t.Fatalf("device descriptor claim/length = %d/%d", claim.ResponseKind(), len(response))
	}
	var fullDevice [USBDeviceDescriptorSize]byte
	if err := profile.EncodeUSBDeviceDescriptorInto(fullDevice[:]); err != nil {
		t.Fatal(err)
	}
	if string(response) != string(fullDevice[:8]) {
		t.Fatalf("device prefix = % x, want % x", response, fullDevice[:8])
	}

	_, response = deliverUSBControl(t, &plane,
		testUSBSetup(0x80, 0x06, 0x0200, 0, 0xffff))
	if len(response) != USBControllerConfigurationDescriptorSize {
		t.Fatalf("configuration response length = %d", len(response))
	}
	_, response = deliverUSBControl(t, &plane,
		testUSBSetup(0x80, 0x06, 0x0300, 0, 4))
	if want := []byte{0x04, 0x03, 0x09, 0x04}; string(response) != string(want) {
		t.Fatalf("LANGID = % x, want % x", response, want)
	}

	setAddress := testUSBSetup(0x00, 0x05, 5, 0, 0)
	claim, err = plane.Claim(setAddress)
	if err != nil {
		t.Fatalf("claim SET_ADDRESS: %v", err)
	}
	if plane.Snapshot().State != USBControlDeviceDefault {
		t.Fatal("SET_ADDRESS effect applied before delivery")
	}
	if err := plane.AdmitAndCopy(claim, nil); err != nil {
		t.Fatalf("admit SET_ADDRESS: %v", err)
	}
	if err := plane.Resolve(claim, USBControlDelivered); err != nil {
		t.Fatalf("resolve SET_ADDRESS: %v", err)
	}
	if got := plane.Snapshot(); got.State != USBControlDeviceAddressed || got.Address != 5 {
		t.Fatalf("addressed snapshot = %+v", got)
	}

	claim, response = deliverUSBControl(t, &plane,
		testUSBSetup(0x80, 0x06, 0x03ee, 0, MicrosoftOSStringDescriptorSize))
	if claim.ResponseKind() != USBControlResponseMicrosoftOSStringDescriptor ||
		len(response) != MicrosoftOSStringDescriptorSize {
		t.Fatalf("MS OS response = kind %d len %d", claim.ResponseKind(), len(response))
	}
	claim, response = deliverUSBControl(t, &plane,
		testUSBSetup(0xc0, MicrosoftOSVendorCode, 0, 4, 16))
	if claim.ResponseKind() != USBControlResponseMicrosoftCompatibleIDDescriptor ||
		len(response) != 16 {
		t.Fatalf("compatible ID header response = kind %d len %d",
			claim.ResponseKind(), len(response))
	}
	var fullCompatible [MicrosoftExtendedCompatibleIDDescriptorSize]byte
	if err := profile.EncodeMicrosoftExtendedCompatibleIDDescriptorInto(
		fullCompatible[:]); err != nil {
		t.Fatal(err)
	}
	if string(response) != string(fullCompatible[:16]) {
		t.Fatalf("compatible ID header = % x, want % x",
			response, fullCompatible[:16])
	}
	claim, response = deliverUSBControl(t, &plane,
		testUSBSetup(0xc0, MicrosoftOSVendorCode, 0, 4,
			MicrosoftExtendedCompatibleIDDescriptorSize))
	if claim.ResponseKind() != USBControlResponseMicrosoftCompatibleIDDescriptor ||
		len(response) != MicrosoftExtendedCompatibleIDDescriptorSize {
		t.Fatalf("compatible ID response = kind %d len %d", claim.ResponseKind(), len(response))
	}
	claim, response = deliverUSBControl(t, &plane,
		testUSBSetup(0xc1, MicrosoftOSVendorCode, 0, 5,
			MicrosoftExtendedPropertiesDescriptorSize))
	if claim.ResponseKind() != USBControlResponseMicrosoftExtendedPropertiesDescriptor ||
		len(response) != MicrosoftExtendedPropertiesDescriptorSize ||
		string(response) != string([]byte{
			0x0a, 0, 0, 0, 0, 1, 5, 0, 0, 0,
		}) {
		t.Fatalf("extended properties response = kind %d wire % x",
			claim.ResponseKind(), response)
	}
	_, response = deliverUSBControl(t, &plane,
		testUSBSetup(0x80, 0x08, 0, 0, 1))
	if len(response) != 1 || response[0] != 0 {
		t.Fatalf("addressed configuration = % x", response)
	}

	deliverUSBControl(t, &plane, testUSBSetup(0x00, 0x09, 1, 0, 0))
	if got := plane.Snapshot(); got.State != USBControlDeviceConfigured || got.Configuration != 1 {
		t.Fatalf("configured snapshot = %+v", got)
	}
	deliverUSBControl(t, &plane,
		testUSBSetup(0x00, 0x03, usbFeatureRemoteWakeup, 0, 0))
	_, response = deliverUSBControl(t, &plane,
		testUSBSetup(0x80, 0x00, 0, 0, 2))
	if len(response) != 2 || response[0] != 0x02 || response[1] != 0 {
		t.Fatalf("device status = % x", response)
	}
	deliverUSBControl(t, &plane,
		testUSBSetup(0x02, 0x03, usbFeatureEndpointHalt, 0x81, 0))
	_, response = deliverUSBControl(t, &plane,
		testUSBSetup(0x82, 0x00, 0, 0x81, 2))
	if len(response) != 2 || response[0] != 1 || response[1] != 0 {
		t.Fatalf("endpoint status = % x", response)
	}

	// Re-selecting the configuration reinitializes endpoint halt state.
	deliverUSBControl(t, &plane, testUSBSetup(0x00, 0x09, 1, 0, 0))
	_, response = deliverUSBControl(t, &plane,
		testUSBSetup(0x82, 0x00, 0, 0x81, 2))
	if response[0] != 0 {
		t.Fatalf("endpoint halt survived SET_CONFIGURATION: % x", response)
	}

	if err := plane.Reset(2); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := plane.Snapshot(); got.State != USBControlDeviceDefault ||
		got.Generation != 2 || got.Address != 0 || got.Configuration != 0 ||
		got.RemoteWakeup || got.EndpointHalt != [3]bool{} {
		t.Fatalf("reset snapshot = %+v", got)
	}
}

func TestUSBControlPlaneZeroValuesFailClosed(t *testing.T) {
	if _, err := NewUSBControlPlane(UnregisteredControllerProfile{}); !errors.Is(
		err, ErrUninitializedControllerProfile) {
		t.Fatalf("zero profile error = %v", err)
	}
	var plane USBControlPlane
	if _, err := plane.Claim(make([]byte, usbSetupPacketSize)); !errors.Is(
		err, ErrUninitializedUSBControlPlane) {
		t.Fatalf("zero plane claim error = %v", err)
	}
	if err := plane.Reset(1); !errors.Is(err, ErrUninitializedUSBControlPlane) {
		t.Fatalf("zero plane reset error = %v", err)
	}
}

func TestUSBControlPlaneKeepsExternalIdentityStringsFailClosed(t *testing.T) {
	plane := newTestUSBControlPlane(t)
	for index := uint16(1); index <= 3; index++ {
		setup := testUSBSetup(0x80, 0x06, 0x0300|index,
			USBEnglishUnitedStatesLanguageID, 0xff)
		if _, err := plane.Claim(setup); !errors.Is(err, ErrUSBStringDescriptorUnavailable) ||
			!errors.Is(err, ErrUSBControlRequestStalled) {
			t.Fatalf("string index %d error = %v", index, err)
		}
		if plane.Snapshot().ClaimOutstanding {
			t.Fatalf("string index %d created a claim", index)
		}
	}
}

func TestUSBControlPlaneStateMatrixFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		setup []byte
	}{
		{name: "device status in default", setup: testUSBSetup(0x80, 0x00, 0, 0, 2)},
		{name: "endpoint status in default", setup: testUSBSetup(0x82, 0x00, 0, 0, 2)},
		{name: "remote wake in default", setup: testUSBSetup(0x00, 0x03, 1, 0, 0)},
		{name: "get configuration in default", setup: testUSBSetup(0x80, 0x08, 0, 0, 1)},
		{name: "set configuration in default", setup: testUSBSetup(0x00, 0x09, 1, 0, 0)},
		{name: "MS OS string in default", setup: testUSBSetup(0x80, 0x06, 0x03ee, 0, 18)},
		{name: "compatible ID in default", setup: testUSBSetup(0xc0, 0x90, 0, 4, 40)},
		{name: "device qualifier", setup: testUSBSetup(0x80, 0x06, 0x0600, 0, 10)},
		{name: "get interface", setup: testUSBSetup(0x81, 0x0a, 0, 0, 1)},
		{name: "set interface", setup: testUSBSetup(0x01, 0x0b, 0, 0, 0)},
		{name: "sync frame", setup: testUSBSetup(0x82, 0x0c, 0, 0x81, 2)},
		{name: "set descriptor", setup: testUSBSetup(0x00, 0x07, 0x0100, 0, 18)},
		{name: "unknown", setup: testUSBSetup(0xff, 0xff, 0xffff, 0xffff, 0xffff)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plane := newTestUSBControlPlane(t)
			if _, err := plane.Claim(test.setup); !errors.Is(err, ErrUSBControlRequestStalled) {
				t.Fatalf("error = %v, want stall", err)
			}
		})
	}

	t.Run("configured rejects set address", func(t *testing.T) {
		plane := newTestUSBControlPlane(t)
		deliverUSBControl(t, plane, testUSBSetup(0x00, 0x05, 1, 0, 0))
		deliverUSBControl(t, plane, testUSBSetup(0x00, 0x09, 1, 0, 0))
		if _, err := plane.Claim(testUSBSetup(0x00, 0x05, 2, 0, 0)); !errors.Is(err, ErrUSBControlRequestStalled) {
			t.Fatalf("SET_ADDRESS error = %v", err)
		}
	})

	t.Run("addressed endpoint scope", func(t *testing.T) {
		plane := newTestUSBControlPlane(t)
		deliverUSBControl(t, plane, testUSBSetup(0x00, 0x05, 1, 0, 0))
		for _, endpoint := range []uint16{0x00, 0x80} {
			deliverUSBControl(t, plane,
				testUSBSetup(0x82, 0x00, 0, endpoint, 2))
		}
		for _, endpoint := range []uint16{0x01, 0x81, 0x82, 0x0180} {
			if _, err := plane.Claim(testUSBSetup(0x82, 0x00, 0, endpoint, 2)); !errors.Is(err, ErrUSBControlRequestStalled) {
				t.Fatalf("endpoint 0x%04x error = %v", endpoint, err)
			}
		}
	})

	t.Run("malformed fields", func(t *testing.T) {
		setups := [][]byte{
			testUSBSetup(0x00, 0x05, 128, 0, 0),
			testUSBSetup(0x00, 0x05, 1, 1, 0),
			testUSBSetup(0x00, 0x05, 1, 0, 1),
			testUSBSetup(0x80, 0x06, 0x0101, 0, 18),
			testUSBSetup(0x80, 0x06, 0x0100, 1, 18),
		}
		for _, setup := range setups {
			plane := newTestUSBControlPlane(t)
			if _, err := plane.Claim(setup); !errors.Is(err, ErrUSBControlRequestStalled) {
				t.Fatalf("setup % x error = %v", setup, err)
			}
		}
	})
}

func TestUSBControlPlaneEndpointHaltRequestMatrixExhaustive(t *testing.T) {
	type operation struct {
		name          string
		bmRequestType byte
		bRequest      byte
		wLength       uint16
	}
	operations := []operation{
		{name: "get status", bmRequestType: usbRequestTypeEndpointIn,
			bRequest: usbRequestGetStatus, wLength: 2},
		{name: "clear halt", bmRequestType: usbRequestTypeEndpointOut,
			bRequest: usbRequestClearFeature},
		{name: "set halt", bmRequestType: usbRequestTypeEndpointOut,
			bRequest: usbRequestSetFeature},
	}
	type stateCase struct {
		name  string
		state USBControlDeviceState
	}
	states := []stateCase{
		{name: "default", state: USBControlDeviceDefault},
		{name: "addressed", state: USBControlDeviceAddressed},
		{name: "configured", state: USBControlDeviceConfigured},
	}

	prepare := func(t *testing.T, state USBControlDeviceState) *USBControlPlane {
		t.Helper()
		plane := newTestUSBControlPlane(t)
		if state == USBControlDeviceDefault {
			return plane
		}
		deliverUSBControl(t, plane,
			testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetAddress, 1, 0, 0))
		if state == USBControlDeviceConfigured {
			deliverUSBControl(t, plane,
				testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration,
					uint16(usbConfigurationGIP), 0, 0))
		}
		return plane
	}

	allowed := func(state USBControlDeviceState, operationName string, endpoint byte) bool {
		control := endpoint == 0x00 || endpoint == 0x80
		data := endpoint == 0x01 || endpoint == 0x81
		switch state {
		case USBControlDeviceAddressed:
			return control && operationName != "set halt"
		case USBControlDeviceConfigured:
			return (control && operationName != "set halt") || data
		default:
			return false
		}
	}

	for _, state := range states {
		for _, operation := range operations {
			t.Run(state.name+"/"+operation.name, func(t *testing.T) {
				for endpointValue := 0; endpointValue <= 0xff; endpointValue++ {
					endpoint := byte(endpointValue)
					plane := prepare(t, state.state)
					before := plane.Snapshot()
					setup := testUSBSetup(operation.bmRequestType, operation.bRequest,
						usbFeatureEndpointHalt, uint16(endpoint), operation.wLength)
					claim, err := plane.Claim(setup)
					if !allowed(state.state, operation.name, endpoint) {
						if !errors.Is(err, ErrUSBControlRequestStalled) {
							t.Fatalf("endpoint 0x%02x error = %v, want stall", endpoint, err)
						}
						if got := plane.Snapshot(); got != before {
							t.Fatalf("endpoint 0x%02x rejected request changed state: before=%+v after=%+v",
								endpoint, before, got)
						}
						continue
					}
					if err != nil {
						t.Fatalf("endpoint 0x%02x error = %v", endpoint, err)
					}
					response := make([]byte, claim.ResponseSize())
					if err := plane.AdmitAndCopy(claim, response); err != nil {
						t.Fatalf("endpoint 0x%02x admit: %v", endpoint, err)
					}
					if err := plane.Resolve(claim, USBControlDelivered); err != nil {
						t.Fatalf("endpoint 0x%02x resolve: %v", endpoint, err)
					}

					if operation.name == "get status" {
						if len(response) != 2 || response[0] != 0 || response[1] != 0 {
							t.Fatalf("endpoint 0x%02x status = % x, want 00 00",
								endpoint, response)
						}
						continue
					}
					if endpoint == 0x00 || endpoint == 0x80 {
						if plane.Snapshot().EndpointHalt[0] {
							t.Fatalf("endpoint 0x%02x changed unsupported EP0 halt state", endpoint)
						}
						continue
					}
					slot := 1
					if endpoint == 0x81 {
						slot = 2
					}
					wantHalt := operation.name == "set halt"
					if got := plane.Snapshot().EndpointHalt[slot]; got != wantHalt {
						t.Fatalf("endpoint 0x%02x halt = %t, want %t",
							endpoint, got, wantHalt)
					}
				}
			})
		}
	}

	// Figure 9-2 reserves the high byte of endpoint wIndex. Check every
	// non-zero high-byte value rather than only a few aliases.
	for high := 1; high <= 0xff; high++ {
		for _, endpoint := range []byte{0x00, 0x01, 0x80, 0x81} {
			for _, operation := range operations {
				plane := prepare(t, USBControlDeviceConfigured)
				setup := testUSBSetup(operation.bmRequestType, operation.bRequest,
					usbFeatureEndpointHalt, uint16(high)<<8|uint16(endpoint), operation.wLength)
				if _, err := plane.Claim(setup); !errors.Is(err, ErrUSBControlRequestStalled) {
					t.Fatalf("high 0x%02x endpoint 0x%02x %s error = %v, want stall",
						high, endpoint, operation.name, err)
				}
			}
		}
	}
}

func TestUSBControlPlaneTransactionsAreAtomicAndGenerationFenced(t *testing.T) {
	plane := newTestUSBControlPlane(t)
	for _, size := range []int{usbSetupPacketSize - 1, usbSetupPacketSize + 1} {
		if _, err := plane.Claim(make([]byte, size)); !errors.Is(err, ErrInvalidUSBSetupPacket) {
			t.Fatalf("setup length %d error = %v", size, err)
		}
	}

	setup := testUSBSetup(0x80, 0x06, 0x0100, 0, 8)
	claim, err := plane.Claim(setup)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := plane.Claim(setup); !errors.Is(err, ErrUSBControlClaimOutstanding) {
		t.Fatalf("second Claim error = %v", err)
	}
	if err := plane.Resolve(claim, USBControlDelivered); !errors.Is(err, ErrUSBControlClaimNotAdmitted) {
		t.Fatalf("unadmitted delivery error = %v", err)
	}
	if err := plane.Resolve(claim, USBControlOutcome(0xff)); !errors.Is(err, ErrInvalidUSBControlOutcome) {
		t.Fatalf("invalid outcome error = %v", err)
	}
	for _, size := range []int{claim.ResponseSize() - 1, claim.ResponseSize() + 1} {
		destination := make([]byte, size)
		for index := range destination {
			destination[index] = 0xa5
		}
		before := append([]byte(nil), destination...)
		if err := plane.AdmitAndCopy(claim, destination); !errors.Is(err, ErrInvalidUSBControlDestination) {
			t.Fatalf("destination length %d error = %v", size, err)
		}
		if string(destination) != string(before) {
			t.Fatalf("destination length %d changed", size)
		}
	}
	forged := claim
	forged.responseSize++
	if err := plane.AdmitAndCopy(forged, make([]byte, forged.ResponseSize())); !errors.Is(err, ErrInvalidUSBControlClaim) {
		t.Fatalf("forged claim error = %v", err)
	}
	response := make([]byte, claim.ResponseSize())
	if err := plane.AdmitAndCopy(claim, response); err != nil {
		t.Fatalf("admit exact: %v", err)
	}
	if err := plane.AdmitAndCopy(claim, response); !errors.Is(err, ErrInvalidUSBControlClaim) {
		t.Fatalf("duplicate admission error = %v", err)
	}
	if err := plane.Reset(2); !errors.Is(err, ErrUSBControlBoundaryBlocked) {
		t.Fatalf("reset with claim error = %v", err)
	}
	if err := plane.Resolve(claim, USBControlExecutionCancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := plane.Reset(2); err != nil {
		t.Fatalf("reset after drain: %v", err)
	}
	if err := plane.Resolve(claim, USBControlDeliveryFailed); !errors.Is(err, ErrInvalidUSBControlClaim) {
		t.Fatalf("stale resolution error = %v", err)
	}

	setAddress := testUSBSetup(0x00, 0x05, 7, 0, 0)
	claim, err = plane.Claim(setAddress)
	if err != nil {
		t.Fatalf("claim SET_ADDRESS: %v", err)
	}
	if err := plane.AdmitAndCopy(claim, nil); err != nil {
		t.Fatalf("admit SET_ADDRESS: %v", err)
	}
	if err := plane.Resolve(claim, USBControlDeliveryFailed); err != nil {
		t.Fatalf("fail SET_ADDRESS: %v", err)
	}
	if plane.Snapshot().State != USBControlDeviceDefault {
		t.Fatal("failed SET_ADDRESS changed state")
	}
	deliverUSBControl(t, plane, setAddress)
	deliverUSBControl(t, plane,
		testUSBSetup(0x00, 0x03, usbFeatureRemoteWakeup, 0, 0))
	deliverUSBControl(t, plane,
		testUSBSetup(0x02, 0x01, usbFeatureEndpointHalt, 0x00, 0))
	deliverUSBControl(t, plane, testUSBSetup(0x00, 0x05, 0, 0, 0))
	if got := plane.Snapshot(); got.State != USBControlDeviceDefault ||
		got.Address != 0 || got.Configuration != 0 || got.RemoteWakeup ||
		got.EndpointHalt != [3]bool{} {
		t.Fatalf("SET_ADDRESS zero snapshot = %+v", got)
	}

	if err := plane.Disconnect(3); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if got := plane.Snapshot(); got.State != USBControlDeviceDetached ||
		got.Address != 0 || got.Configuration != 0 || got.RemoteWakeup {
		t.Fatalf("detached snapshot = %+v", got)
	}
	if _, err := plane.Claim(setup); !errors.Is(err, ErrUSBControlPlaneDetached) {
		t.Fatalf("detached claim error = %v", err)
	}
	if err := plane.Reset(4); !errors.Is(err, ErrInvalidUSBControlTransition) {
		t.Fatalf("reset while detached error = %v", err)
	}
	if err := plane.Disconnect(4); !errors.Is(err, ErrInvalidUSBControlTransition) {
		t.Fatalf("double disconnect error = %v", err)
	}
	if err := plane.Reconnect(4); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if got := plane.Snapshot(); got.State != USBControlDeviceDefault || got.Generation != 4 {
		t.Fatalf("reconnected snapshot = %+v", got)
	}
	if err := plane.Reconnect(5); !errors.Is(err, ErrInvalidUSBControlTransition) {
		t.Fatalf("reconnect while attached error = %v", err)
	}
}

func TestUSBControlPlaneFixedDescriptorPathAllocatesZero(t *testing.T) {
	plane := newTestUSBControlPlane(t)
	setup := [usbSetupPacketSize]byte{0x80, 0x06, 0x00, 0x01, 0, 0, 8, 0}
	response := [8]byte{}
	if allocs := testing.AllocsPerRun(1000, func() {
		claim, err := plane.Claim(setup[:])
		if err != nil {
			panic(err)
		}
		if err := plane.AdmitAndCopy(claim, response[:]); err != nil {
			panic(err)
		}
		if err := plane.Resolve(claim, USBControlDelivered); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("EP0 fixed descriptor allocations = %v, want 0", allocs)
	}
}
