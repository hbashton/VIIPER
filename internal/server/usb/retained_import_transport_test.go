package usb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/internal/retainedusb"
	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

type retainedImportTransportTestDevice struct {
	*schedulerTestDevice
	deviceID uint64
	owner    retainedusb.ImportOwner
}

type blockingRetainedDescriptorDevice struct {
	*retainedImportTransportTestDevice
	calls   atomic.Uint32
	release <-chan struct{}
}

type retainedCapabilityCountDevice struct {
	*blockingRetainedDescriptorDevice
	ownerCalls    atomic.Uint32
	deviceIDCalls atomic.Uint32
}

func (device *retainedCapabilityCountDevice) RetainedUSBImportOwner() retainedusb.ImportOwner {
	device.ownerCalls.Add(1)
	return device.blockingRetainedDescriptorDevice.RetainedUSBImportOwner()
}

func (device *retainedCapabilityCountDevice) RetainedUSBImportDeviceID() uint64 {
	device.deviceIDCalls.Add(1)
	return device.blockingRetainedDescriptorDevice.RetainedUSBImportDeviceID()
}

type retainedCloseResultConn struct {
	net.Conn
	result error
	calls  atomic.Uint32
}

type retainedTestClientConnection struct {
	net.Conn
	wireDeviceID uint32
}

type retainedCloseResultListener struct {
	result error
}

func (*retainedCloseResultListener) Accept() (net.Conn, error) {
	return nil, net.ErrClosed
}

func (listener *retainedCloseResultListener) Close() error {
	return listener.result
}

func (*retainedCloseResultListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

func (conn *retainedTestClientConnection) retainedTestWireDeviceID() uint32 {
	return conn.wireDeviceID
}

func retainedTestWireDeviceID(conn net.Conn) uint32 {
	provider, _ := conn.(interface{ retainedTestWireDeviceID() uint32 })
	if provider == nil {
		return 0
	}
	return provider.retainedTestWireDeviceID()
}

func (conn *retainedCloseResultConn) Close() error {
	conn.calls.Add(1)
	return conn.result
}

func (device *blockingRetainedDescriptorDevice) GetDescriptor() *rootusb.Descriptor {
	device.calls.Add(1)
	<-device.release
	return device.retainedImportTransportTestDevice.GetDescriptor()
}

type retainedImportControlOverlapDevice struct {
	*retainedImportTransportTestDevice
}

func (*retainedImportControlOverlapDevice) ClaimControlTransaction(
	rootusb.ControlTransactionRequest,
) (rootusb.ControlTransactionClaim, error) {
	return rootusb.ControlTransactionClaim{}, nil
}
func (*retainedImportControlOverlapDevice) AdmitControlTransaction(
	rootusb.ControlTransactionClaim, []byte,
) error {
	return nil
}
func (*retainedImportControlOverlapDevice) CompleteControlTransaction(
	rootusb.ControlTransactionClaim, rootusb.ControlTransactionOutcome,
) error {
	return nil
}

type retainedImportInterruptOverlapDevice struct {
	*retainedImportTransportTestDevice
}

func (*retainedImportInterruptOverlapDevice) ClaimInterruptOutTransaction(
	rootusb.InterruptOutTransactionRequest,
) (rootusb.InterruptOutTransactionClaim, error) {
	return rootusb.InterruptOutTransactionClaim{}, nil
}
func (*retainedImportInterruptOverlapDevice) AdmitInterruptOutTransaction(
	rootusb.InterruptOutTransactionClaim,
) error {
	return nil
}
func (*retainedImportInterruptOverlapDevice) CompleteInterruptOutTransaction(
	rootusb.InterruptOutTransactionClaim, rootusb.InterruptOutTransactionOutcome,
) error {
	return nil
}

type retainedImportPreparedOverlapDevice struct {
	*retainedImportTransportTestDevice
}

func (*retainedImportPreparedOverlapDevice) OwnsInputPresentationEndpoint(uint8) bool {
	return true
}
func (*retainedImportPreparedOverlapDevice) InputPresentationGeneration() uint64 {
	return 1
}
func (*retainedImportPreparedOverlapDevice) ResolveInputPresentation(
	inputpresentation.Claim, inputpresentation.Outcome, time.Time,
) bool {
	return true
}
func (*retainedImportPreparedOverlapDevice) RetireInputPresentationGeneration(
	uint64, time.Time,
) bool {
	return true
}
func (*retainedImportPreparedOverlapDevice) SelectInputPresentation(
	int, time.Time,
) (inputpresentation.Claim, bool) {
	return inputpresentation.Claim{}, false
}
func (*retainedImportPreparedOverlapDevice) AdmitAndCopyInputPresentation(
	inputpresentation.Claim, []byte, time.Time,
) bool {
	return false
}

func (device *retainedImportTransportTestDevice) RetainedUSBImportDeviceID() uint64 {
	if device == nil {
		return 0
	}
	return device.deviceID
}

func (device *retainedImportTransportTestDevice) RetainedUSBImportOwner() retainedusb.ImportOwner {
	if device == nil {
		return nil
	}
	return device.owner
}

func newRetainedImportTransportOwner() (
	*scriptedImportSessionOwner,
	*scriptedRetainedOwner,
) {
	hot := newScriptedRetainedOwner(1)
	owner := &scriptedImportSessionOwner{
		scriptedRetainedOwner: hot,
		id:                    hot.identity,
	}
	return owner, hot
}

func startRetainedImportTransportServer(
	t *testing.T,
	busID uint32,
	authorityID uint64,
	owner retainedusb.ImportOwner,
) (*Server, net.Conn, <-chan error) {
	t.Helper()
	return startRetainedImportTransportServerWithConfig(t, busID, owner, ServerConfig{
		ConnectionTimeout:         time.Second,
		RetainedImportAuthorityID: authorityID,
	})
}

func startRetainedImportTransportServerWithConfig(
	t *testing.T,
	busID uint32,
	owner retainedusb.ImportOwner,
	config ServerConfig,
) (*Server, net.Conn, <-chan error) {
	t.Helper()
	device := &retainedImportTransportTestDevice{
		schedulerTestDevice: &schedulerTestDevice{
			desc: retainedImportTransportDescriptor(owner.Limits()),
		},
		deviceID: uint64(busID)<<32 | 1,
		owner:    owner,
	}
	bus := virtualbus.New(busID)
	t.Cleanup(func() { _ = bus.Close() })
	_, err := bus.Add(device)
	require.NoError(t, err)
	server := New(config, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	serverConn, rawClientConn := net.Pipe()
	clientConn := &retainedTestClientConnection{
		Conn: rawClientConn, wireDeviceID: busID<<16 | 1,
	}
	result := make(chan error, 1)
	go func() { result <- server.handleConn(serverConn) }()
	t.Cleanup(func() { _ = clientConn.Close() })
	return server, clientConn, result
}

func retainedImportTransportDescriptor(
	limits retainedusb.Limits,
) *rootusb.Descriptor {
	return &rootusb.Descriptor{
		Device: rootusb.DeviceDescriptor{
			BcdUSB: 0x0200, BDeviceClass: 0xff, BDeviceSubClass: 0x47,
			BDeviceProtocol: 0xd0, BMaxPacketSize0: 64,
			IDVendor: 0xf00d, IDProduct: 0xbeef, BcdDevice: 0x0100,
			BNumConfigurations: 1, Speed: 2,
		},
		Configuration: rootusb.ConfigurationDescriptor{
			BConfigurationValue: 1, BMAttributes: 0x80, BMaxPower: 50,
		},
		Interfaces: []rootusb.InterfaceConfig{{
			Descriptor: rootusb.InterfaceDescriptor{
				BInterfaceNumber: 0, BAlternateSetting: 0, BNumEndpoints: 2,
				BInterfaceClass: 0xff, BInterfaceSubClass: 0x47,
				BInterfaceProtocol: 0xd0,
			},
			Endpoints: []rootusb.EndpointDescriptor{
				{BEndpointAddress: limits.InterruptOutRoute.EndpointAddress, BMAttributes: 0x03,
					WMaxPacketSize: uint16(limits.MaximumInterruptOut), BInterval: 4},
				{BEndpointAddress: limits.InterruptInRoute.EndpointAddress, BMAttributes: 0x03,
					WMaxPacketSize: uint16(limits.MaximumInterruptIn), BInterval: 4},
			},
		}},
	}
}

func TestRetainedImportDescriptorMustExactlyMatchOwnerRoutes(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	limits := owner.Limits()
	valid := retainedImportTransportDescriptor(limits)
	require.NoError(t, validateRetainedImportDescriptor(valid, limits))

	tests := map[string]func(*rootusb.Descriptor){
		"missing-in": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Endpoints[1].BEndpointAddress = 0x82
		},
		"iso-out": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Endpoints[0].BMAttributes = 0x01
		},
		"packet-size": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Endpoints[1].WMaxPacketSize = 32
		},
		"endpoint-count": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Descriptor.BNumEndpoints = 1
		},
		"duplicate-interface-alternate": func(descriptor *rootusb.Descriptor) {
			duplicate := descriptor.Interfaces[0]
			duplicate.Descriptor.BNumEndpoints = 0
			duplicate.Endpoints = nil
			descriptor.Interfaces = append(descriptor.Interfaces, duplicate)
		},
		"duplicate-route-endpoint": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Endpoints = append(
				descriptor.Interfaces[0].Endpoints,
				descriptor.Interfaces[0].Endpoints[1])
			descriptor.Interfaces[0].Descriptor.BNumEndpoints++
		},
		"third-endpoint": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Endpoints = append(
				descriptor.Interfaces[0].Endpoints,
				rootusb.EndpointDescriptor{
					BEndpointAddress: 0x82, BMAttributes: 0x03,
					WMaxPacketSize: 64, BInterval: 4,
				})
			descriptor.Interfaces[0].Descriptor.BNumEndpoints = 3
		},
		"extra-interface": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces = append(
				descriptor.Interfaces, rootusb.InterfaceConfig{
					Descriptor: rootusb.InterfaceDescriptor{
						BInterfaceNumber: 1,
					},
				})
		},
		"nonzero-interface-number": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Descriptor.BInterfaceNumber = 1
		},
		"nonzero-alternate-setting": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Descriptor.BAlternateSetting = 1
		},
		"zero-interval": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Endpoints[0].BInterval = 0
		},
		"interface-association": func(descriptor *rootusb.Descriptor) {
			descriptor.Associations = []rootusb.InterfaceAssociationDescriptor{{
				BFirstInterface: 0, BInterfaceCount: 1,
			}}
		},
		"hid-function": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].HID = &rootusb.HIDFunction{}
		},
		"interface-class-descriptor": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].ClassDescriptors =
				[]rootusb.ClassSpecificDescriptor{{DescriptorType: 0x24}}
		},
		"endpoint-trailing-data": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Endpoints[0].Trailing = rootusb.Data{0x01}
		},
		"endpoint-class-descriptor": func(descriptor *rootusb.Descriptor) {
			descriptor.Interfaces[0].Endpoints[0].ClassDescriptors =
				[]rootusb.ClassSpecificDescriptor{{DescriptorType: 0x25}}
		},
		"zero-configuration-value": func(descriptor *rootusb.Descriptor) {
			descriptor.Configuration.BConfigurationValue = 0
		},
		"multiple-configurations": func(descriptor *rootusb.Descriptor) {
			descriptor.Device.BNumConfigurations = 2
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			descriptor := retainedImportTransportDescriptor(limits)
			mutate(descriptor)
			require.Error(t, validateRetainedImportDescriptor(descriptor, limits))
		})
	}
}

func TestRetainedDescriptorSealIsDeepAndTimedOutDeviceIsRejected(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	source := retainedImportTransportDescriptor(owner.Limits())
	source.Strings = map[uint8]string{1: "owner-served string"}
	source.MicrosoftOS10 = &rootusb.MicrosoftOS10Descriptor{
		DeviceInterfaceGUID: "{owner-served-control-plane}",
	}
	sealed, err := sealRetainedImportDescriptor(source)
	require.NoError(t, err)
	source.Interfaces[0].Endpoints[0].BInterval++
	source.Interfaces[0].Descriptor.BInterfaceClass++
	require.NotEqual(t, source.Interfaces[0].Endpoints[0].BInterval,
		sealed.Interfaces[0].Endpoints[0].BInterval)
	require.NotEqual(t, source.Interfaces[0].Descriptor.BInterfaceClass,
		sealed.Interfaces[0].Descriptor.BInterfaceClass)
	require.Nil(t, sealed.Strings)
	require.Nil(t, sealed.MicrosoftOS10)

	release := make(chan struct{})
	base := &retainedImportTransportTestDevice{
		schedulerTestDevice: &schedulerTestDevice{desc: source},
		deviceID:            1, owner: owner,
	}
	device := &blockingRetainedDescriptorDevice{
		retainedImportTransportTestDevice: base, release: release,
	}
	server := New(ServerConfig{ConnectionTimeout: 20 * time.Millisecond},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	_, firstErr := server.snapshotRetainedDescriptor(device)
	require.ErrorIs(t, firstErr, errRetainedImportCloseTimedOut)
	_, secondErr := server.snapshotRetainedDescriptor(device)
	require.ErrorIs(t, secondErr, errRetainedImportQuarantined)
	require.Equal(t, uint32(1), device.calls.Load())
	close(release)
}

func TestRetainedDescriptorAndOwnerLimitsAreRegistrationLifetimeStable(
	t *testing.T,
) {
	owner, hot := newRetainedImportTransportOwner()
	device := &retainedImportTransportTestDevice{
		schedulerTestDevice: &schedulerTestDevice{
			desc: retainedImportTransportDescriptor(owner.Limits()),
		},
		deviceID: 1, owner: owner,
	}
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	firstDescriptor, err := server.snapshotRetainedDescriptor(device)
	require.NoError(t, err)
	require.NotNil(t, firstDescriptor)
	device.desc.Device.IDProduct++
	_, err = server.snapshotRetainedDescriptor(device)
	require.ErrorIs(t, err, errRetainedImportQuarantined)

	secondDevice := &retainedImportTransportTestDevice{
		schedulerTestDevice: &schedulerTestDevice{
			desc: retainedImportTransportDescriptor(owner.Limits()),
		},
		deviceID: 2, owner: owner,
	}
	first, err := server.beginRetainedDeviceCallbacks(secondDevice)
	require.NoError(t, err)
	require.NoError(t, first.bindCapability(owner, 2))
	require.NoError(t, first.bindLimits(owner.Limits()))
	first.release()
	hot.limits.MaximumInterruptIn--
	second, err := server.beginRetainedDeviceCallbacks(secondDevice)
	require.NoError(t, err)
	require.NoError(t, second.bindCapability(owner, 2))
	require.ErrorIs(t, second.bindLimits(owner.Limits()),
		errRetainedImportQuarantined)
	second.release()
}

func TestRetainedInterruptEndpointReservedAttributeBitsAreRejected(
	t *testing.T,
) {
	owner, _ := newRetainedImportTransportOwner()
	for _, attributes := range []uint8{0x07, 0x83, 0xff} {
		descriptor := retainedImportTransportDescriptor(owner.Limits())
		descriptor.Interfaces[0].Endpoints[0].BMAttributes = attributes
		_, err := sealRetainedImportDescriptor(descriptor)
		require.Error(t, err, "accepted reserved attributes %#02x", attributes)
		require.Error(t, validateRetainedImportDescriptor(
			descriptor, owner.Limits()))
	}
}

func TestRetainedEndpointTopologyObeysAdvertisedUSBSpeed(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	baseLimits := owner.Limits()
	tests := []struct {
		name       string
		speed      uint32
		controlMax uint8
		packet     uint16
		interval   uint8
		valid      bool
	}{
		{"low boundary", 1, 8, 8, 255, true},
		{"low oversized", 1, 8, 9, 1, false},
		{"full boundary", 2, 64, 64, 255, true},
		{"full oversized", 2, 64, 65, 1, false},
		{"full invalid ep0", 2, 12, 64, 1, false},
		{"high boundary", 3, 64, 1024, 16, true},
		{"high oversized", 3, 64, 1025, 1, false},
		{"high interval", 3, 64, 64, 17, false},
		{"super needs companions", 4, 9, 64, 1, false},
		{"undefined speed", 99, 64, 64, 1, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := baseLimits
			limits.MaximumInterruptIn = uint32(test.packet)
			limits.MaximumInterruptOut = uint32(test.packet)
			limits.BufferedRequestBytes =
				limits.MaximumControlOut + limits.MaximumInterruptOut
			descriptor := retainedImportTransportDescriptor(limits)
			descriptor.Device.Speed = test.speed
			descriptor.Device.BMaxPacketSize0 = test.controlMax
			for index := range descriptor.Interfaces[0].Endpoints {
				descriptor.Interfaces[0].Endpoints[index].WMaxPacketSize = test.packet
				descriptor.Interfaces[0].Endpoints[index].BInterval = test.interval
			}
			_, sealErr := sealRetainedImportDescriptor(descriptor)
			validationErr := validateRetainedImportDescriptor(descriptor, limits)
			if test.valid {
				require.NoError(t, sealErr)
				require.NoError(t, validationErr)
			} else {
				require.Error(t, sealErr)
				require.Error(t, validationErr)
			}
		})
	}
}

func TestServerOwnedConnectionCachesJoinedCloseFailureExactlyOnce(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	injected := errors.New("injected raw close failure")
	raw := &retainedCloseResultConn{
		Conn: left, result: errors.Join(net.ErrClosed, injected),
	}
	owned := newServerOwnedConnection(raw)
	first := owned.Close()
	second := owned.Close()
	require.ErrorIs(t, first, injected)
	require.ErrorIs(t, second, injected)
	require.Equal(t, first.Error(), second.Error())
	require.Equal(t, uint32(1), raw.calls.Load())
	require.ErrorIs(t, closeRetainedIngressConn(owned), injected)
	require.Equal(t, uint32(1), raw.calls.Load())
}

func TestRegisteredHandlerAndListenerPreserveJoinedCloseFailureLeaves(
	t *testing.T,
) {
	t.Run("handler", func(t *testing.T) {
		left, right := net.Pipe()
		t.Cleanup(func() {
			_ = left.Close()
			_ = right.Close()
		})
		injected := errors.New("handler close containment failed")
		raw := &retainedCloseResultConn{
			Conn: left, result: errors.Join(net.ErrClosed, injected),
		}
		server := New(ServerConfig{ConnectionTimeout: time.Second},
			slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		result := make(chan error, 1)
		go func() { result <- server.handleConn(raw) }()
		var invalidHeader [headerPeekSize]byte
		_, err := right.Write(invalidHeader[:])
		require.NoError(t, err)
		require.ErrorIs(t, <-result, injected)
		require.Equal(t, uint32(1), raw.calls.Load())
	})

	t.Run("listener", func(t *testing.T) {
		injected := errors.New("listener close containment failed")
		server := New(ServerConfig{},
			slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		server.ln = &retainedCloseResultListener{
			result: errors.Join(net.ErrClosed, injected),
		}
		require.ErrorIs(t, server.Close(), injected)
	})
}

func TestServerCloseJoinsDirectRetainedDescriptorLeaseAndFencesSuccessor(
	t *testing.T,
) {
	owner, _ := newRetainedImportTransportOwner()
	release := make(chan struct{})
	device := &blockingRetainedDescriptorDevice{
		retainedImportTransportTestDevice: &retainedImportTransportTestDevice{
			schedulerTestDevice: &schedulerTestDevice{
				desc: retainedImportTransportDescriptor(owner.Limits()),
			},
			deviceID: 0x9955, owner: owner,
		},
		release: release,
	}
	bus := virtualbus.New(0x9955)
	t.Cleanup(func() { _ = bus.Close() })
	deviceContext, err := bus.Add(device)
	require.NoError(t, err)
	registration, found := bus.GetDeviceRegistration(device, deviceContext)
	require.True(t, found)
	server := New(ServerConfig{ConnectionTimeout: time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	snapshotResult := make(chan error, 1)
	go func() {
		_, snapshotErr := server.SnapshotDeviceDescriptor(registration)
		snapshotResult <- snapshotErr
	}()
	require.Eventually(t, func() bool {
		return device.calls.Load() == 1
	}, time.Second, time.Millisecond)
	closeResult := make(chan error, 1)
	go func() { closeResult <- server.Close() }()
	select {
	case closeErr := <-closeResult:
		t.Fatalf("Server.Close escaped direct retained callback: %v", closeErr)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-snapshotResult)
	require.NoError(t, <-closeResult)
	server.retainedFailureMu.Lock()
	require.Nil(t, server.retainedDeviceAdmissions)
	require.Nil(t, server.retainedOwnerAdmissions)
	server.retainedFailureMu.Unlock()
	_, err = server.SnapshotDeviceDescriptor(registration)
	require.ErrorIs(t, err, net.ErrClosed)
	require.Equal(t, uint32(1), device.calls.Load())
	postCloseBus := virtualbus.New(0x9956)
	t.Cleanup(func() { _ = postCloseBus.Close() })
	require.ErrorIs(t, server.AddBus(postCloseBus), net.ErrClosed)
}

func TestRetainedDescriptorCallbackAdmissionIsSingleFlightAndFailureWins(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	release := make(chan struct{})
	base := &retainedImportTransportTestDevice{
		schedulerTestDevice: &schedulerTestDevice{
			desc: retainedImportTransportDescriptor(owner.Limits()),
		},
		deviceID: 1, owner: owner,
	}
	device := &blockingRetainedDescriptorDevice{
		retainedImportTransportTestDevice: base, release: release,
	}
	server := New(ServerConfig{ConnectionTimeout: 50 * time.Millisecond},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	first := make(chan error, 1)
	go func() {
		_, err := server.snapshotRetainedDescriptor(device)
		first <- err
	}()
	require.Eventually(t, func() bool {
		return device.calls.Load() == 1
	}, time.Second, time.Millisecond)
	_, concurrentErr := server.snapshotRetainedDescriptor(device)
	require.ErrorIs(t, concurrentErr, errRetainedImportBusy)
	require.Equal(t, uint32(1), device.calls.Load())
	require.ErrorIs(t, <-first, errRetainedImportCloseTimedOut)
	_, rejectedErr := server.snapshotRetainedDescriptor(device)
	require.ErrorIs(t, rejectedErr, errRetainedImportQuarantined)
	require.Equal(t, uint32(1), device.calls.Load())
	close(release)
}

func TestRetainedDeviceRemovalWinsAgainstInFlightDescriptorSnapshot(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	release := make(chan struct{})
	base := &retainedImportTransportTestDevice{
		schedulerTestDevice: &schedulerTestDevice{
			desc: retainedImportTransportDescriptor(owner.Limits()),
		},
		deviceID: 1, owner: owner,
	}
	device := &blockingRetainedDescriptorDevice{
		retainedImportTransportTestDevice: base, release: release,
	}
	server := New(ServerConfig{ConnectionTimeout: time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	result := make(chan error, 1)
	go func() {
		_, err := server.snapshotRetainedDescriptor(device)
		result <- err
	}()
	require.Eventually(t, func() bool {
		return device.calls.Load() == 1
	}, time.Second, time.Millisecond)
	server.forgetRetainedDeviceAdmission(device)
	close(release)
	require.ErrorIs(t, <-result, errRetainedImportQuarantined)
	require.Empty(t, server.retainedDeviceAdmissions)
}

func TestStaleRegistrationCannotEnterRetainedDescriptorCallbacks(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	release := make(chan struct{})
	close(release)
	device := &blockingRetainedDescriptorDevice{
		retainedImportTransportTestDevice: &retainedImportTransportTestDevice{
			schedulerTestDevice: &schedulerTestDevice{
				desc: retainedImportTransportDescriptor(owner.Limits()),
			},
			deviceID: 1, owner: owner,
		},
		release: release,
	}
	bus := virtualbus.New(0x9951)
	t.Cleanup(func() { _ = bus.Close() })
	_, err := bus.Add(device)
	require.NoError(t, err)
	stale, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)
	server := New(ServerConfig{BusCleanupTimeout: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	require.NoError(t, server.RemoveDeviceByID(0x9951, "1"))

	_, err = server.snapshotRegisteredRetainedDescriptor(stale)
	require.ErrorIs(t, err, errRetainedImportInvalid)
	require.Zero(t, device.calls.Load(),
		"stale registration entered GetDescriptor after detach")

	_, err = bus.Add(device)
	require.NoError(t, err)
	current, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)
	require.NotEqual(t, stale.RegistrationToken, current.RegistrationToken)
	_, err = server.snapshotRegisteredRetainedDescriptor(stale)
	require.ErrorIs(t, err, errRetainedImportInvalid)
	require.Zero(t, device.calls.Load())
	_, err = server.snapshotRegisteredRetainedDescriptor(current)
	require.NoError(t, err)
	require.Equal(t, uint32(1), device.calls.Load())
}

func TestUnrepresentableRetainedAddressRunsNoDeviceOrOwnerCallback(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	release := make(chan struct{})
	close(release)
	device := &retainedCapabilityCountDevice{
		blockingRetainedDescriptorDevice: &blockingRetainedDescriptorDevice{
			retainedImportTransportTestDevice: &retainedImportTransportTestDevice{
				schedulerTestDevice: &schedulerTestDevice{
					desc: retainedImportTransportDescriptor(owner.Limits()),
				},
				deviceID: 1, owner: owner,
			},
			release: release,
		},
	}
	bus := virtualbus.New(0x10000)
	t.Cleanup(func() { _ = bus.Close() })
	_, err := bus.Add(device)
	require.NoError(t, err)
	server := New(ServerConfig{ConnectionTimeout: time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	result := make(chan error, 1)
	go func() { result <- server.handleConn(serverConn) }()
	writeRetainedImportRequest(t, clientConn, "65536-1")
	var failureReply [8]byte
	require.NoError(t, usbip.ReadExactly(clientConn, failureReply[:]))
	require.NotZero(t, binary.BigEndian.Uint32(failureReply[4:8]))
	require.ErrorIs(t, <-result, errRetainedImportInvalid)
	require.Zero(t, device.calls.Load())
	require.Zero(t, device.ownerCalls.Load())
	require.Zero(t, device.deviceIDCalls.Load())
	require.Empty(t, server.retainedDeviceAdmissions)
	require.Empty(t, server.retainedOwnerAdmissions)

	valid, err := retainedImportWireDeviceID(usbip.ExportMeta{
		BusID: 0xffff, DevID: 0xffff,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(0xffffffff), valid)
	_, err = retainedImportWireDeviceID(usbip.ExportMeta{
		BusID: 1, DevID: 0x10000,
	})
	require.ErrorIs(t, err, errRetainedImportInvalid)
}

func TestFailedDeviceCallbackCannotBeRetriedBySamePointerReRegistration(
	t *testing.T,
) {
	owner, _ := newRetainedImportTransportOwner()
	release := make(chan struct{})
	close(release)
	descriptor := retainedImportTransportDescriptor(owner.Limits())
	descriptor.Interfaces[0].Endpoints[0].BMAttributes = 0x83
	device := &blockingRetainedDescriptorDevice{
		retainedImportTransportTestDevice: &retainedImportTransportTestDevice{
			schedulerTestDevice: &schedulerTestDevice{desc: descriptor},
			deviceID:            1, owner: owner,
		},
		release: release,
	}
	bus := virtualbus.New(0x9957)
	t.Cleanup(func() { _ = bus.Close() })
	_, err := bus.Add(device)
	require.NoError(t, err)
	first, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)
	server := New(ServerConfig{BusCleanupTimeout: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	_, err = server.SnapshotDeviceDescriptor(first)
	require.Error(t, err)
	require.Equal(t, uint32(1), device.calls.Load())
	removed, err := server.RemoveDeviceRegistrationIfPresent(first)
	require.NoError(t, err)
	require.True(t, removed)
	_, err = bus.Add(device)
	require.NoError(t, err)
	second, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)

	_, err = server.SnapshotDeviceDescriptor(second)
	require.ErrorIs(t, err, errRetainedImportQuarantined)
	require.Equal(t, uint32(1), device.calls.Load(),
		"same failed device callback ran after re-registration")
}

func TestExactCleanupCannotRemoveSamePointerReRegistration(t *testing.T) {
	device := &schedulerTestDevice{desc: testCompositeDescriptor()}
	bus := virtualbus.New(0x9953)
	t.Cleanup(func() { _ = bus.Close() })
	_, err := bus.Add(device)
	require.NoError(t, err)
	stale, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)
	server := New(ServerConfig{BusCleanupTimeout: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	removed, err := server.RemoveDeviceRegistrationIfPresent(stale)
	require.NoError(t, err)
	require.True(t, removed)
	_, err = bus.Add(device)
	require.NoError(t, err)
	current, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)
	require.NotEqual(t, stale.RegistrationToken, current.RegistrationToken)

	removed, err = server.RemoveDeviceRegistrationIfPresent(stale)
	require.NoError(t, err)
	require.False(t, removed)
	require.True(t, bus.AuthenticatesRegistration(current))
}

func TestEmptyBusCleanupCannotRemoveReplacementBusWithSameID(t *testing.T) {
	const busID uint32 = 0x9954
	oldBus := virtualbus.New(busID)
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(oldBus))
	staleEmpty := oldBus.GetBusEmptyContext()
	require.NotNil(t, staleEmpty)
	require.NoError(t, server.RemoveBus(busID))

	replacement := virtualbus.New(busID)
	t.Cleanup(func() { _ = replacement.Close() })
	_, err := replacement.Add(&schedulerTestDevice{
		desc: testCompositeDescriptor(),
	})
	require.NoError(t, err)
	require.NoError(t, server.AddBus(replacement))
	removed, err := server.removeEmptyBusIncarnation(
		busID, oldBus, staleEmpty)
	require.NoError(t, err)
	require.False(t, removed)
	require.Same(t, replacement, server.GetBus(busID))
	require.Len(t, replacement.Devices(), 1)
}

func TestRetainedCallbackAdmissionFencesSameOwnerAcrossDeviceWrappers(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	newDevice := func(deviceID uint64) *retainedImportTransportTestDevice {
		return &retainedImportTransportTestDevice{
			schedulerTestDevice: &schedulerTestDevice{
				desc: retainedImportTransportDescriptor(owner.Limits()),
			},
			deviceID: deviceID, owner: owner,
		}
	}
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	firstDevice := newDevice(1)
	first, err := server.beginRetainedDeviceCallbacks(firstDevice)
	require.NoError(t, err)
	require.NoError(t, first.bindCapability(owner, 1))
	require.Same(t, firstDevice, first.admission.reference)
	require.Same(t, owner, first.ownerAdmission.reference)

	second, err := server.beginRetainedDeviceCallbacks(newDevice(2))
	require.NoError(t, err)
	require.ErrorIs(t, second.bindCapability(owner, 1), errRetainedImportBusy)
	second.release()

	failure := errors.New("owner policy callback timed out")
	first.reject(failure)
	first.release()
	third, err := server.beginRetainedDeviceCallbacks(newDevice(3))
	require.NoError(t, err)
	require.ErrorIs(t, third.bindCapability(owner, 1), errRetainedImportQuarantined)
	third.release()
}

func TestRetainedRegistrationCapabilityAndOwnerIdentityCannotDrift(
	t *testing.T,
) {
	ownerA, _ := newRetainedImportTransportOwner()
	ownerB, _ := newRetainedImportTransportOwner()
	device := &retainedImportTransportTestDevice{
		schedulerTestDevice: &schedulerTestDevice{
			desc: retainedImportTransportDescriptor(ownerA.Limits()),
		},
		deviceID: 1, owner: ownerA,
	}
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	first, err := server.beginRetainedDeviceCallbacks(device)
	require.NoError(t, err)
	require.NoError(t, first.bindCapability(ownerA, 1))
	require.NoError(t, first.bindOwnerIdentity(ownerA.Identity()))
	first.release()

	ownerDrift, err := server.beginRetainedDeviceCallbacks(device)
	require.NoError(t, err)
	require.ErrorIs(t, ownerDrift.bindCapability(ownerB, 1),
		errRetainedImportQuarantined)
	ownerDrift.release()

	secondDevice := &retainedImportTransportTestDevice{
		schedulerTestDevice: device.schedulerTestDevice,
		deviceID:            2, owner: ownerA,
	}
	identityLease, err := server.beginRetainedDeviceCallbacks(secondDevice)
	require.NoError(t, err)
	require.NoError(t, identityLease.bindCapability(ownerA, 2))
	require.NoError(t, identityLease.bindOwnerIdentity(ownerA.Identity()))
	identityLease.release()
	ownerA.id++
	identityDrift, err := server.beginRetainedDeviceCallbacks(secondDevice)
	require.NoError(t, err)
	require.NoError(t, identityDrift.bindCapability(ownerA, 2))
	require.ErrorIs(t, identityDrift.bindOwnerIdentity(ownerA.Identity()),
		errRetainedImportQuarantined)
	identityDrift.release()
}

func TestRetainedBusyFirstCapabilitySampleStillLatchesIdentity(t *testing.T) {
	ownerA, _ := newRetainedImportTransportOwner()
	ownerB, _ := newRetainedImportTransportOwner()
	newDevice := func(id uint64) *retainedImportTransportTestDevice {
		return &retainedImportTransportTestDevice{
			schedulerTestDevice: &schedulerTestDevice{
				desc: retainedImportTransportDescriptor(ownerA.Limits()),
			},
			deviceID: id, owner: ownerA,
		}
	}
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	holder, err := server.beginRetainedDeviceCallbacks(newDevice(1))
	require.NoError(t, err)
	require.NoError(t, holder.bindCapability(ownerA, 1))
	targetDevice := newDevice(2)
	target, err := server.beginRetainedDeviceCallbacks(targetDevice)
	require.NoError(t, err)
	require.ErrorIs(t, target.bindCapability(ownerA, 2),
		errRetainedImportBusy)
	target.release()
	holder.release()

	retry, err := server.beginRetainedDeviceCallbacks(targetDevice)
	require.NoError(t, err)
	require.ErrorIs(t, retry.bindCapability(ownerB, 3),
		errRetainedImportQuarantined)
	retry.release()
}

func TestRetainedAuthorityPublishesIdentityBeforeSecondAvailabilityLoss(
	t *testing.T,
) {
	authority, err := newRetainedImportAuthority(0x9958, 0, 0)
	require.NoError(t, err)
	loser, _ := newRetainedImportTransportOwner()
	winner, _ := newRetainedImportTransportOwner()
	loser.identityEntered = make(chan struct{})
	identityRelease := make(chan struct{})
	loser.identityRelease = identityRelease
	observed := make(chan uint64, 1)
	loserResult := make(chan error, 1)
	go func() {
		_, _, reserveErr := authority.reserveWithIdentityObserver(
			1, loser, time.Now().Add(time.Second), func(identity uint64) error {
				observed <- identity
				return nil
			})
		loserResult <- reserveErr
	}()
	select {
	case <-loser.identityEntered:
	case <-time.After(time.Second):
		t.Fatal("losing contender did not enter Identity")
	}
	winnerReservation, _, err := authority.reserve(
		1, winner, time.Now().Add(time.Second))
	require.NoError(t, err)
	close(identityRelease)
	require.Equal(t, loser.id, <-observed)
	require.ErrorIs(t, <-loserResult, errRetainedImportBusy)
	require.NoError(t, authority.abort(winnerReservation))
}

func TestRemovedWrapperCannotEraseSharedOwnerQuarantineForLazySibling(
	t *testing.T,
) {
	owner, _ := newRetainedImportTransportOwner()
	newDevice := func(id uint64) *retainedImportTransportTestDevice {
		return &retainedImportTransportTestDevice{
			schedulerTestDevice: &schedulerTestDevice{
				desc: retainedImportTransportDescriptor(owner.Limits()),
			},
			deviceID: id, owner: owner,
		}
	}
	firstDevice := newDevice(1)
	siblingDevice := newDevice(2)
	bus := virtualbus.New(0x9952)
	t.Cleanup(func() { _ = bus.Close() })
	_, err := bus.Add(firstDevice)
	require.NoError(t, err)
	_, err = bus.Add(siblingDevice)
	require.NoError(t, err)
	firstRegistration, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)
	siblingRegistration, _, found := bus.GetDeviceImportSnapshot(2)
	require.True(t, found)
	server := New(ServerConfig{BusCleanupTimeout: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	first, err := server.beginRetainedRegisteredDeviceCallbacks(firstRegistration)
	require.NoError(t, err)
	require.NoError(t, first.bindCapability(owner, 1))
	first.reject(errors.New("shared owner callback failed"))
	first.release()

	// Sibling is registered and descriptor-admitted, but lazy capability
	// discovery has not yet published its shared owner reference.
	_, err = server.snapshotRegisteredRetainedDescriptor(siblingRegistration)
	require.NoError(t, err)
	require.NoError(t, server.RemoveDeviceByID(0x9952, "1"))
	sibling, err := server.beginRetainedRegisteredDeviceCallbacks(
		siblingRegistration)
	require.NoError(t, err)
	require.ErrorIs(t, sibling.bindCapability(owner, 2),
		errRetainedImportQuarantined)
	sibling.release()
}

func TestRetainedOwnerClaimPublishesAssociationBeforeCleanupCanScan(
	t *testing.T,
) {
	owner, _ := newRetainedImportTransportOwner()
	newDevice := func(deviceID uint64) *retainedImportTransportTestDevice {
		return &retainedImportTransportTestDevice{
			schedulerTestDevice: &schedulerTestDevice{
				desc: retainedImportTransportDescriptor(owner.Limits()),
			},
			deviceID: deviceID, owner: owner,
		}
	}
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	first, err := server.beginRetainedDeviceCallbacks(newDevice(1))
	require.NoError(t, err)
	require.NoError(t, first.bindCapability(owner, 1))
	ownerAdmission := first.ownerAdmission
	first.release()
	first.admission.mu.Lock()
	first.admission.removed = true
	first.admission.mu.Unlock()

	second, err := server.beginRetainedDeviceCallbacks(newDevice(2))
	require.NoError(t, err)
	ownerAdmission.mu.Lock()
	bindDone := make(chan error, 1)
	go func() { bindDone <- second.bindCapability(owner, 1) }()
	require.Eventually(t, func() bool {
		if server.retainedFailureMu.TryLock() {
			server.retainedFailureMu.Unlock()
			return false
		}
		return true
	}, time.Second, time.Millisecond,
		"owner binder never acquired admission-map ownership")
	cleanupDone := make(chan struct{})
	go func() {
		server.cleanupRetainedDeviceAdmission(first.reference)
		close(cleanupDone)
	}()
	select {
	case <-cleanupDone:
		t.Fatal("cleanup crossed an owner claim before atomic publication")
	case <-time.After(20 * time.Millisecond):
	}
	ownerAdmission.mu.Unlock()
	require.NoError(t, <-bindDone)
	<-cleanupDone

	server.retainedFailureMu.Lock()
	require.Same(t, ownerAdmission,
		server.retainedOwnerAdmissions[second.ownerReference])
	server.retainedFailureMu.Unlock()
	second.admission.mu.Lock()
	require.Equal(t, second.ownerReference,
		second.admission.ownerReference)
	second.admission.mu.Unlock()
	ownerAdmission.mu.Lock()
	require.True(t, ownerAdmission.inFlight)
	ownerAdmission.mu.Unlock()
	second.release()
}

func TestRetainedDeviceAdmissionReleasesButOwnerProofSurvivesUntilServerClose(
	t *testing.T,
) {
	owner, _ := newRetainedImportTransportOwner()
	device := &retainedImportTransportTestDevice{
		schedulerTestDevice: &schedulerTestDevice{
			desc: retainedImportTransportDescriptor(owner.Limits()),
		},
		deviceID: 0x9961, owner: owner,
	}
	bus := virtualbus.New(996)
	t.Cleanup(func() { _ = bus.Close() })
	_, err := bus.Add(device)
	require.NoError(t, err)
	server := New(ServerConfig{BusCleanupTimeout: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	registration, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)
	lease, err := server.beginRetainedRegisteredDeviceCallbacks(registration)
	require.NoError(t, err)
	require.NoError(t, lease.bindCapability(owner, 1))
	require.Len(t, server.retainedDeviceAdmissions, 1)
	require.Len(t, server.retainedOwnerAdmissions, 1)

	require.NoError(t, server.RemoveDeviceByID(996, "1"))
	require.Len(t, server.retainedDeviceAdmissions, 1,
		"active admission was deleted before its callback owner released it")
	lease.release()
	require.Empty(t, server.retainedDeviceAdmissions)
	require.Len(t, server.retainedOwnerAdmissions, 1,
		"lazy sibling discovery requires owner proof beyond wrapper removal")
	require.NoError(t, server.Close())
	require.Nil(t, server.retainedOwnerAdmissions)
}

func TestRetainedImportRejectsAmbiguousLegacyContracts(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	base := &retainedImportTransportTestDevice{
		schedulerTestDevice: &schedulerTestDevice{
			desc: retainedImportTransportDescriptor(owner.Limits()),
		},
		deviceID: 1, owner: owner,
	}
	if retainedImportHasAmbiguousContracts(base) {
		t.Fatal("exact retained-only device was rejected")
	}
	for name, device := range map[string]rootusb.Device{
		"control":        &retainedImportControlOverlapDevice{base},
		"interrupt-out":  &retainedImportInterruptOverlapDevice{base},
		"prepared-input": &retainedImportPreparedOverlapDevice{base},
	} {
		t.Run(name, func(t *testing.T) {
			if !retainedImportHasAmbiguousContracts(device) {
				t.Fatal("ambiguous legacy contract was admitted")
			}
			if _, _, retained := retainedImportCapability(device); retained {
				t.Fatal("ambiguous retained capability escaped")
			}
		})
	}
}

func writeRetainedImportRequest(t *testing.T, conn net.Conn, busID string) {
	t.Helper()
	require.NoError(t, (&usbip.MgmtHeader{
		Version: usbip.Version, Command: usbip.OpReqImport,
	}).Write(conn))
	var bus [busIDSize]byte
	copy(bus[:], busID)
	_, err := conn.Write(bus[:])
	require.NoError(t, err)
}

func readSuccessfulRetainedImport(t *testing.T, conn net.Conn) {
	t.Helper()
	var reply [8 + 312]byte
	require.NoError(t, usbip.ReadExactly(conn, reply[:]))
	require.Equal(t, uint16(usbip.Version),
		binary.BigEndian.Uint16(reply[0:2]))
	require.Equal(t, uint16(usbip.OpRepImport),
		binary.BigEndian.Uint16(reply[2:4]))
	require.Zero(t, binary.BigEndian.Uint32(reply[4:8]))
}

func writeRetainedSubmit(
	t *testing.T,
	conn net.Conn,
	sequence, direction, endpoint, length uint32,
	setup [8]byte,
	payload []byte,
) {
	t.Helper()
	command := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdSubmitCode, Seqnum: sequence,
			Devid: retainedTestWireDeviceID(conn), Dir: direction, Ep: endpoint,
		},
		TransferBufferLen: length,
		NumberOfPackets:   -1,
		Setup:             setup,
	}
	require.NoError(t, command.Write(conn))
	if len(payload) != 0 {
		_, err := conn.Write(payload)
		require.NoError(t, err)
	}
}

func readRetainedSubmitResponse(
	t *testing.T,
	conn net.Conn,
) (sequence uint32, status int32, actual uint32, data []byte) {
	return readRetainedSubmitResponseForDirection(t, conn, usbip.DirIn)
}

func readRetainedSubmitResponseForDirection(
	t *testing.T,
	conn net.Conn,
	direction uint32,
) (sequence uint32, status int32, actual uint32, data []byte) {
	t.Helper()
	var header [retSubmitHeaderSize]byte
	require.NoError(t, usbip.ReadExactly(conn, header[:]))
	require.Equal(t, uint32(usbip.RetSubmitCode),
		binary.BigEndian.Uint32(header[0:4]))
	sequence = binary.BigEndian.Uint32(header[4:8])
	status = int32(binary.BigEndian.Uint32(header[20:24]))
	actual = binary.BigEndian.Uint32(header[24:28])
	// RET_SUBMIT actual_length acknowledges consumed host bytes for OUT, but
	// only an IN completion appends a device-to-host response body.
	if actual != 0 && status == 0 && direction == usbip.DirIn {
		data = make([]byte, actual)
		require.NoError(t, usbip.ReadExactly(conn, data))
	}
	return sequence, status, actual, data
}

func TestRetainedImportBindsBeforeSuccessAndRoutesInterruptIn(t *testing.T) {
	owner, hot := newRetainedImportTransportOwner()
	bindEntered := make(chan struct{})
	bindRelease := make(chan struct{})
	owner.bindEntered = bindEntered
	owner.bindRelease = bindRelease
	hot.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result: retainedusb.ResultSuccess,
	})
	hot.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{0x20, 0x69, 0x01},
	})
	_, client, result := startRetainedImportTransportServer(
		t, 940, 0x9401, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "940-1")
	select {
	case <-bindEntered:
	case <-time.After(time.Second):
		t.Fatal("retained owner did not reach BindImport")
	}

	require.NoError(t, client.SetReadDeadline(time.Now().Add(30*time.Millisecond)))
	var early [1]byte
	_, earlyErr := client.Read(early[:])
	var networkErr net.Error
	require.True(t, errors.As(earlyErr, &networkErr) && networkErr.Timeout(),
		"success reply escaped before BindImport: %v", earlyErr)
	close(bindRelease)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	readSuccessfulRetainedImport(t, client)

	writeRetainedSubmit(t, client, 100, usbip.DirIn, 1, 64, [8]byte{}, nil)
	sequence, status, actual, _ := readRetainedSubmitResponse(t, client)
	require.Equal(t, uint32(100), sequence)
	require.Equal(t, int32(errPipe), status)
	require.Zero(t, actual)
	require.Zero(t, hot.stageCalls.Load())

	setConfiguration := [8]byte{usbReqTypeStandardToDevice,
		usbReqSetConfiguration, 1, 0, 0, 0, 0, 0}
	writeRetainedSubmit(
		t, client, 102, usbip.DirOut, 0, 0, setConfiguration, nil)
	sequence, status, actual, _ = readRetainedSubmitResponseForDirection(
		t, client, usbip.DirOut)
	require.Equal(t, uint32(102), sequence)
	require.Zero(t, status)
	require.Zero(t, actual)

	writeRetainedSubmit(t, client, 101, usbip.DirIn, 1, 64, [8]byte{}, nil)
	sequence, status, actual, data := readRetainedSubmitResponse(t, client)
	require.Equal(t, uint32(101), sequence)
	require.Zero(t, status)
	require.Equal(t, uint32(3), actual)
	require.Equal(t, []byte{0x20, 0x69, 0x01}, data)

	require.NoError(t, client.Close())
	select {
	case err := <-result:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("retained import did not close")
	}
	require.Equal(t, uint32(1), owner.bindCalls.Load())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Equal(t, uint32(1), owner.disconnectCalls.Load())
	require.Equal(t, uint32(2), hot.stageCalls.Load())
}

func TestServerCloseJoinsActiveRetainedImport(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	server, client, result := startRetainedImportTransportServer(
		t, 946, 0x9461, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "946-1")
	readSuccessfulRetainedImport(t, client)

	require.NoError(t, server.Close())
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("server close returned without retained connection join")
	}
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Equal(t, uint32(1), owner.disconnectCalls.Load())
}

func TestServerCloseReturnsRetainedTeardownFailure(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	teardownFailure := errors.New("retained local drain failed")
	owner.drainErr = teardownFailure
	server, client, result := startRetainedImportTransportServer(
		t, 950, 0x9501, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "950-1")
	readSuccessfulRetainedImport(t, client)

	require.ErrorIs(t, server.Close(), teardownFailure)
	select {
	case err := <-result:
		require.ErrorIs(t, err, teardownFailure)
	case <-time.After(2 * time.Second):
		t.Fatal("failed retained teardown did not join")
	}
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Zero(t, owner.disconnectCalls.Load())
}

func TestServerCloseFilteringPreservesJoinedRetainedTeardownFailure(t *testing.T) {
	teardownFailure := errors.New("authoritative neutral proof failed")
	protocolFailure := errors.New("scheduler invariant failed")
	terminal := fmt.Errorf("handle import: %w", &retainedImportTerminalError{
		transport: errors.Join(io.EOF, protocolFailure),
		teardown:  teardownFailure,
	})
	failure := retainedServerTerminalFailure(terminal)
	require.ErrorIs(t, failure, teardownFailure)
	require.ErrorIs(t, failure, protocolFailure)
	require.NoError(t, retainedServerTerminalFailure(io.EOF))
	require.NoError(t, retainedServerTerminalFailure(
		fmt.Errorf("read retained command: %w", io.EOF)))
}

func TestServerCloseJoinsPreImportConnectionBeforeOwnerCallbacks(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	server, client, result := startRetainedImportTransportServer(
		t, 947, 0x9471, owner)
	require.Eventually(t, func() bool {
		server.serverLifecycleMu.Lock()
		defer server.serverLifecycleMu.Unlock()
		return len(server.serverConnections) == 1
	}, time.Second, time.Millisecond)
	require.NoError(t, server.Close())
	var closed [1]byte
	_, readErr := client.Read(closed[:])
	require.Error(t, readErr)
	require.Error(t, <-result)
	require.Zero(t, owner.bindCalls.Load())
}

func TestServerCloseJoinsRetainedDevListDescriptorCallback(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	release := make(chan struct{})
	device := &blockingRetainedDescriptorDevice{
		retainedImportTransportTestDevice: &retainedImportTransportTestDevice{
			schedulerTestDevice: &schedulerTestDevice{
				desc: retainedImportTransportDescriptor(owner.Limits()),
			},
			deviceID: 0x9481,
			owner:    owner,
		},
		release: release,
	}
	bus := virtualbus.New(948)
	t.Cleanup(func() { _ = bus.Close() })
	_, err := bus.Add(device)
	require.NoError(t, err)
	server := New(ServerConfig{ConnectionTimeout: time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	handleResult := make(chan error, 1)
	go func() { handleResult <- server.handleConn(serverConn) }()
	request := [headerPeekSize]byte{}
	binary.BigEndian.PutUint16(request[0:2], usbip.Version)
	binary.BigEndian.PutUint16(request[2:4], usbip.OpReqDevlist)
	_, err = clientConn.Write(request[:])
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return device.calls.Load() == 1
	}, time.Second, time.Millisecond)

	closeResult := make(chan error, 1)
	go func() { closeResult <- server.Close() }()
	select {
	case closeErr := <-closeResult:
		t.Fatalf("Close escaped retained DEVLIST callback: %v", closeErr)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-closeResult)
	require.Error(t, <-handleResult)
	server.retainedFailureMu.Lock()
	require.Nil(t, server.retainedDeviceAdmissions)
	require.Nil(t, server.retainedOwnerAdmissions)
	server.retainedFailureMu.Unlock()
}

func TestRetainedImportBindRejectionWritesFailureBeforeClosingIngress(
	t *testing.T,
) {
	owner, _ := newRetainedImportTransportOwner()
	owner.bindState = retainedusb.ImportBindRejected
	_, client, result := startRetainedImportTransportServer(
		t, 944, 0x9441, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "944-1")
	var reply [8]byte
	require.NoError(t, usbip.ReadExactly(client, reply[:]))
	require.Equal(t, uint16(usbip.Version),
		binary.BigEndian.Uint16(reply[0:2]))
	require.Equal(t, uint16(usbip.OpRepImport),
		binary.BigEndian.Uint16(reply[2:4]))
	require.NotZero(t, binary.BigEndian.Uint32(reply[4:8]))
	require.ErrorIs(t, <-result, errRetainedImportBindRejected)
}

func TestRetainedImportUnsupportedEndpointStallsWithoutOwnerDispatch(t *testing.T) {
	owner, hot := newRetainedImportTransportOwner()
	_, client, result := startRetainedImportTransportServer(
		t, 941, 0x9411, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "941-1")
	readSuccessfulRetainedImport(t, client)

	writeRetainedSubmit(t, client, 201, usbip.DirIn, 2, 64, [8]byte{}, nil)
	sequence, status, actual, _ := readRetainedSubmitResponse(t, client)
	require.Equal(t, uint32(201), sequence)
	require.Equal(t, int32(errPipe), status)
	require.Zero(t, actual)
	require.Zero(t, hot.stageCalls.Load())

	require.NoError(t, client.Close())
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("retained import did not close")
	}
}

func TestRetainedImportRejectedSubmissionCannotDuplicateLiveSequence(t *testing.T) {
	owner, hot := newRetainedImportTransportOwner()
	hot.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result: retainedusb.ResultSuccess,
	})
	hot.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	_, client, result := startRetainedImportTransportServer(
		t, 945, 0x9451, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "945-1")
	readSuccessfulRetainedImport(t, client)
	setConfiguration := [8]byte{usbReqTypeStandardToDevice,
		usbReqSetConfiguration, 1, 0, 0, 0, 0, 0}
	writeRetainedSubmit(
		t, client, 206, usbip.DirOut, 0, 0, setConfiguration, nil)
	readRetainedSubmitResponseForDirection(t, client, usbip.DirOut)

	writeRetainedSubmit(t, client, 207, usbip.DirIn, 1, 64, [8]byte{}, nil)
	require.Eventually(t, func() bool {
		return hot.prepareCalls.Load() != 0
	}, time.Second, time.Millisecond)
	writeRetainedSubmit(t, client, 207, usbip.DirIn, 2, 64, [8]byte{}, nil)

	var responseByte [1]byte
	_, readErr := client.Read(responseByte[:])
	require.Error(t, readErr, "duplicate rejected submission emitted a second RET_SUBMIT")
	select {
	case err := <-result:
		require.ErrorIs(t, err, errRetainedSubmissionDuplicateSequence)
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate retained sequence did not close the import")
	}
}

func TestRetainedDuplicateSequenceIsRejectedAtHeaderBeforeOUTBody(
	t *testing.T,
) {
	owner, hot := newRetainedImportTransportOwner()
	hot.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result: retainedusb.ResultSuccess,
	})
	hot.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	_, client, result := startRetainedImportTransportServer(
		t, 953, 0x9531, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "953-1")
	readSuccessfulRetainedImport(t, client)
	setConfiguration := [8]byte{usbReqTypeStandardToDevice,
		usbReqSetConfiguration, 1, 0, 0, 0, 0, 0}
	writeRetainedSubmit(
		t, client, 501, usbip.DirOut, 0, 0, setConfiguration, nil)
	readRetainedSubmitResponseForDirection(t, client, usbip.DirOut)
	writeRetainedSubmit(t, client, 502, usbip.DirIn, 1, 64, [8]byte{}, nil)
	require.Eventually(t, func() bool {
		return hot.prepareCalls.Load() != 0
	}, time.Second, time.Millisecond)

	// Declare an OUT body but send only the header. Sequence ownership must be
	// decided now; waiting for the body would reopen the predecessor-removal
	// race and could permit a second RET_SUBMIT for 502.
	duplicateHeader := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdSubmitCode, Seqnum: 502,
			Devid: retainedTestWireDeviceID(client), Dir: usbip.DirOut, Ep: 1,
		},
		TransferBufferLen: 8,
		NumberOfPackets:   -1,
	}
	require.NoError(t, duplicateHeader.Write(client))
	var responseByte [1]byte
	_, readErr := client.Read(responseByte[:])
	require.Error(t, readErr,
		"duplicate header waited for an OUT body or emitted a response")
	select {
	case err := <-result:
		require.ErrorIs(t, err, errRetainedSubmissionDuplicateSequence)
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate header did not terminate retained import")
	}
}

func TestRetainedWrongWireDeviceIsRejectedBeforeBodyOrOwnerDispatch(
	t *testing.T,
) {
	tests := []struct {
		name   string
		busID  uint32
		devid  func(uint32) uint32
		unlink bool
	}{
		{"wrong bus submit", 954, func(want uint32) uint32 {
			return want ^ (uint32(1) << 16)
		}, false},
		{"wrong device submit", 955, func(want uint32) uint32 {
			return want ^ 1
		}, false},
		{"wrong device unlink", 956, func(want uint32) uint32 {
			return want ^ 1
		}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owner, hot := newRetainedImportTransportOwner()
			_, client, result := startRetainedImportTransportServer(
				t, test.busID, uint64(test.busID)<<16|1, owner)
			require.NoError(t,
				client.SetDeadline(time.Now().Add(2*time.Second)))
			writeRetainedImportRequest(
				t, client, fmt.Sprintf("%d-1", test.busID))
			readSuccessfulRetainedImport(t, client)
			wrong := test.devid(retainedTestWireDeviceID(client))
			if test.unlink {
				command := usbip.CmdUnlink{Basic: usbip.HeaderBasic{
					Command: usbip.CmdUnlinkCode, Seqnum: 601,
					Devid: wrong, Dir: usbip.DirIn, Ep: 1,
				}, UnlinkSeqnum: 99}
				require.NoError(t, command.Write(client))
			} else {
				// Header only despite a declared OUT body: device identity must
				// terminate framing before the reader waits for those bytes.
				command := usbip.CmdSubmit{Basic: usbip.HeaderBasic{
					Command: usbip.CmdSubmitCode, Seqnum: 601,
					Devid: wrong, Dir: usbip.DirOut, Ep: 1,
				}, TransferBufferLen: 8, NumberOfPackets: -1}
				require.NoError(t, command.Write(client))
			}
			var response [1]byte
			_, readErr := client.Read(response[:])
			require.Error(t, readErr)
			select {
			case err := <-result:
				require.ErrorContains(t, err, "does not match import")
			case <-time.After(2 * time.Second):
				t.Fatal("wrong wire device did not terminate import")
			}
			require.Zero(t, hot.stageCalls.Load())
		})
	}
}

func TestRetainedUnlinkCommandCannotDuplicateLiveSubmitSequence(t *testing.T) {
	owner, hot := newRetainedImportTransportOwner()
	hot.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result: retainedusb.ResultSuccess,
	})
	hot.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	_, client, result := startRetainedImportTransportServer(
		t, 949, 0x9491, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "949-1")
	readSuccessfulRetainedImport(t, client)
	setConfiguration := [8]byte{usbReqTypeStandardToDevice,
		usbReqSetConfiguration, 1, 0, 0, 0, 0, 0}
	writeRetainedSubmit(
		t, client, 210, usbip.DirOut, 0, 0, setConfiguration, nil)
	readRetainedSubmitResponseForDirection(t, client, usbip.DirOut)
	writeRetainedSubmit(t, client, 211, usbip.DirIn, 1, 64, [8]byte{}, nil)
	require.Eventually(t, func() bool {
		return hot.prepareCalls.Load() >= 2
	}, time.Second, time.Millisecond)

	unlink := usbip.CmdUnlink{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdUnlinkCode, Seqnum: 211,
			Devid: retainedTestWireDeviceID(client), Dir: usbip.DirIn, Ep: 1,
		},
		UnlinkSeqnum: 999,
	}
	require.NoError(t, unlink.Write(client))
	var responseByte [1]byte
	_, readErr := client.Read(responseByte[:])
	require.Error(t, readErr, "duplicate unlink emitted an ambiguous response")
	select {
	case err := <-result:
		require.ErrorIs(t, err, errRetainedSubmissionDuplicateSequence)
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate unlink sequence did not close the import")
	}
}

func TestRetainedLifecycleReplyFencesPostReplyInterruptSubmission(t *testing.T) {
	owner, hot := newRetainedImportTransportOwner()
	completeEntered := make(chan struct{})
	completeRelease := make(chan struct{})
	hot.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result:          retainedusb.ResultSuccess,
		completeEntered: completeEntered,
		completeRelease: completeRelease,
	})
	hot.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{0x20, 0x69, 0x02},
	})
	_, client, result := startRetainedImportTransportServer(
		t, 948, 0x9481, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "948-1")
	readSuccessfulRetainedImport(t, client)

	setConfiguration := [8]byte{usbReqTypeStandardToDevice,
		usbReqSetConfiguration, 1, 0, 0, 0, 0, 0}
	writeRetainedSubmit(
		t, client, 301, usbip.DirOut, 0, 0, setConfiguration, nil)
	sequence, status, actual, _ := readRetainedSubmitResponseForDirection(
		t, client, usbip.DirOut)
	require.Equal(t, uint32(301), sequence)
	require.Zero(t, status)
	require.Zero(t, actual)
	select {
	case <-completeEntered:
	case <-time.After(time.Second):
		t.Fatal("lifecycle completion did not enter")
	}

	writeRetainedSubmit(t, client, 302, usbip.DirIn, 1, 64, [8]byte{}, nil)
	close(completeRelease)
	sequence, status, actual, data := readRetainedSubmitResponse(t, client)
	require.Equal(t, uint32(302), sequence)
	require.Zero(t, status)
	require.Equal(t, uint32(3), actual)
	require.Equal(t, []byte{0x20, 0x69, 0x02}, data)

	hot.mu.Lock()
	require.Len(t, hot.requests, 2)
	require.Less(t,
		hot.requests[0].request.BindingGeneration,
		hot.requests[1].request.BindingGeneration)
	hot.mu.Unlock()
	require.NoError(t, client.Close())
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("retained lifecycle barrier import did not close")
	}
}

func TestRetainedPipelinedLifecycleResolvesAgainstDeliveredPredecessorState(t *testing.T) {
	owner, hot := newRetainedImportTransportOwner()
	completeEntered := make(chan struct{})
	completeRelease := make(chan struct{})
	hot.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result:          retainedusb.ResultSuccess,
		completeEntered: completeEntered,
		completeRelease: completeRelease,
	})
	hot.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result: retainedusb.ResultSuccess,
	})
	hot.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{0x44},
	})
	_, client, result := startRetainedImportTransportServer(
		t, 949, 0x9491, owner)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "949-1")
	readSuccessfulRetainedImport(t, client)

	setConfiguration := [8]byte{usbReqTypeStandardToDevice,
		usbReqSetConfiguration, 1, 0, 0, 0, 0, 0}
	writeRetainedSubmit(
		t, client, 401, usbip.DirOut, 0, 0, setConfiguration, nil)
	sequence, status, _, _ := readRetainedSubmitResponseForDirection(
		t, client, usbip.DirOut)
	require.Equal(t, uint32(401), sequence)
	require.Zero(t, status)
	select {
	case <-completeEntered:
	case <-time.After(time.Second):
		t.Fatal("configuration completion did not enter")
	}

	clearHalt := [8]byte{usbReqTypeStandardToEndpoint,
		usbReqClearFeature, 0, 0, 0x81, 0, 0, 0}
	writeRetainedSubmit(t, client, 402, usbip.DirOut, 0, 0, clearHalt, nil)
	close(completeRelease)
	sequence, status, _, _ = readRetainedSubmitResponseForDirection(
		t, client, usbip.DirOut)
	require.Equal(t, uint32(402), sequence)
	require.Zero(t, status)

	writeRetainedSubmit(t, client, 403, usbip.DirIn, 1, 64, [8]byte{}, nil)
	sequence, status, actual, data := readRetainedSubmitResponse(t, client)
	require.Equal(t, uint32(403), sequence)
	require.Zero(t, status)
	require.Equal(t, uint32(1), actual)
	require.Equal(t, []byte{0x44}, data)

	hot.mu.Lock()
	require.Len(t, hot.requests, 3)
	firstGeneration := hot.requests[0].request.BindingGeneration
	secondGeneration := hot.requests[1].request.BindingGeneration
	thirdGeneration := hot.requests[2].request.BindingGeneration
	hot.mu.Unlock()
	require.Equal(t, firstGeneration+1, secondGeneration,
		"dependent CLEAR_FEATURE did not resolve after SET_CONFIGURATION")
	require.Equal(t, secondGeneration+1, thirdGeneration,
		"delivered CLEAR_FEATURE did not rotate the route generation")

	require.NoError(t, client.Close())
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("pipelined retained lifecycle import did not close")
	}
}

func TestZeroAuthorityRejectsRetainedCapableDeviceWithoutOwnerCallbacks(t *testing.T) {
	owner, _ := newRetainedImportTransportOwner()
	server, client, result := startRetainedImportTransportServer(
		t, 942, 0, owner)
	require.Nil(t, server.retainedImports)
	require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
	writeRetainedImportRequest(t, client, "942-1")
	var failure [8]byte
	require.NoError(t, usbip.ReadExactly(client, failure[:]))
	require.Equal(t, uint16(usbip.OpRepImport),
		binary.BigEndian.Uint16(failure[2:4]))
	require.NotZero(t, binary.BigEndian.Uint32(failure[4:8]))
	require.Zero(t, owner.bindCalls.Load())

	select {
	case err := <-result:
		require.ErrorContains(t, err, "requires an explicit authority")
	case <-time.After(2 * time.Second):
		t.Fatal("rejected retained import did not close")
	}
	require.Zero(t, owner.drainCalls.Load())
	require.Zero(t, owner.disconnectCalls.Load())
}
