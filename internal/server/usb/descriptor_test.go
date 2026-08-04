package usb

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
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
			Endpoints: []usbdesc.EndpointDescriptor{{BEndpointAddress: 0x01, BMAttributes: 0x05}},
		},
		{
			Descriptor: usbdesc.InterfaceDescriptor{
				BInterfaceNumber: 2, BAlternateSetting: 1, BInterfaceClass: 0x01,
			},
			Endpoints: []usbdesc.EndpointDescriptor{{BEndpointAddress: 0x82, BMAttributes: 0x05}},
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
