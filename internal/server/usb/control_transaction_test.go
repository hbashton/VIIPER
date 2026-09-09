package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	usbdesc "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

const (
	transactionalControlStallRequest     = 0xfc
	transactionalControlUnhandledRequest = 0xfd
)

var errTransactionalControlTestClaim = errors.New("invalid transactional test claim")

type transactionalControlTestDevice struct {
	desc *usbdesc.Descriptor

	mu                     sync.Mutex
	generation             uint64
	nextToken              uint64
	outstanding            bool
	activeClaim            usbdesc.ControlTransactionClaim
	admitted               bool
	pendingData            []byte
	pendingConfiguration   byte
	pendingConfigValid     bool
	configuration          byte
	configurationCommits   int
	pendingAlt             byte
	pendingAltValid        bool
	interfaceAlt           byte
	pendingEndpointClear   bool
	endpointClears         int
	claimCalls             int
	admissionCalls         int
	completionAttempts     int
	completionOutcomes     []usbdesc.ControlTransactionOutcome
	legacyControlCalls     int
	forcedAdmissionError   error
	returnForgedClaim      bool
	makeReturnedClaimStale bool
	stallConfiguration     bool
	zeroLengthDataClaim    bool
	admittedSignal         chan struct{}
	admittedSignalOnce     sync.Once
}

func newTransactionalControlTestDevice() *transactionalControlTestDevice {
	return &transactionalControlTestDevice{
		desc: &usbdesc.Descriptor{
			Device: usbdesc.DeviceDescriptor{
				BcdUSB: 0x0200, BMaxPacketSize0: 64,
				IDVendor: 0x1209, IDProduct: 0x0001,
				BNumConfigurations: 1, Speed: 2,
			},
			Configuration: usbdesc.ConfigurationDescriptor{
				BConfigurationValue: 1,
			},
			Interfaces: []usbdesc.InterfaceConfig{
				{
					Descriptor: usbdesc.InterfaceDescriptor{
						BInterfaceNumber: 0, BAlternateSetting: 0,
						BNumEndpoints: 1,
					},
					Endpoints: []usbdesc.EndpointDescriptor{{
						BEndpointAddress: 0x81, BMAttributes: 0x03,
						WMaxPacketSize: 64, BInterval: 4,
					}},
				},
				{
					Descriptor: usbdesc.InterfaceDescriptor{
						BInterfaceNumber: 0, BAlternateSetting: 1,
						BNumEndpoints: 1,
					},
					Endpoints: []usbdesc.EndpointDescriptor{{
						BEndpointAddress: 0x81, BMAttributes: 0x03,
						WMaxPacketSize: 64, BInterval: 8,
					}},
				},
			},
			Strings: map[uint8]string{0: "\u0409"},
		},
		generation: 1,
	}
}

func (device *transactionalControlTestDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}

func (device *transactionalControlTestDevice) GetDescriptor() *usbdesc.Descriptor {
	return device.desc
}

func (*transactionalControlTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (device *transactionalControlTestDevice) HandleControl(
	_ uint8,
	bRequest uint8,
	_ uint16,
	_ uint16,
	_ uint16,
	_ []byte,
) ([]byte, bool) {
	device.mu.Lock()
	defer device.mu.Unlock()
	device.legacyControlCalls++
	if bRequest == transactionalControlUnhandledRequest {
		return []byte{0x44}, true
	}
	return nil, false
}

func (device *transactionalControlTestDevice) ClaimControlTransaction(
	request usbdesc.ControlTransactionRequest,
) (usbdesc.ControlTransactionClaim, error) {
	device.mu.Lock()
	defer device.mu.Unlock()
	device.claimCalls++
	if cap(request.Data) != len(request.Data) {
		return usbdesc.ControlTransactionClaim{}, errTransactionalControlTestClaim
	}

	if request.Setup[1] == transactionalControlUnhandledRequest {
		return usbdesc.ControlTransactionClaim{}, nil
	}
	if device.outstanding {
		return usbdesc.ControlTransactionClaim{}, errTransactionalControlTestClaim
	}

	var result usbdesc.ControlTransactionResult
	data := []byte(nil)
	configuration := byte(0)
	configurationValid := false
	alt := byte(0)
	altValid := false
	endpointClear := false
	switch request.Setup[1] {
	case usbReqGetDescriptor:
		result = usbdesc.ControlTransactionData
		data = []byte{0xa4, 0xb5, 0xc6, 0xd7}
	case usbReqGetStatus:
		result = usbdesc.ControlTransactionData
		if !device.zeroLengthDataClaim {
			data = []byte{0x02, 0x00}
		}
	case usbReqSetConfiguration:
		if device.stallConfiguration {
			result = usbdesc.ControlTransactionStall
		} else {
			result = usbdesc.ControlTransactionNoData
			configuration = request.Setup[2]
			configurationValid = true
		}
	case usbReqSetInterface:
		result = usbdesc.ControlTransactionNoData
		alt = request.Setup[2]
		altValid = true
	case usbReqClearFeature:
		result = usbdesc.ControlTransactionNoData
		endpointClear = true
	case transactionalControlStallRequest:
		result = usbdesc.ControlTransactionStall
	default:
		return usbdesc.ControlTransactionClaim{}, nil
	}

	device.nextToken++
	if device.nextToken == 0 {
		device.nextToken++
	}
	claim := usbdesc.ControlTransactionClaim{
		Token: device.nextToken, Generation: device.generation,
		Result: result, ResponseLength: uint32(len(data)),
	}
	device.outstanding = true
	device.activeClaim = claim
	device.admitted = false
	device.pendingData = append(device.pendingData[:0], data...)
	device.pendingConfiguration = configuration
	device.pendingConfigValid = configurationValid
	device.pendingAlt = alt
	device.pendingAltValid = altValid
	device.pendingEndpointClear = endpointClear

	returned := claim
	if device.returnForgedClaim {
		returned.Token++
	}
	if device.makeReturnedClaimStale {
		device.generation++
	}
	return returned, nil
}

func (device *transactionalControlTestDevice) AdmitControlTransaction(
	claim usbdesc.ControlTransactionClaim,
	destination []byte,
) error {
	device.mu.Lock()
	defer device.mu.Unlock()
	device.admissionCalls++
	if !device.validClaimLocked(claim) || device.admitted ||
		len(destination) != len(device.pendingData) ||
		cap(destination) != len(destination) {
		return errTransactionalControlTestClaim
	}
	if device.forcedAdmissionError != nil {
		return device.forcedAdmissionError
	}
	copy(destination, device.pendingData)
	device.admitted = true
	if device.admittedSignal != nil {
		device.admittedSignalOnce.Do(func() { close(device.admittedSignal) })
	}
	return nil
}

func (device *transactionalControlTestDevice) CompleteControlTransaction(
	claim usbdesc.ControlTransactionClaim,
	outcome usbdesc.ControlTransactionOutcome,
) error {
	device.mu.Lock()
	defer device.mu.Unlock()
	device.completionAttempts++
	if !device.validClaimLocked(claim) {
		return errTransactionalControlTestClaim
	}
	if outcome < usbdesc.ControlTransactionDelivered ||
		outcome > usbdesc.ControlTransactionCancelled ||
		(outcome == usbdesc.ControlTransactionDelivered && !device.admitted) {
		return errTransactionalControlTestClaim
	}
	device.completionOutcomes = append(device.completionOutcomes, outcome)
	if outcome == usbdesc.ControlTransactionDelivered &&
		device.pendingConfigValid {
		device.configuration = device.pendingConfiguration
		device.configurationCommits++
	}
	if outcome == usbdesc.ControlTransactionDelivered &&
		device.pendingAltValid {
		device.interfaceAlt = device.pendingAlt
	}
	if outcome == usbdesc.ControlTransactionDelivered &&
		device.pendingEndpointClear {
		device.endpointClears++
	}
	device.outstanding = false
	device.activeClaim = usbdesc.ControlTransactionClaim{}
	device.admitted = false
	device.pendingData = device.pendingData[:0]
	device.pendingConfiguration = 0
	device.pendingConfigValid = false
	device.pendingAlt = 0
	device.pendingAltValid = false
	device.pendingEndpointClear = false
	return nil
}

func (device *transactionalControlTestDevice) validClaimLocked(
	claim usbdesc.ControlTransactionClaim,
) bool {
	return device.outstanding && claim.Valid() && claim.Handled() &&
		claim == device.activeClaim && claim.Generation == device.generation
}

type transactionalControlSnapshot struct {
	configuration        byte
	configurationCommits int
	interfaceAlt         byte
	endpointClears       int
	outstanding          bool
	claimCalls           int
	admissionCalls       int
	completionAttempts   int
	completionOutcomes   []usbdesc.ControlTransactionOutcome
	legacyControlCalls   int
}

func (device *transactionalControlTestDevice) snapshot() transactionalControlSnapshot {
	device.mu.Lock()
	defer device.mu.Unlock()
	outcomes := append([]usbdesc.ControlTransactionOutcome(nil),
		device.completionOutcomes...)
	return transactionalControlSnapshot{
		configuration: device.configuration, outstanding: device.outstanding,
		configurationCommits: device.configurationCommits,
		interfaceAlt:         device.interfaceAlt, endpointClears: device.endpointClears,
		claimCalls: device.claimCalls, admissionCalls: device.admissionCalls,
		completionAttempts: device.completionAttempts,
		completionOutcomes: outcomes,
		legacyControlCalls: device.legacyControlCalls,
	}
}

type transactionalControlStream struct {
	client net.Conn
	errors <-chan error
	server *Server
}

func newTransactionalControlStream(
	t *testing.T,
	device *transactionalControlTestDevice,
) transactionalControlStream {
	t.Helper()
	bus := virtualbus.New(249)
	_, err := bus.Add(device)
	require.NoError(t, err)
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	serverConn, clientConn := net.Pipe()
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(2*time.Second)))
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.handleUrbStream(serverConn, device)
		_ = serverConn.Close()
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = bus.Close()
	})
	return transactionalControlStream{
		client: clientConn, errors: errCh, server: server,
	}
}

type transactionalControlResponse struct {
	status int32
	actual uint32
	data   []byte
}

func submitTransactionalControl(
	t *testing.T,
	connection net.Conn,
	seq, direction uint32,
	setup [8]byte,
) transactionalControlResponse {
	t.Helper()
	length := uint32(0)
	if direction == usbip.DirIn {
		length = uint32(binary.LittleEndian.Uint16(setup[6:8]))
	}
	command := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdSubmitCode, Seqnum: seq,
			Dir: direction, Ep: 0,
		},
		TransferBufferLen: length,
		NumberOfPackets:   -1,
		Setup:             setup,
	}
	require.NoError(t, command.Write(connection))
	var header [retSubmitHeaderSize]byte
	require.NoError(t, usbip.ReadExactly(connection, header[:]))
	response := transactionalControlResponse{
		status: int32(binary.BigEndian.Uint32(header[20:24])),
		actual: binary.BigEndian.Uint32(header[24:28]),
	}
	response.data = make([]byte, response.actual)
	if response.actual != 0 {
		require.NoError(t, usbip.ReadExactly(connection, response.data))
	}
	return response
}

func submitTransactionalEndpointIn(
	t *testing.T,
	connection net.Conn,
	seq, endpoint, length uint32,
) transactionalControlResponse {
	t.Helper()
	command := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdSubmitCode, Seqnum: seq,
			Dir: usbip.DirIn, Ep: endpoint,
		},
		TransferBufferLen: length,
		NumberOfPackets:   -1,
	}
	require.NoError(t, command.Write(connection))
	var header [retSubmitHeaderSize]byte
	require.NoError(t, usbip.ReadExactly(connection, header[:]))
	response := transactionalControlResponse{
		status: int32(binary.BigEndian.Uint32(header[20:24])),
		actual: binary.BigEndian.Uint32(header[24:28]),
	}
	response.data = make([]byte, response.actual)
	if response.actual != 0 {
		require.NoError(t, usbip.ReadExactly(connection, response.data))
	}
	return response
}

func transactionalControlSetup(
	bmRequestType, bRequest uint8,
	wValue, wIndex, wLength uint16,
) [8]byte {
	var setup [8]byte
	setup[0] = bmRequestType
	setup[1] = bRequest
	binary.LittleEndian.PutUint16(setup[2:4], wValue)
	binary.LittleEndian.PutUint16(setup[4:6], wIndex)
	binary.LittleEndian.PutUint16(setup[6:8], wLength)
	return setup
}

func TestBuildTransactionalControlResponseMatchesCanonicalRETSubmit(
	t *testing.T,
) {
	for _, test := range []struct {
		name   string
		status int32
		data   []byte
		actual uint32
	}{
		{name: "data", data: []byte{1, 2, 3}, actual: 3},
		{name: "no data"},
		{name: "stall", status: errPipe},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := buildTransactionalControlResponse(
				nil, 99, test.status, test.actual, uint32(len(test.data)))
			copy(got[retSubmitHeaderSize:], test.data)
			want := buildRetSubmitPacket(
				nil, 99, test.status, test.actual, test.data, nil, false)
			require.Equal(t, want, got)
		})
	}
}

func transactionalControlEndpointGenerationTotal(
	server *Server,
) (uint64, bool) {
	snapshot := server.EndpointDiagnosticsSnapshot()
	if len(snapshot.Connections) != 1 {
		return 0, false
	}
	var total uint64
	count := 0
	for _, endpoint := range snapshot.Connections[0].Endpoints {
		if endpoint.Endpoint == 1 && endpoint.Direction == usbip.DirIn {
			total += endpoint.Generation
			count++
		}
	}
	return total, count == 2
}

func TestTransactionalControlOwnsEP0BeforeGenericAndLegacyHandling(t *testing.T) {
	device := newTransactionalControlTestDevice()
	stream := newTransactionalControlStream(t, device)

	descriptor := submitTransactionalControl(t, stream.client, 1, usbip.DirIn,
		transactionalControlSetup(usbReqTypeStandardFromDevice,
			usbReqGetDescriptor, 0x0100, 0, 4))
	require.Zero(t, descriptor.status)
	require.Equal(t, uint32(4), descriptor.actual)
	require.Equal(t, []byte{0xa4, 0xb5, 0xc6, 0xd7}, descriptor.data)

	status := submitTransactionalControl(t, stream.client, 2, usbip.DirIn,
		transactionalControlSetup(usbReqTypeStandardFromDevice,
			usbReqGetStatus, 0, 0, 2))
	require.Zero(t, status.status)
	require.Equal(t, []byte{0x02, 0x00}, status.data)

	configuration := submitTransactionalControl(t, stream.client, 3,
		usbip.DirOut, transactionalControlSetup(usbReqTypeStandardToDevice,
			usbReqSetConfiguration, 1, 0, 0))
	require.Zero(t, configuration.status)
	require.Zero(t, configuration.actual)
	require.Empty(t, configuration.data)

	stalled := submitTransactionalControl(t, stream.client, 4, usbip.DirIn,
		transactionalControlSetup(0xc0,
			transactionalControlStallRequest, 0, 0, 0))
	require.Equal(t, int32(errPipe), stalled.status)
	require.Zero(t, stalled.actual)

	unhandled := submitTransactionalControl(t, stream.client, 5, usbip.DirIn,
		transactionalControlSetup(0xc0,
			transactionalControlUnhandledRequest, 0, 0, 1))
	require.Zero(t, unhandled.status)
	require.Equal(t, []byte{0x44}, unhandled.data)

	snapshot := device.snapshot()
	require.Equal(t, byte(1), snapshot.configuration)
	require.Equal(t, 1, snapshot.configurationCommits)
	require.False(t, snapshot.outstanding)
	require.Equal(t, 5, snapshot.claimCalls)
	require.Equal(t, 4, snapshot.admissionCalls)
	require.Equal(t, 4, snapshot.completionAttempts)
	require.Equal(t, []usbdesc.ControlTransactionOutcome{
		usbdesc.ControlTransactionDelivered,
		usbdesc.ControlTransactionDelivered,
		usbdesc.ControlTransactionDelivered,
		usbdesc.ControlTransactionDelivered,
	}, snapshot.completionOutcomes)
	require.Equal(t, 1, snapshot.legacyControlCalls)

	require.NoError(t, stream.client.Close())
	require.Error(t, <-stream.errors)
}

func TestTransactionalControlDeliveredLifecycleSynchronizesServerState(
	t *testing.T,
) {
	device := newTransactionalControlTestDevice()
	stream := newTransactionalControlStream(t, device)
	var initialGeneration uint64
	require.Eventually(t, func() bool {
		var ready bool
		initialGeneration, ready =
			transactionalControlEndpointGenerationTotal(stream.server)
		return ready
	}, 2*time.Second, time.Millisecond)
	require.Equal(t, uint8(0), stream.server.getDeviceConfiguration(device))

	configure := submitTransactionalControl(t, stream.client, 19,
		usbip.DirOut, transactionalControlSetup(
			usbReqTypeStandardToDevice, usbReqSetConfiguration,
			1, 0, 0))
	require.Zero(t, configure.status)
	require.Eventually(t, func() bool {
		generation, ready :=
			transactionalControlEndpointGenerationTotal(stream.server)
		snapshot := device.snapshot()
		return ready && generation > initialGeneration &&
			stream.server.getDeviceConfiguration(device) == 1 &&
			snapshot.configuration == 1 &&
			snapshot.configurationCommits == 1
	}, 2*time.Second, time.Millisecond)
	afterConfiguration, _ :=
		transactionalControlEndpointGenerationTotal(stream.server)

	setInterface := submitTransactionalControl(t, stream.client, 20,
		usbip.DirOut, transactionalControlSetup(
			usbReqTypeStandardFromInterface, usbReqSetInterface,
			1, 0, 0))
	require.Zero(t, setInterface.status)
	require.Eventually(t, func() bool {
		generation, ready :=
			transactionalControlEndpointGenerationTotal(stream.server)
		return ready && generation > afterConfiguration &&
			stream.server.getInterfaceAlt(device, 0) == 1 &&
			device.snapshot().interfaceAlt == 1
	}, 2*time.Second, time.Millisecond)
	afterInterface, _ :=
		transactionalControlEndpointGenerationTotal(stream.server)

	clearHalt := submitTransactionalControl(t, stream.client, 21,
		usbip.DirOut, transactionalControlSetup(
			usbReqTypeStandardToEndpoint, usbReqClearFeature,
			0, 0x0081, 0))
	require.Zero(t, clearHalt.status)
	require.Eventually(t, func() bool {
		generation, ready :=
			transactionalControlEndpointGenerationTotal(stream.server)
		return ready && generation > afterInterface &&
			device.snapshot().endpointClears == 1
	}, 2*time.Second, time.Millisecond)
	afterClear, _ := transactionalControlEndpointGenerationTotal(stream.server)

	unconfigure := submitTransactionalControl(t, stream.client, 22,
		usbip.DirOut, transactionalControlSetup(
			usbReqTypeStandardToDevice, usbReqSetConfiguration,
			0, 0, 0))
	require.Zero(t, unconfigure.status)
	require.Eventually(t, func() bool {
		generation, ready :=
			transactionalControlEndpointGenerationTotal(stream.server)
		snapshot := device.snapshot()
		return ready && generation > afterClear &&
			stream.server.getDeviceConfiguration(device) == 0 &&
			stream.server.getInterfaceAlt(device, 0) == 0 &&
			snapshot.configuration == 0 &&
			snapshot.configurationCommits == 2
	}, 2*time.Second, time.Millisecond)

	require.NoError(t, stream.client.Close())
	require.Error(t, <-stream.errors)
}

func TestTransactionalControlStartsWithNonzeroEndpointsInactive(t *testing.T) {
	device := newTransactionalControlTestDevice()
	stream := newTransactionalControlStream(t, device)

	beforeConfiguration := submitTransactionalEndpointIn(
		t, stream.client, 26, 1, 64)
	require.Equal(t, int32(errPipe), beforeConfiguration.status)
	require.Zero(t, beforeConfiguration.actual)
	require.Equal(t, uint8(0), stream.server.getDeviceConfiguration(device))

	configure := submitTransactionalControl(t, stream.client, 27,
		usbip.DirOut, transactionalControlSetup(
			usbReqTypeStandardToDevice, usbReqSetConfiguration,
			1, 0, 0))
	require.Zero(t, configure.status)

	afterConfiguration := submitTransactionalEndpointIn(
		t, stream.client, 28, 1, 64)
	require.Zero(t, afterConfiguration.status)
	require.Zero(t, afterConfiguration.actual)
	require.Equal(t, uint8(1), stream.server.getDeviceConfiguration(device))

	require.NoError(t, stream.client.Close())
	require.Error(t, <-stream.errors)
}

func TestTransactionalControlLifecycleStallDoesNotMutateServerState(
	t *testing.T,
) {
	device := newTransactionalControlTestDevice()
	device.stallConfiguration = true
	stream := newTransactionalControlStream(t, device)
	var initialGeneration uint64
	require.Eventually(t, func() bool {
		var ready bool
		initialGeneration, ready =
			transactionalControlEndpointGenerationTotal(stream.server)
		return ready
	}, 2*time.Second, time.Millisecond)

	response := submitTransactionalControl(t, stream.client, 23,
		usbip.DirOut, transactionalControlSetup(
			usbReqTypeStandardToDevice, usbReqSetConfiguration,
			1, 0, 0))
	require.Equal(t, int32(errPipe), response.status)
	// A following unhandled request proves the STALL completion callback has
	// returned before state is inspected.
	fallback := submitTransactionalControl(t, stream.client, 24,
		usbip.DirIn, transactionalControlSetup(
			0xc0, transactionalControlUnhandledRequest, 0, 0, 1))
	require.Zero(t, fallback.status)

	generation, ready :=
		transactionalControlEndpointGenerationTotal(stream.server)
	require.True(t, ready)
	require.Equal(t, initialGeneration, generation)
	require.Equal(t, uint8(0), stream.server.getDeviceConfiguration(device))
	snapshot := device.snapshot()
	require.Zero(t, snapshot.configurationCommits)
	require.Zero(t, snapshot.configuration)
	require.Equal(t, []usbdesc.ControlTransactionOutcome{
		usbdesc.ControlTransactionDelivered,
	}, snapshot.completionOutcomes)

	require.NoError(t, stream.client.Close())
	require.Error(t, <-stream.errors)
}

func TestTransactionalControlAdmissionFailureCancelsWithoutResponseOrEffect(
	t *testing.T,
) {
	device := newTransactionalControlTestDevice()
	device.forcedAdmissionError = errors.New("stale at final admission")
	stream := newTransactionalControlStream(t, device)

	command := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdSubmitCode, Seqnum: 10,
			Dir: usbip.DirOut, Ep: 0,
		},
		NumberOfPackets: -1,
		Setup: transactionalControlSetup(usbReqTypeStandardToDevice,
			usbReqSetConfiguration, 1, 0, 0),
	}
	require.NoError(t, command.Write(stream.client))
	var one [1]byte
	_, readErr := stream.client.Read(one[:])
	require.Error(t, readErr)
	require.ErrorContains(t, <-stream.errors, "stale at final admission")

	snapshot := device.snapshot()
	require.Zero(t, snapshot.configuration)
	require.Zero(t, snapshot.configurationCommits)
	require.Equal(t, uint8(1), stream.server.getDeviceConfiguration(device))
	require.False(t, snapshot.outstanding)
	require.Equal(t, 1, snapshot.admissionCalls)
	require.Equal(t, 1, snapshot.completionAttempts)
	require.Equal(t, []usbdesc.ControlTransactionOutcome{
		usbdesc.ControlTransactionCancelled,
	}, snapshot.completionOutcomes)
}

func TestTransactionalControlConnectionLossCompletesDeliveryFailedOnce(
	t *testing.T,
) {
	device := newTransactionalControlTestDevice()
	device.admittedSignal = make(chan struct{})
	stream := newTransactionalControlStream(t, device)

	command := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdSubmitCode, Seqnum: 11,
			Dir: usbip.DirOut, Ep: 0,
		},
		NumberOfPackets: -1,
		Setup: transactionalControlSetup(usbReqTypeStandardToDevice,
			usbReqSetConfiguration, 0, 0, 0),
	}
	require.NoError(t, command.Write(stream.client))
	select {
	case <-device.admittedSignal:
	case <-time.After(2 * time.Second):
		t.Fatal("transaction was not admitted")
	}
	require.NoError(t, stream.client.Close())
	require.Error(t, <-stream.errors)

	snapshot := device.snapshot()
	require.Zero(t, snapshot.configuration)
	require.Zero(t, snapshot.configurationCommits)
	require.Equal(t, uint8(1), stream.server.getDeviceConfiguration(device))
	require.False(t, snapshot.outstanding)
	require.Equal(t, 1, snapshot.admissionCalls)
	require.Equal(t, 1, snapshot.completionAttempts)
	require.Equal(t, []usbdesc.ControlTransactionOutcome{
		usbdesc.ControlTransactionDeliveryFailed,
	}, snapshot.completionOutcomes)
}

func TestTransactionalControlDataCannotExceedSetupWLength(t *testing.T) {
	device := newTransactionalControlTestDevice()
	stream := newTransactionalControlStream(t, device)

	// The USB/IP receive buffer is large enough for the fake device's four-byte
	// descriptor, but the control setup packet requests only two bytes. The
	// setup packet remains the device-to-host data-stage bound.
	command := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdSubmitCode, Seqnum: 25,
			Dir: usbip.DirIn, Ep: 0,
		},
		TransferBufferLen: 4,
		NumberOfPackets:   -1,
		Setup: transactionalControlSetup(
			usbReqTypeStandardFromDevice, usbReqGetDescriptor,
			0x0100, 0, 2),
	}
	require.NoError(t, command.Write(stream.client))
	var one [1]byte
	_, readErr := stream.client.Read(one[:])
	require.Error(t, readErr)
	require.ErrorContains(t, <-stream.errors, "invalid result envelope")

	snapshot := device.snapshot()
	require.Zero(t, snapshot.admissionCalls)
	require.False(t, snapshot.outstanding)
	require.Equal(t, 1, snapshot.completionAttempts)
	require.Equal(t, []usbdesc.ControlTransactionOutcome{
		usbdesc.ControlTransactionCancelled,
	}, snapshot.completionOutcomes)
}

func TestTransactionalControlMalformedForgedAndStaleClaimsFailClosed(
	t *testing.T,
) {
	for _, test := range []struct {
		name              string
		mutate            func(*transactionalControlTestDevice)
		request           uint8
		direction         uint32
		wLength           uint16
		admissionAttempts int
	}{
		{name: "forged token", mutate: func(device *transactionalControlTestDevice) {
			device.returnForgedClaim = true
		}, request: usbReqSetConfiguration, direction: usbip.DirOut,
			admissionAttempts: 1},
		{name: "stale generation", mutate: func(device *transactionalControlTestDevice) {
			device.makeReturnedClaimStale = true
		}, request: usbReqSetConfiguration, direction: usbip.DirOut,
			admissionAttempts: 1},
		{name: "zero-length data result", mutate: func(device *transactionalControlTestDevice) {
			device.zeroLengthDataClaim = true
		}, request: usbReqGetStatus, direction: usbip.DirIn, wLength: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			device := newTransactionalControlTestDevice()
			test.mutate(device)
			stream := newTransactionalControlStream(t, device)
			command := usbip.CmdSubmit{
				Basic: usbip.HeaderBasic{
					Command: usbip.CmdSubmitCode, Seqnum: 12,
					Dir: test.direction, Ep: 0,
				},
				TransferBufferLen: uint32(test.wLength),
				NumberOfPackets:   -1,
				Setup: transactionalControlSetup(0x80,
					test.request, 1, 0, test.wLength),
			}
			if test.direction == usbip.DirOut {
				command.Setup[0] = usbReqTypeStandardToDevice
			}
			require.NoError(t, command.Write(stream.client))
			var one [1]byte
			_, readErr := stream.client.Read(one[:])
			require.Error(t, readErr)
			require.Error(t, <-stream.errors)

			snapshot := device.snapshot()
			require.Zero(t, snapshot.configuration)
			require.True(t, snapshot.outstanding,
				"a device which returned a forged/stale claim retains its own invalid capability")
			require.Equal(t, test.admissionAttempts, snapshot.admissionCalls)
			require.Equal(t, 1, snapshot.completionAttempts,
				"server must make one terminal cancellation attempt")
			require.Empty(t, snapshot.completionOutcomes)
		})
	}
}
