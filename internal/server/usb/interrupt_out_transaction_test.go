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

var errInterruptOutTestClaim = errors.New("invalid interrupt OUT test claim")

type transactionalInterruptOutTestDevice struct {
	desc *usbdesc.Descriptor

	mu                     sync.Mutex
	generation             uint64
	nextToken              uint64
	activeClaim            usbdesc.InterruptOutTransactionClaim
	outstanding            bool
	admitted               bool
	pendingPayload         []byte
	committedPayloads      [][]byte
	claimCalls             int
	admissionCalls         int
	completionAttempts     int
	completionOutcomes     []usbdesc.InterruptOutTransactionOutcome
	legacyCalls            int
	legacyPayloads         [][]byte
	forcedAdmissionError   error
	returnForgedClaim      bool
	makeReturnedClaimStale bool
}

func newTransactionalInterruptOutTestDevice() *transactionalInterruptOutTestDevice {
	return &transactionalInterruptOutTestDevice{
		desc: &usbdesc.Descriptor{
			Device: usbdesc.DeviceDescriptor{
				BcdUSB: 0x0200, BMaxPacketSize0: 64,
				IDVendor: 0x1209, IDProduct: 0x0002,
				BNumConfigurations: 1, Speed: 2,
			},
			Configuration: usbdesc.ConfigurationDescriptor{
				BConfigurationValue: 1,
			},
			Interfaces: []usbdesc.InterfaceConfig{{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 0, BAlternateSetting: 0,
					BNumEndpoints: 2,
				},
				Endpoints: []usbdesc.EndpointDescriptor{
					{BEndpointAddress: 0x01, BMAttributes: 0x03,
						WMaxPacketSize: 64, BInterval: 4},
					{BEndpointAddress: 0x02, BMAttributes: 0x02,
						WMaxPacketSize: 64},
				},
			}},
			Strings: map[uint8]string{0: "\u0409"},
		},
		generation: 1,
	}
}

func (device *transactionalInterruptOutTestDevice) HandleTransfer(
	_ context.Context,
	_ uint32,
	_ uint32,
	out []byte,
) []byte {
	device.mu.Lock()
	defer device.mu.Unlock()
	device.legacyCalls++
	device.legacyPayloads = append(device.legacyPayloads,
		append([]byte(nil), out...))
	return []byte{0xff}
}

func (device *transactionalInterruptOutTestDevice) GetDescriptor() *usbdesc.Descriptor {
	return device.desc
}

func (*transactionalInterruptOutTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (device *transactionalInterruptOutTestDevice) ClaimInterruptOutTransaction(
	request usbdesc.InterruptOutTransactionRequest,
) (usbdesc.InterruptOutTransactionClaim, error) {
	device.mu.Lock()
	defer device.mu.Unlock()
	device.claimCalls++
	if request.Endpoint != 1 || cap(request.Data) != len(request.Data) {
		return usbdesc.InterruptOutTransactionClaim{}, errInterruptOutTestClaim
	}
	if len(request.Data) > 0 && request.Data[0] == 0xfe {
		return usbdesc.InterruptOutTransactionClaim{}, nil
	}
	if device.outstanding {
		return usbdesc.InterruptOutTransactionClaim{}, errInterruptOutTestClaim
	}
	device.nextToken++
	if device.nextToken == 0 {
		device.nextToken++
	}
	result := usbdesc.InterruptOutTransactionAccepted
	if len(request.Data) > 0 && request.Data[0] == 0xfd {
		result = usbdesc.InterruptOutTransactionStall
	}
	claim := usbdesc.InterruptOutTransactionClaim{
		Token: device.nextToken, Generation: device.generation, Result: result,
	}
	device.activeClaim = claim
	device.outstanding = true
	device.admitted = false
	device.pendingPayload = append(device.pendingPayload[:0], request.Data...)

	returned := claim
	if device.returnForgedClaim {
		returned.Token++
	}
	if device.makeReturnedClaimStale {
		device.generation++
	}
	return returned, nil
}

func (device *transactionalInterruptOutTestDevice) AdmitInterruptOutTransaction(
	claim usbdesc.InterruptOutTransactionClaim,
) error {
	device.mu.Lock()
	defer device.mu.Unlock()
	device.admissionCalls++
	if !device.validClaimLocked(claim) || device.admitted {
		return errInterruptOutTestClaim
	}
	if device.forcedAdmissionError != nil {
		return device.forcedAdmissionError
	}
	device.admitted = true
	return nil
}

func (device *transactionalInterruptOutTestDevice) CompleteInterruptOutTransaction(
	claim usbdesc.InterruptOutTransactionClaim,
	outcome usbdesc.InterruptOutTransactionOutcome,
) error {
	device.mu.Lock()
	defer device.mu.Unlock()
	device.completionAttempts++
	if !device.validClaimLocked(claim) ||
		outcome < usbdesc.InterruptOutTransactionDelivered ||
		outcome > usbdesc.InterruptOutTransactionCancelled ||
		(outcome != usbdesc.InterruptOutTransactionCancelled && !device.admitted) {
		return errInterruptOutTestClaim
	}
	device.completionOutcomes = append(device.completionOutcomes, outcome)
	if outcome == usbdesc.InterruptOutTransactionDelivered &&
		claim.Result == usbdesc.InterruptOutTransactionAccepted {
		device.committedPayloads = append(device.committedPayloads,
			append([]byte(nil), device.pendingPayload...))
	}
	device.activeClaim = usbdesc.InterruptOutTransactionClaim{}
	device.outstanding = false
	device.admitted = false
	device.pendingPayload = device.pendingPayload[:0]
	return nil
}

func (device *transactionalInterruptOutTestDevice) validClaimLocked(
	claim usbdesc.InterruptOutTransactionClaim,
) bool {
	return device.outstanding && claim.Valid() && claim.Handled() &&
		claim == device.activeClaim && claim.Generation == device.generation
}

type transactionalInterruptOutSnapshot struct {
	outstanding        bool
	committedPayloads  [][]byte
	claimCalls         int
	admissionCalls     int
	completionAttempts int
	completionOutcomes []usbdesc.InterruptOutTransactionOutcome
	legacyCalls        int
	legacyPayloads     [][]byte
}

func (device *transactionalInterruptOutTestDevice) snapshot() transactionalInterruptOutSnapshot {
	device.mu.Lock()
	defer device.mu.Unlock()
	return transactionalInterruptOutSnapshot{
		outstanding:       device.outstanding,
		committedPayloads: cloneByteSlices(device.committedPayloads),
		claimCalls:        device.claimCalls, admissionCalls: device.admissionCalls,
		completionAttempts: device.completionAttempts,
		completionOutcomes: append([]usbdesc.InterruptOutTransactionOutcome(nil),
			device.completionOutcomes...),
		legacyCalls:    device.legacyCalls,
		legacyPayloads: cloneByteSlices(device.legacyPayloads),
	}
}

func cloneByteSlices(source [][]byte) [][]byte {
	clone := make([][]byte, len(source))
	for index := range source {
		clone[index] = append([]byte(nil), source[index]...)
	}
	return clone
}

type transactionalInterruptOutStream struct {
	client net.Conn
	errors <-chan error
}

func newTransactionalInterruptOutStream(
	t *testing.T,
	device *transactionalInterruptOutTestDevice,
) transactionalInterruptOutStream {
	t.Helper()
	bus := virtualbus.New(248)
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
	return transactionalInterruptOutStream{client: clientConn, errors: errCh}
}

func submitTransactionalInterruptOut(
	t *testing.T,
	connection net.Conn,
	seq, endpoint uint32,
	payload []byte,
) (int32, uint32) {
	t.Helper()
	command := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdSubmitCode, Seqnum: seq,
			Dir: usbip.DirOut, Ep: endpoint,
		},
		TransferBufferLen: uint32(len(payload)),
		NumberOfPackets:   -1,
	}
	require.NoError(t, command.Write(connection))
	if len(payload) != 0 {
		_, err := connection.Write(payload)
		require.NoError(t, err)
	}
	var header [retSubmitHeaderSize]byte
	require.NoError(t, usbip.ReadExactly(connection, header[:]))
	return int32(binary.BigEndian.Uint32(header[20:24])),
		binary.BigEndian.Uint32(header[24:28])
}

func requireInterruptOutSettled(
	t *testing.T,
	device *transactionalInterruptOutTestDevice,
	wantCompletions int,
) transactionalInterruptOutSnapshot {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		snapshot := device.snapshot()
		if snapshot.completionAttempts >= wantCompletions {
			return snapshot
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "interrupt OUT transaction did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTransactionalInterruptOutAcceptedCommitsOnlyAfterDeliveredResponse(
	t *testing.T,
) {
	device := newTransactionalInterruptOutTestDevice()
	stream := newTransactionalInterruptOutStream(t, device)
	payload := []byte{0x09, 0x08, 0x07}

	status, actual := submitTransactionalInterruptOut(
		t, stream.client, 1, 1, payload)
	require.Zero(t, status)
	require.Equal(t, uint32(len(payload)), actual)

	snapshot := requireInterruptOutSettled(t, device, 1)
	require.False(t, snapshot.outstanding)
	require.Equal(t, [][]byte{payload}, snapshot.committedPayloads)
	require.Equal(t, 1, snapshot.claimCalls)
	require.Equal(t, 1, snapshot.admissionCalls)
	require.Equal(t, 1, snapshot.completionAttempts)
	require.Equal(t, []usbdesc.InterruptOutTransactionOutcome{
		usbdesc.InterruptOutTransactionDelivered,
	}, snapshot.completionOutcomes)
	require.Zero(t, snapshot.legacyCalls)
}

func TestTransactionalInterruptOutUnhandledPreservesLegacyPath(t *testing.T) {
	device := newTransactionalInterruptOutTestDevice()
	stream := newTransactionalInterruptOutStream(t, device)
	payload := []byte{0xfe, 0x44}

	status, actual := submitTransactionalInterruptOut(
		t, stream.client, 2, 1, payload)
	require.Zero(t, status)
	require.Equal(t, uint32(len(payload)), actual)

	snapshot := device.snapshot()
	require.Equal(t, 1, snapshot.claimCalls)
	require.Zero(t, snapshot.admissionCalls)
	require.Zero(t, snapshot.completionAttempts)
	require.Equal(t, 1, snapshot.legacyCalls)
	require.Equal(t, [][]byte{payload}, snapshot.legacyPayloads)
}

func TestTransactionalInterruptOutStallNeverCommitsOrFallsBack(t *testing.T) {
	device := newTransactionalInterruptOutTestDevice()
	stream := newTransactionalInterruptOutStream(t, device)

	status, actual := submitTransactionalInterruptOut(
		t, stream.client, 3, 1, []byte{0xfd, 0x01})
	require.Equal(t, int32(errPipe), status)
	require.Zero(t, actual)

	snapshot := requireInterruptOutSettled(t, device, 1)
	require.Empty(t, snapshot.committedPayloads)
	require.Zero(t, snapshot.legacyCalls)
	require.Equal(t, []usbdesc.InterruptOutTransactionOutcome{
		usbdesc.InterruptOutTransactionDelivered,
	}, snapshot.completionOutcomes)
}

func TestTransactionalInterruptOutAdmissionFailureCancelsWithoutResponseOrEffect(
	t *testing.T,
) {
	device := newTransactionalInterruptOutTestDevice()
	device.forcedAdmissionError = errors.New("forced admission failure")
	stream := newTransactionalInterruptOutStream(t, device)
	command := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: 4,
			Dir: usbip.DirOut, Ep: 1},
		TransferBufferLen: 1, NumberOfPackets: -1,
	}
	require.NoError(t, command.Write(stream.client))
	_, err := stream.client.Write([]byte{0x01})
	require.NoError(t, err)

	var header [retSubmitHeaderSize]byte
	err = usbip.ReadExactly(stream.client, header[:])
	require.Error(t, err)
	serverErr := <-stream.errors
	require.ErrorContains(t, serverErr, "forced admission failure")

	snapshot := device.snapshot()
	require.False(t, snapshot.outstanding)
	require.Empty(t, snapshot.committedPayloads)
	require.Zero(t, snapshot.legacyCalls)
	require.Equal(t, []usbdesc.InterruptOutTransactionOutcome{
		usbdesc.InterruptOutTransactionCancelled,
	}, snapshot.completionOutcomes)
}

type alwaysFailWriter struct{ err error }

func (writer alwaysFailWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestTransactionalInterruptOutDeliveryFailureRetiresWithoutEffect(t *testing.T) {
	device := newTransactionalInterruptOutTestDevice()
	writeFailure := errors.New("forced response write failure")
	responses := newResponseWriter(alwaysFailWriter{err: writeFailure}, nil)

	_, handled, err := processTransactionalInterruptOutSubmission(
		responses, device, nil, 5, 1, []byte{0x33, 0x22})
	require.True(t, handled)
	require.ErrorContains(t, err, writeFailure.Error())

	snapshot := device.snapshot()
	require.False(t, snapshot.outstanding)
	require.Empty(t, snapshot.committedPayloads)
	require.Zero(t, snapshot.legacyCalls)
	require.Equal(t, []usbdesc.InterruptOutTransactionOutcome{
		usbdesc.InterruptOutTransactionDeliveryFailed,
	}, snapshot.completionOutcomes)
}

func TestTransactionalInterruptOutIsLimitedToActiveInterruptEndpoint(t *testing.T) {
	device := newTransactionalInterruptOutTestDevice()
	stream := newTransactionalInterruptOutStream(t, device)
	payload := []byte{0x41}

	status, actual := submitTransactionalInterruptOut(
		t, stream.client, 6, 2, payload)
	require.Zero(t, status)
	require.Equal(t, uint32(len(payload)), actual)

	snapshot := device.snapshot()
	require.Zero(t, snapshot.claimCalls)
	require.Equal(t, 1, snapshot.legacyCalls)
	require.Equal(t, [][]byte{payload}, snapshot.legacyPayloads)
}

func TestTransactionalInterruptOutForgedAndStaleClaimsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*transactionalInterruptOutTestDevice)
	}{
		{name: "forged token", mutate: func(device *transactionalInterruptOutTestDevice) {
			device.returnForgedClaim = true
		}},
		{name: "stale generation", mutate: func(device *transactionalInterruptOutTestDevice) {
			device.makeReturnedClaimStale = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			device := newTransactionalInterruptOutTestDevice()
			test.mutate(device)
			responses := newResponseWriter(io.Discard, nil)

			_, handled, err := processTransactionalInterruptOutSubmission(
				responses, device, nil, 7, 1, []byte{0x55})
			require.True(t, handled)
			require.ErrorIs(t, err, errInterruptOutTestClaim)
			snapshot := device.snapshot()
			require.Empty(t, snapshot.committedPayloads)
			require.Zero(t, snapshot.legacyCalls)
			require.Equal(t, 1, snapshot.completionAttempts)
		})
	}
}
