package usb

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	usbdesc "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type altSettingTestDevice struct {
	desc          *usbdesc.Descriptor
	altEvents     [][2]uint8
	transferCalls int
}

type controlLifecycleTestDevice struct {
	*altSettingTestDevice
	controlCalls   int
	resetEndpoints []uint8
}

type immediateInterruptInTestDevice struct {
	desc *usbdesc.Descriptor
}

type activeEndpointTestDevice struct {
	desc          *usbdesc.Descriptor
	transferCalls atomic.Uint32
}

func (d *activeEndpointTestDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	d.transferCalls.Add(1)
	return []byte{0x5a}
}

func (d *activeEndpointTestDevice) GetDescriptor() *usbdesc.Descriptor {
	return d.desc
}

func (d *activeEndpointTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (d *immediateInterruptInTestDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return []byte{0x5a}
}

func (d *immediateInterruptInTestDevice) GetDescriptor() *usbdesc.Descriptor {
	return d.desc
}

func (d *immediateInterruptInTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (d *controlLifecycleTestDevice) HandleControl(
	uint8, uint8, uint16, uint16, uint16, []byte,
) ([]byte, bool) {
	d.controlCalls++
	return nil, false
}

func (d *controlLifecycleTestDevice) ResetEndpoint(endpointAddress uint8) {
	d.resetEndpoints = append(d.resetEndpoints, endpointAddress)
}

func (d *altSettingTestDevice) HandleTransfer(context.Context, uint32, uint32, []byte) []byte {
	d.transferCalls++
	return nil
}

func (d *altSettingTestDevice) GetDescriptor() *usbdesc.Descriptor {
	return d.desc
}

func (d *altSettingTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (d *altSettingTestDevice) SetInterfaceAltSetting(iface, alt uint8) {
	d.altEvents = append(d.altEvents, [2]uint8{iface, alt})
}

func TestBuildConfigDescriptorSupportsIADAndAlternateSettings(t *testing.T) {
	desc := &usbdesc.Descriptor{
		Configuration: usbdesc.ConfigurationDescriptor{
			BConfigurationValue: 0x01,
			IConfiguration:      0x04,
			BMAttributes:        0xC0,
			BMaxPower:           0xFA,
		},
		Associations: []usbdesc.InterfaceAssociationDescriptor{
			{BFirstInterface: 0, BInterfaceCount: 1, BFunctionClass: 0x03},
			{BFirstInterface: 1, BInterfaceCount: 2, BFunctionClass: 0x01, BFunctionSubClass: 0x01},
		},
		Interfaces: []usbdesc.InterfaceConfig{
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 0, BNumEndpoints: 1, BInterfaceClass: 0x03,
				},
				Endpoints: []usbdesc.EndpointDescriptor{{BEndpointAddress: 0x81, BMAttributes: 0x03, WMaxPacketSize: 64, BInterval: 4}},
			},
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 1, BAlternateSetting: 0, BInterfaceClass: 0x01, BInterfaceSubClass: 0x02,
				},
			},
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 1, BAlternateSetting: 1, BNumEndpoints: 1, BInterfaceClass: 0x01, BInterfaceSubClass: 0x02,
				},
				Endpoints: []usbdesc.EndpointDescriptor{{
					BEndpointAddress: 0x03,
					BMAttributes:     0x0D,
					WMaxPacketSize:   0x00C0,
					BInterval:        1,
					Trailing:         usbdesc.Data{0x00, 0x00},
					ClassDescriptors: []usbdesc.ClassSpecificDescriptor{
						{DescriptorType: 0x25, Payload: usbdesc.Data{0x01, 0x00, 0x00, 0x00, 0x00}},
					},
				}},
			},
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 2, BAlternateSetting: 0, BInterfaceClass: 0x01, BInterfaceSubClass: 0x02,
				},
			},
		},
	}

	got := (&Server{}).buildConfigDescriptor(desc)
	require.NotEmpty(t, got)
	assert.Equal(t, uint16(len(got)), binary.LittleEndian.Uint16(got[2:4]))
	assert.Equal(t, byte(3), got[4])
	assert.Equal(t, byte(0x01), got[5])
	assert.Equal(t, byte(0x04), got[6])
	assert.Equal(t, byte(0xC0), got[7])
	assert.Equal(t, byte(0xFA), got[8])

	assert.Equal(t, []byte{0x08, 0x0B, 0x00, 0x01, 0x03, 0x00, 0x00, 0x00}, got[9:17])
	assert.True(t, bytes.Contains(got, []byte{0x09, 0x05, 0x03, 0x0D, 0xC0, 0x00, 0x01, 0x00, 0x00}))
	assert.True(t, bytes.Contains(got, []byte{0x07, 0x25, 0x01, 0x00, 0x00, 0x00, 0x00}))
}

func TestProcessSubmitTracksInterfaceAlternateSetting(t *testing.T) {
	desc := &usbdesc.Descriptor{
		Interfaces: []usbdesc.InterfaceConfig{
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 2, BAlternateSetting: 0, BInterfaceClass: 0x01, BInterfaceSubClass: 0x02,
				},
			},
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 2, BAlternateSetting: 1, BNumEndpoints: 1, BInterfaceClass: 0x01, BInterfaceSubClass: 0x02,
				},
			},
		},
	}
	dev := &altSettingTestDevice{desc: desc}
	server := New(ServerConfig{}, nil, nil)

	getAlt := []byte{0x81, usbReqGetInterface, 0x00, 0x00, 0x02, 0x00, 0x01, 0x00}
	setAltOne := []byte{0x01, usbReqSetInterface, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00}
	setConfig := []byte{0x00, usbReqSetConfiguration, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00}

	assert.Equal(t, []byte{0x00}, server.processSubmit(context.Background(), dev, 0, 0, getAlt, nil))
	server.processSubmit(context.Background(), dev, 0, 0, setAltOne, nil)
	assert.Equal(t, []byte{0x01}, server.processSubmit(context.Background(), dev, 0, 0, getAlt, nil))
	assert.Equal(t, [][2]uint8{{2, 1}}, dev.altEvents)
	server.processSubmit(context.Background(), dev, 0, 0, setConfig, nil)
	assert.Equal(t, []byte{0x00}, server.processSubmit(context.Background(), dev, 0, 0, getAlt, nil))
	assert.Equal(t, [][2]uint8{{2, 1}, {2, 0}}, dev.altEvents)
}

func TestProcessSubmitTracksDescriptorConfigurationValue(t *testing.T) {
	desc := &usbdesc.Descriptor{
		Configuration: usbdesc.ConfigurationDescriptor{
			BConfigurationValue: 4,
		},
		Interfaces: []usbdesc.InterfaceConfig{{
			Descriptor: usbdesc.InterfaceDescriptor{
				BInterfaceNumber: 0,
			},
			Endpoints: []usbdesc.EndpointDescriptor{{
				BEndpointAddress: 0x81,
				BMAttributes:     0x03,
			}},
		}},
	}
	dev := &altSettingTestDevice{desc: desc}
	server := New(ServerConfig{}, nil, nil)
	getConfiguration := []byte{
		usbReqTypeStandardFromDevice, usbReqGetConfiguration,
		0, 0, 0, 0, 1, 0,
	}
	setConfigurationZero := []byte{
		usbReqTypeStandardToDevice, usbReqSetConfiguration,
		0, 0, 0, 0, 0, 0,
	}
	setConfigurationFour := []byte{
		usbReqTypeStandardToDevice, usbReqSetConfiguration,
		4, 0, 0, 0, 0, 0,
	}
	clearEndpointHalt := []byte{
		usbReqTypeStandardToEndpoint, usbReqClearFeature,
		0, 0, 0x81, 0, 0, 0,
	}

	assert.Equal(t, []byte{4}, server.processSubmit(
		context.Background(), dev, 0, 0, getConfiguration, nil))
	require.True(t, server.parseControlLifecycleSetup(
		dev, clearEndpointHalt).accepted)
	server.processSubmit(context.Background(), dev, 0, 0,
		setConfigurationZero, nil)
	assert.Equal(t, []byte{0}, server.processSubmit(
		context.Background(), dev, 0, 0, getConfiguration, nil))
	require.False(t, server.parseControlLifecycleSetup(
		dev, clearEndpointHalt).accepted,
		"an unconfigured device exposed an active endpoint")
	server.processSubmit(context.Background(), dev, 0, 0,
		setConfigurationFour, nil)
	assert.Equal(t, []byte{4}, server.processSubmit(
		context.Background(), dev, 0, 0, getConfiguration, nil))
	require.True(t, server.parseControlLifecycleSetup(
		dev, clearEndpointHalt).accepted)
}

func TestManagementRepliesUseDescriptorConfigurationValue(t *testing.T) {
	desc := &usbdesc.Descriptor{
		Device: usbdesc.DeviceDescriptor{
			BNumConfigurations: 1,
		},
		Configuration: usbdesc.ConfigurationDescriptor{
			BConfigurationValue: 4,
		},
		Interfaces: []usbdesc.InterfaceConfig{{
			Descriptor: usbdesc.InterfaceDescriptor{
				BInterfaceNumber: 0,
			},
		}},
	}
	dev := &altSettingTestDevice{desc: desc}
	bus := virtualbus.New(253)
	defer bus.Close() //nolint:errcheck
	_, err := bus.Add(dev)
	require.NoError(t, err)
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))

	t.Run("device list", func(t *testing.T) {
		serverConn, clientConn := net.Pipe()
		defer serverConn.Close() //nolint:errcheck
		defer clientConn.Close() //nolint:errcheck
		require.NoError(t,
			clientConn.SetDeadline(time.Now().Add(2*time.Second)))
		errCh := make(chan error, 1)
		go func() { errCh <- server.handleDevList(serverConn) }()
		// Management header (8), device-count header (4), exported-device
		// fixed record (312), and one interface tuple (4).
		var response [328]byte
		require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
		assert.Equal(t, byte(4), response[12+309])
		require.NoError(t, <-errCh)
	})

	t.Run("import", func(t *testing.T) {
		serverConn, clientConn := net.Pipe()
		defer serverConn.Close() //nolint:errcheck
		defer clientConn.Close() //nolint:errcheck
		require.NoError(t,
			clientConn.SetDeadline(time.Now().Add(2*time.Second)))
		type importResult struct {
			release func()
			err     error
		}
		resultCh := make(chan importResult, 1)
		go func() {
			_, release, err := server.handleImport(serverConn)
			resultCh <- importResult{release: release, err: err}
		}()
		var request [busIDSize]byte
		copy(request[:], "253-1")
		_, err = clientConn.Write(request[:])
		require.NoError(t, err)
		var response [8 + 312]byte
		require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
		assert.Equal(t, byte(4), response[8+309])
		result := <-resultCh
		require.NoError(t, result.err)
		require.NotNil(t, result.release)
		result.release()
	})
}

func TestUrbStreamStallsRejectedLifecycleRequest(t *testing.T) {
	dev := newResetPresentationTestDevice(t)
	bus := virtualbus.New(252)
	defer bus.Close() //nolint:errcheck
	_, err := bus.Add(dev)
	require.NoError(t, err)
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close() //nolint:errcheck
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(2*time.Second)))
	errCh := make(chan error, 1)
	go func() { errCh <- server.handleUrbStream(serverConn, dev) }()

	cmd := usbip.CmdSubmit{
		Basic: usbip.HeaderBasic{
			Command: usbip.CmdSubmitCode,
			Seqnum:  91,
			Dir:     usbip.DirOut,
			Ep:      0,
		},
		NumberOfPackets: -1,
		Setup: [8]byte{
			usbReqTypeStandardToDevice, usbReqSetConfiguration,
			2, 0, 0, 0, 0, 0,
		},
	}
	require.NoError(t, cmd.Write(clientConn))
	var response [retSubmitHeaderSize]byte
	require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
	assert.Equal(t, uint32(usbip.RetSubmitCode),
		binary.BigEndian.Uint32(response[0:4]))
	assert.Equal(t, uint32(91), binary.BigEndian.Uint32(response[4:8]))
	assert.Equal(t, int32(errPipe),
		int32(binary.BigEndian.Uint32(response[20:24])))
	assert.Zero(t, binary.BigEndian.Uint32(response[24:28]))

	require.NoError(t, clientConn.Close())
	require.Error(t, <-errCh)
}

func TestUrbStreamVersionedGetReportEnforcesIDAndHIDInterfaceOwnership(
	t *testing.T,
) {
	base := newVersionedInputTestDevice()
	dev := &versionedInputIDTestDevice{versionedInputTestDevice: base}
	bus := virtualbus.New(251)
	defer bus.Close() //nolint:errcheck
	_, err := bus.Add(dev)
	require.NoError(t, err)
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close() //nolint:errcheck
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(2*time.Second)))
	errCh := make(chan error, 1)
	go func() { errCh <- server.handleUrbStream(serverConn, dev) }()

	submit := func(seq uint32, reportID uint8, wIndex uint16) (
		int32, uint32, []byte,
	) {
		t.Helper()
		var setup [8]byte
		setup[0] = hidReqTypeIn
		setup[1] = hidReqGetReport
		binary.LittleEndian.PutUint16(setup[2:4],
			uint16(0x01)<<8|uint16(reportID))
		binary.LittleEndian.PutUint16(setup[4:6], wIndex)
		binary.LittleEndian.PutUint16(setup[6:8], 64)
		command := usbip.CmdSubmit{
			Basic: usbip.HeaderBasic{
				Command: usbip.CmdSubmitCode, Seqnum: seq,
				Dir: usbip.DirIn, Ep: 0,
			},
			TransferBufferLen: 64,
			NumberOfPackets:   -1,
			Setup:             setup,
		}
		require.NoError(t, command.Write(clientConn))
		var response [retSubmitHeaderSize]byte
		require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
		require.Equal(t, uint32(usbip.RetSubmitCode),
			binary.BigEndian.Uint32(response[0:4]))
		require.Equal(t, seq, binary.BigEndian.Uint32(response[4:8]))
		status := int32(binary.BigEndian.Uint32(response[20:24]))
		actual := binary.BigEndian.Uint32(response[24:28])
		payload := make([]byte, actual)
		if actual != 0 {
			require.NoError(t, usbip.ReadExactly(clientConn, payload))
		}
		return status, actual, payload
	}

	for _, test := range []struct {
		name     string
		reportID uint8
		wIndex   uint16
	}{
		{name: "unsupported ID", reportID: 0x01, wIndex: 0x0000},
		{name: "vendor interface", reportID: 0x05, wIndex: 0x0001},
		{name: "high-byte interface alias", reportID: 0x05, wIndex: 0x0100},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, actual, payload := submit(
				100+uint32(len(test.name)), test.reportID, test.wIndex)
			require.Equal(t, int32(errPipe), status)
			require.Zero(t, actual)
			require.Empty(t, payload)
		})
	}

	status, actual, payload := submit(200, 0x05, 0x0000)
	require.Zero(t, status)
	require.Equal(t, uint32(64), actual)
	require.Len(t, payload, 64)
	require.Equal(t, byte(0x05), payload[0])
	require.Equal(t, uint64(1), base.snapshotCalls.Load(),
		"stalled requests must not reach the snapshot source")

	require.NoError(t, clientConn.Close())
	require.Error(t, <-errCh)
}

func TestUrbStreamLifecycleRequiresExactControlEnvelope(t *testing.T) {
	device := newPresentationTestDevice()
	device.desc = &usbdesc.Descriptor{
		Device: usbdesc.DeviceDescriptor{Speed: 2},
		Configuration: usbdesc.ConfigurationDescriptor{
			BConfigurationValue: 1,
		},
		Interfaces: []usbdesc.InterfaceConfig{{
			Descriptor: usbdesc.InterfaceDescriptor{
				BInterfaceNumber: 0, BAlternateSetting: 0,
			},
			Endpoints: []usbdesc.EndpointDescriptor{
				{
					BEndpointAddress: 0x01, BMAttributes: 0x02,
					WMaxPacketSize: 64, BInterval: 1,
				},
				{
					BEndpointAddress: 0x84, BMAttributes: 0x03,
					WMaxPacketSize: 64, BInterval: 4,
				},
			},
		}},
	}
	bus := virtualbus.New(253)
	defer bus.Close() //nolint:errcheck
	_, err := bus.Add(device)
	require.NoError(t, err)
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close() //nolint:errcheck
	require.NoError(t,
		clientConn.SetDeadline(time.Now().Add(2*time.Second)))
	errCh := make(chan error, 1)
	go func() { errCh <- server.handleUrbStream(serverConn, device) }()

	submit := func(seq, dir, ep uint32, setup [8]byte,
		payload []byte) (int32, uint32) {
		t.Helper()
		cmd := usbip.CmdSubmit{
			Basic: usbip.HeaderBasic{
				Command: usbip.CmdSubmitCode, Seqnum: seq,
				Dir: dir, Ep: ep,
			},
			TransferBufferLen: uint32(len(payload)),
			NumberOfPackets:   -1,
			Setup:             setup,
		}
		require.NoError(t, cmd.Write(clientConn))
		if len(payload) != 0 {
			_, err := clientConn.Write(payload)
			require.NoError(t, err)
		}
		var response [retSubmitHeaderSize]byte
		require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
		return int32(binary.BigEndian.Uint32(response[20:24])),
			binary.BigEndian.Uint32(response[24:28])
	}

	setConfigurationOne := [8]byte{
		usbReqTypeStandardToDevice, usbReqSetConfiguration,
		1, 0, 0, 0, 0, 0,
	}
	setConfigurationZero := setConfigurationOne
	setConfigurationZero[2] = 0
	require.Equal(t, uint64(1), device.generation.Load())
	require.Equal(t, uint8(1), server.getDeviceConfiguration(device))

	status, actual := submit(101, usbip.DirOut, 1,
		setConfigurationZero, []byte{0xa5})
	require.Zero(t, status)
	require.Equal(t, uint32(1), actual)
	require.Equal(t, uint64(1), device.generation.Load(),
		"non-control reserved setup bytes retired presentation")
	require.Equal(t, uint8(1), server.getDeviceConfiguration(device))
	device.mu.Lock()
	require.Equal(t, [][]byte{{0xa5}}, device.isoOutPayloads,
		"active non-control OUT did not reach the device")
	device.mu.Unlock()

	clearInputHalt := [8]byte{
		usbReqTypeStandardToEndpoint, usbReqClearFeature,
		0, 0, 0x84, 0, 0, 0,
	}
	for index, setup := range [][8]byte{
		setConfigurationZero, clearInputHalt,
	} {
		status, actual = submit(102+uint32(index), usbip.DirIn, 0,
			setup, nil)
		require.Equal(t, int32(errPipe), status)
		require.Zero(t, actual)
		require.Equal(t, uint64(1), device.generation.Load(),
			"DirIn EP0 submit retired presentation")
		require.Equal(t, uint8(1), server.getDeviceConfiguration(device))
	}

	status, actual = submit(104, usbip.DirOut, 0,
		setConfigurationZero, []byte{0x5a})
	require.Equal(t, int32(errPipe), status)
	require.Zero(t, actual)
	require.Equal(t, uint64(1), device.generation.Load(),
		"mismatched EP0 data stage retired presentation")
	require.Equal(t, uint8(1), server.getDeviceConfiguration(device))

	status, actual = submit(105, usbip.DirOut, 0,
		setConfigurationOne, nil)
	require.Zero(t, status)
	require.Zero(t, actual)
	require.Equal(t, uint64(2), device.generation.Load(),
		"exact lifecycle request did not retire exactly once")
	require.Equal(t, uint8(1), server.getDeviceConfiguration(device))

	require.NoError(t, clientConn.Close())
	require.Error(t, <-errCh)
}

func TestUrbStreamAdmitsOnlyConfiguredActiveAlternateEndpoint(t *testing.T) {
	desc := &usbdesc.Descriptor{
		Device: usbdesc.DeviceDescriptor{Speed: 2},
		Configuration: usbdesc.ConfigurationDescriptor{
			BConfigurationValue: 1,
		},
		Interfaces: []usbdesc.InterfaceConfig{
			{Descriptor: usbdesc.InterfaceDescriptor{
				BInterfaceNumber: 0, BAlternateSetting: 0,
			}},
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 0, BAlternateSetting: 1,
				},
				Endpoints: []usbdesc.EndpointDescriptor{
					{
						BEndpointAddress: 0x81, BMAttributes: 0x02,
						WMaxPacketSize: 64, BInterval: 1,
					},
					{
						BEndpointAddress: 0x01, BMAttributes: 0x02,
						WMaxPacketSize: 64, BInterval: 1,
					},
					{
						BEndpointAddress: 0x02, BMAttributes: 0x01,
						WMaxPacketSize: 8, BInterval: 1,
					},
				},
			},
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 0, BAlternateSetting: 2,
				},
				Endpoints: []usbdesc.EndpointDescriptor{{
					BEndpointAddress: 0x81, BMAttributes: 0x03,
					WMaxPacketSize: 8, BInterval: 4,
				}},
			},
		},
	}
	dev := &activeEndpointTestDevice{desc: desc}
	bus := virtualbus.New(254)
	defer bus.Close() //nolint:errcheck
	_, err := bus.Add(dev)
	require.NoError(t, err)
	server := New(ServerConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close() //nolint:errcheck
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(2*time.Second)))
	errCh := make(chan error, 1)
	go func() { errCh <- server.handleUrbStream(serverConn, dev) }()

	submit := func(seq uint32, dir, ep, length uint32,
		setup [8]byte) (int32, uint32, []byte) {
		t.Helper()
		cmd := usbip.CmdSubmit{
			Basic: usbip.HeaderBasic{
				Command: usbip.CmdSubmitCode, Seqnum: seq,
				Dir: dir, Ep: ep,
			},
			TransferBufferLen: length,
			NumberOfPackets:   -1,
			Setup:             setup,
		}
		require.NoError(t, cmd.Write(clientConn))
		var header [retSubmitHeaderSize]byte
		require.NoError(t, usbip.ReadExactly(clientConn, header[:]))
		status := int32(binary.BigEndian.Uint32(header[20:24]))
		actual := binary.BigEndian.Uint32(header[24:28])
		payload := make([]byte, actual)
		if actual > 0 {
			require.NoError(t, usbip.ReadExactly(clientConn, payload))
		}
		return status, actual, payload
	}
	setInterface := func(seq uint32, alt uint8) int32 {
		status, _, _ := submit(seq, usbip.DirOut, 0, 0, [8]byte{
			usbReqTypeStandardFromInterface, usbReqSetInterface,
			alt, 0, 0, 0, 0, 0,
		})
		return status
	}
	setConfiguration := func(seq uint32, value uint8) int32 {
		status, _, _ := submit(seq, usbip.DirOut, 0, 0, [8]byte{
			usbReqTypeStandardToDevice, usbReqSetConfiguration,
			value, 0, 0, 0, 0, 0,
		})
		return status
	}
	submitInput := func(seq uint32) (int32, uint32, []byte) {
		return submit(seq, usbip.DirIn, 1, 64, [8]byte{})
	}
	submitOutput := func(seq, ep uint32, payload []byte,
		iso bool) (int32, uint32, []byte) {
		t.Helper()
		packetCount := int32(-1)
		if iso {
			packetCount = 1
		}
		cmd := usbip.CmdSubmit{
			Basic: usbip.HeaderBasic{
				Command: usbip.CmdSubmitCode, Seqnum: seq,
				Dir: usbip.DirOut, Ep: ep,
			},
			TransferBufferLen: uint32(len(payload)),
			NumberOfPackets:   packetCount,
		}
		require.NoError(t, cmd.Write(clientConn))
		_, err := clientConn.Write(payload)
		require.NoError(t, err)
		if iso {
			descriptor := usbip.IsoPacketDescriptor{
				Length: uint32(len(payload)),
			}
			require.NoError(t, descriptor.Write(clientConn))
		}
		var header [retSubmitHeaderSize]byte
		require.NoError(t, usbip.ReadExactly(clientConn, header[:]))
		status := int32(binary.BigEndian.Uint32(header[20:24]))
		actual := binary.BigEndian.Uint32(header[24:28])
		var descriptorWire []byte
		if iso {
			descriptorWire = make([]byte, usbip.IsoPacketDescriptorSize)
			require.NoError(t,
				usbip.ReadExactly(clientConn, descriptorWire))
		}
		return status, actual, descriptorWire
	}

	status, actual, _ := submitInput(1)
	assert.Equal(t, int32(errPipe), status)
	assert.Zero(t, actual)
	assert.Zero(t, dev.transferCalls.Load())

	assert.Zero(t, setInterface(2, 1))
	bindingOne, found := server.activeEndpointBinding(dev, 1, usbip.DirIn)
	require.True(t, found)
	assert.Equal(t, uint8(1), bindingOne.alternateSetting)
	status, actual, payload := submitInput(3)
	assert.Zero(t, status)
	assert.Equal(t, uint32(1), actual)
	assert.Equal(t, []byte{0x5a}, payload)
	assert.Equal(t, uint32(1), dev.transferCalls.Load())
	status, actual, _ = submitOutput(4, 1, []byte{0x11, 0x22}, false)
	assert.Zero(t, status)
	assert.Equal(t, uint32(2), actual)
	assert.Equal(t, uint32(2), dev.transferCalls.Load())

	assert.Zero(t, setConfiguration(5, 0))
	status, actual, _ = submitInput(6)
	assert.Equal(t, int32(errPipe), status)
	assert.Zero(t, actual)
	assert.Equal(t, uint32(2), dev.transferCalls.Load())
	status, actual, _ = submitOutput(7, 1, []byte{0x33}, false)
	assert.Equal(t, int32(errPipe), status)
	assert.Zero(t, actual)
	status, actual, descriptorWire := submitOutput(
		8, 2, []byte{0x44}, true)
	assert.Equal(t, int32(errPipe), status)
	assert.Zero(t, actual)
	require.Len(t, descriptorWire, usbip.IsoPacketDescriptorSize)
	assert.Equal(t, int32(errPipe), int32(binary.BigEndian.Uint32(
		descriptorWire[12:16])))
	assert.Equal(t, uint32(2), dev.transferCalls.Load())

	assert.Zero(t, setConfiguration(9, 1))
	status, _, _ = submitInput(10)
	assert.Equal(t, int32(errPipe), status,
		"reconfiguration did not restore alternate setting zero")
	assert.Zero(t, setInterface(11, 2))
	bindingTwo, found := server.activeEndpointBinding(dev, 1, usbip.DirIn)
	require.True(t, found)
	assert.Equal(t, uint8(2), bindingTwo.alternateSetting)
	assert.Equal(t, uint16(8), bindingTwo.descriptor.WMaxPacketSize)
	status, actual, payload = submitInput(12)
	assert.Zero(t, status)
	assert.Equal(t, uint32(1), actual)
	assert.Equal(t, []byte{0x5a}, payload)
	assert.Equal(t, uint32(3), dev.transferCalls.Load())

	require.NoError(t, clientConn.Close())
	require.Error(t, <-errCh)
}

func TestProcessSubmitResolvesLogicalHIDInterfaceBeforeDeviceDispatch(t *testing.T) {
	desc := &usbdesc.Descriptor{Interfaces: []usbdesc.InterfaceConfig{
		{Descriptor: usbdesc.InterfaceDescriptor{
			BInterfaceNumber: 1, BAlternateSetting: 0, BInterfaceClass: 0x01,
		}},
		{Descriptor: usbdesc.InterfaceDescriptor{
			BInterfaceNumber: 1, BAlternateSetting: 1, BInterfaceClass: 0x01,
		}},
		{Descriptor: usbdesc.InterfaceDescriptor{
			BInterfaceNumber: 2, BAlternateSetting: 0, BInterfaceClass: 0x01,
		}},
		{Descriptor: usbdesc.InterfaceDescriptor{
			BInterfaceNumber: 3, BAlternateSetting: 0, BInterfaceClass: usbInterfaceClassHID,
		}},
	}}
	dev := &controlLifecycleTestDevice{altSettingTestDevice: &altSettingTestDevice{desc: desc}}
	server := New(ServerConfig{}, nil, nil)

	setIdle := []byte{hidReqTypeOut, hidReqSetIdle, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00}
	if response := server.processSubmit(context.Background(), dev, 0, 0, setIdle, nil); response != nil {
		t.Fatalf("SET_IDLE returned unexpected payload: % x", response)
	}
	getIdle := []byte{hidReqTypeIn, hidReqGetIdle, 0x00, 0x00, 0x03, 0x00, 0x01, 0x00}
	if response := server.processSubmit(context.Background(), dev, 0, 0, getIdle, nil); !bytes.Equal(response, []byte{0}) {
		t.Fatalf("GET_IDLE returned unexpected payload: % x", response)
	}
	if dev.controlCalls != 0 {
		t.Fatalf("common HID requests reached controller-specific dispatch %d times", dev.controlCalls)
	}
}

func TestProcessSubmitClearFeatureResetsOnlyKnownEndpoint(t *testing.T) {
	desc := &usbdesc.Descriptor{Interfaces: []usbdesc.InterfaceConfig{
		{
			Descriptor: usbdesc.InterfaceDescriptor{
				BInterfaceNumber: 1, BAlternateSetting: 1, BInterfaceClass: 0x01,
			},
			Endpoints: []usbdesc.EndpointDescriptor{{BEndpointAddress: 0x01, BMAttributes: 0x03}},
		},
		{
			Descriptor: usbdesc.InterfaceDescriptor{
				BInterfaceNumber: 2, BAlternateSetting: 1, BInterfaceClass: 0x01,
			},
			Endpoints: []usbdesc.EndpointDescriptor{{BEndpointAddress: 0x82, BMAttributes: 0x03}},
		},
	}}
	dev := &controlLifecycleTestDevice{altSettingTestDevice: &altSettingTestDevice{desc: desc}}
	server := New(ServerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	server.setInterfaceAlt(dev, 1, 1)
	server.setInterfaceAlt(dev, 2, 1)

	for _, endpoint := range []uint8{0x01, 0x82} {
		clearHalt := []byte{usbReqTypeStandardToEndpoint, usbReqClearFeature,
			0x00, 0x00, endpoint, 0x00, 0x00, 0x00}
		server.processSubmit(context.Background(), dev, 0, 0, clearHalt, nil)
	}
	if !assert.ObjectsAreEqual([]uint8{0x01, 0x82}, dev.resetEndpoints) {
		t.Fatalf("known endpoint reset mismatch: % x", dev.resetEndpoints)
	}
	if got := server.getInterfaceAlt(dev, 1); got != 1 {
		t.Fatalf("speaker endpoint reset changed interface alt setting: %d", got)
	}
	if got := server.getInterfaceAlt(dev, 2); got != 1 {
		t.Fatalf("microphone endpoint reset changed interface alt setting: %d", got)
	}

	clearUnknown := []byte{usbReqTypeStandardToEndpoint, usbReqClearFeature,
		0x00, 0x00, 0x83, 0x00, 0x00, 0x00}
	server.processSubmit(context.Background(), dev, 0, 0, clearUnknown, nil)
	if !assert.ObjectsAreEqual([]uint8{0x01, 0x82}, dev.resetEndpoints) {
		t.Fatalf("unknown endpoint triggered reset: % x", dev.resetEndpoints)
	}
}

func TestEndpointIsIsochronousUsesDescriptorDirection(t *testing.T) {
	desc := &usbdesc.Descriptor{Interfaces: []usbdesc.InterfaceConfig{{
		Endpoints: []usbdesc.EndpointDescriptor{
			{BEndpointAddress: 0x82, BMAttributes: 0x05},
			{BEndpointAddress: 0x02, BMAttributes: 0x02},
		},
	}}}

	assert.True(t, endpointIsIsochronous(desc, 2, usbip.DirIn))
	assert.False(t, endpointIsIsochronous(desc, 2, usbip.DirOut))
	assert.False(t, endpointIsIsochronous(desc, 0, usbip.DirIn))
}

func TestUrbStreamMalformedIsoInDoesNotConsumePCMAndResetsAlternateSettings(t *testing.T) {
	for i, packetCount := range []int32{-1, 0} {
		name := "non_iso_marker"
		if packetCount == 0 {
			name = "zero_packets"
		}
		t.Run(name, func(t *testing.T) {
			desc := &usbdesc.Descriptor{Interfaces: []usbdesc.InterfaceConfig{
				{
					Descriptor: usbdesc.InterfaceDescriptor{
						BInterfaceNumber:  2,
						BAlternateSetting: 0,
					},
				},
				{
					Descriptor: usbdesc.InterfaceDescriptor{
						BInterfaceNumber:  2,
						BAlternateSetting: 1,
					},
					Endpoints: []usbdesc.EndpointDescriptor{{
						BEndpointAddress: 0x82,
						BMAttributes:     0x05,
						WMaxPacketSize:   192,
						BInterval:        1,
					}},
				},
			}}
			dev := &altSettingTestDevice{desc: desc}
			bus := virtualbus.New(uint32(240 + i))
			defer bus.Close() //nolint:errcheck
			_, err := bus.Add(dev)
			require.NoError(t, err)

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			server := New(ServerConfig{}, logger, nil)
			require.NoError(t, server.AddBus(bus))
			server.setInterfaceAlt(dev, 2, 1)
			server.notifyInterfaceAlt(dev, 2, 1)

			serverConn, clientConn := net.Pipe()
			defer serverConn.Close() //nolint:errcheck
			require.NoError(t, clientConn.SetDeadline(time.Now().Add(2*time.Second)))
			errCh := make(chan error, 1)
			go func() { errCh <- server.handleUrbStream(serverConn, dev) }()

			cmd := usbip.CmdSubmit{
				Basic: usbip.HeaderBasic{
					Command: usbip.CmdSubmitCode,
					Seqnum:  17,
					Dir:     usbip.DirIn,
					Ep:      2,
				},
				TransferBufferLen: 192,
				NumberOfPackets:   packetCount,
			}
			require.NoError(t, cmd.Write(clientConn))
			var response [retSubmitHeaderSize]byte
			require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
			assert.Equal(t, uint32(usbip.RetSubmitCode), binary.BigEndian.Uint32(response[0:4]))
			assert.Equal(t, uint32(17), binary.BigEndian.Uint32(response[4:8]))
			assert.Zero(t, binary.BigEndian.Uint32(response[24:28]))
			assert.Zero(t, int32(binary.BigEndian.Uint32(response[32:36])))
			assert.Zero(t, dev.transferCalls, "malformed ISO IN consumed microphone PCM")

			require.NoError(t, clientConn.Close())
			require.Error(t, <-errCh)
			assert.Zero(t, server.getInterfaceAlt(dev, 2))
			assert.Equal(t, [][2]uint8{{2, 1}, {2, 0}}, dev.altEvents)
		})
	}
}

func TestUrbStreamPacesImmediatelyAvailableInterruptInput(t *testing.T) {
	desc := &usbdesc.Descriptor{
		Device: usbdesc.DeviceDescriptor{Speed: 2},
		Interfaces: []usbdesc.InterfaceConfig{{
			Endpoints: []usbdesc.EndpointDescriptor{{
				BEndpointAddress: 0x81,
				BMAttributes:     0x03,
				WMaxPacketSize:   32,
				BInterval:        1,
			}},
		}},
	}
	d := &immediateInterruptInTestDevice{desc: desc}
	bus := virtualbus.New(250)
	defer bus.Close() //nolint:errcheck
	_, err := bus.Add(d)
	require.NoError(t, err)

	server := New(ServerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close() //nolint:errcheck
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(2*time.Second)))
	errCh := make(chan error, 1)
	go func() { errCh <- server.handleUrbStream(serverConn, d) }()

	const completions = 8
	completionTimes := make([]time.Time, 0, completions)
	for i := 0; i < completions; i++ {
		cmd := usbip.CmdSubmit{
			Basic: usbip.HeaderBasic{
				Command: usbip.CmdSubmitCode,
				Seqnum:  uint32(i + 1),
				Dir:     usbip.DirIn,
				Ep:      1,
			},
			TransferBufferLen: 1,
			NumberOfPackets:   -1,
		}
		require.NoError(t, cmd.Write(clientConn))
		var response [retSubmitHeaderSize]byte
		require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
		require.Equal(t, uint32(1), binary.BigEndian.Uint32(response[24:28]))
		var payload [1]byte
		require.NoError(t, usbip.ReadExactly(clientConn, payload[:]))
		require.Equal(t, byte(0x5a), payload[0])
		completionTimes = append(completionTimes, time.Now())
	}

	// Seven one-millisecond service gaps should not collapse into a loopback
	// burst. Keep tolerance for timer granularity on loaded CI hosts.
	elapsed := completionTimes[len(completionTimes)-1].Sub(completionTimes[0])
	require.GreaterOrEqual(t, elapsed, 5*time.Millisecond)

	require.NoError(t, clientConn.Close())
	require.Error(t, <-errCh)
}
