package xboxseries

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDescriptorAdvertisesNativeGIPUSB(t *testing.T) {
	descriptor := MakeDescriptor()
	require.Equal(t, uint16(0x045E), descriptor.Device.IDVendor)
	require.Equal(t, uint16(0x0B12), descriptor.Device.IDProduct)
	require.Equal(t, uint8(0xFF), descriptor.Device.BDeviceClass)
	require.Equal(t, uint8(0x47), descriptor.Device.BDeviceSubClass)
	require.Equal(t, uint8(0xD0), descriptor.Device.BDeviceProtocol)
	require.NotNil(t, descriptor.MicrosoftOS10)
	require.Equal(t, "XGIP10", descriptor.MicrosoftOS10.CompatibleID)
	require.Len(t, descriptor.Interfaces, 1)
	require.Len(t, descriptor.Interfaces[0].Endpoints, 2)
	require.Equal(t, uint8(0x01), descriptor.Interfaces[0].Endpoints[0].BEndpointAddress)
	require.Equal(t, uint8(0x81), descriptor.Interfaces[0].Endpoints[1].BEndpointAddress)
}

func TestAuthenticationPacketIsAcknowledgedExactly(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)

	device.handleMessage(messageAuthenticate, 0x30, 0x21,
		bytes.Repeat([]byte{0xA5}, 58), 58)
	packet := device.nextPacket()
	require.Equal(t, []byte{
		messageProtocolControl, 0x20, 0x21, 0x09,
		0x00, messageAuthenticate, 0x20, 0x3A, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}, packet)
}

func TestAuthenticationDataCompleteAcknowledgesTransferLength(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)

	device.handleHostPacket([]byte{messageAuthenticate, 0xA0, 0x17,
		0x80, 0x00, 0x3A})
	require.Equal(t, []byte{
		messageProtocolControl, 0x20, 0x17, 0x09,
		0x00, messageAuthenticate, 0x20, 0x3A, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}, device.nextPacket())
}

func TestInputPayloadCarriesSeriesControls(t *testing.T) {
	state := InputState{
		Buttons: ButtonA | ButtonView | ButtonMenu | ButtonL3 | ButtonRB |
			ButtonDPadUp | ButtonShare,
		LT: 255, RT: 128,
		LX: -32768, LY: 32767, RX: -1234, RY: 5678,
	}
	payload := state.buildGamepadPayload()
	require.Len(t, payload, 32)
	require.Equal(t, byte(0x1C), payload[0])
	require.Equal(t, byte(0x61), payload[1])
	require.Equal(t, uint16(1023), binary.LittleEndian.Uint16(payload[2:4]))
	require.Equal(t, uint16(514), binary.LittleEndian.Uint16(payload[4:6]))
	require.Equal(t, int16(-32768), int16(binary.LittleEndian.Uint16(payload[6:8])))
	require.Equal(t, int16(32767), int16(binary.LittleEndian.Uint16(payload[8:10])))
	require.Equal(t, int16(-1234), int16(binary.LittleEndian.Uint16(payload[10:12])))
	require.Equal(t, int16(5678), int16(binary.LittleEndian.Uint16(payload[12:14])))
	require.Equal(t, byte(1), payload[14])
}

func TestMotorFeedbackKeepsFourChannelsAndTiming(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)

	receivedStates := make(chan MotorState, 2)
	device.SetMotorCallback(func(state MotorState) { receivedStates <- state })
	device.handleMotor([]byte{0, 0x0F, 100, 50, 25, 75, 20, 3, 2})
	received := <-receivedStates
	device.handleMotor([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0})

	require.Equal(t, byte(64), received.LeftMotor)
	require.Equal(t, byte(191), received.RightMotor)
	require.Equal(t, byte(255), received.LeftImpulse)
	require.Equal(t, byte(128), received.RightImpulse)
	require.Equal(t, byte(20), received.Duration)
	require.Equal(t, byte(3), received.Delay)
	require.Equal(t, byte(2), received.Repeat)
}

func TestMotorDurationStopsAllChannels(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)

	received := make(chan MotorState, 2)
	device.SetMotorCallback(func(state MotorState) { received <- state })
	device.handleMotor([]byte{0, 0x0F, 100, 50, 25, 75, 1, 0, 0})

	require.Equal(t, byte(255), (<-received).LeftImpulse)
	select {
	case stopped := <-received:
		require.Equal(t, MotorState{}, stopped)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed motor command did not stop")
	}
}

func TestNewMotorCommandCancelsOldTimedStop(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)

	received := make(chan MotorState, 4)
	device.SetMotorCallback(func(state MotorState) { received <- state })
	device.handleMotor([]byte{0, 0x08, 100, 0, 0, 0, 2, 0, 0})
	require.Equal(t, byte(255), (<-received).LeftImpulse)
	device.handleMotor([]byte{0, 0x04, 0, 100, 0, 0, 5, 0, 0})
	require.Equal(t, byte(255), (<-received).RightImpulse)

	select {
	case stale := <-received:
		t.Fatalf("superseded command published stale state: %+v", stale)
	case <-time.After(35 * time.Millisecond):
	}
	select {
	case stopped := <-received:
		require.Equal(t, MotorState{}, stopped)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("replacement command did not stop")
	}
}

func TestZeroDurationStopsEveryMotor(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)

	var received MotorState
	device.SetMotorCallback(func(state MotorState) { received = state })
	device.handleMotor([]byte{0, 0x0F, 100, 100, 100, 100, 0, 7, 9})

	require.Equal(t, MotorState{Duration: 0, Delay: 7, Repeat: 9}, received)
}

func TestMetadataIncludesSeriesFunctionMap(t *testing.T) {
	metadata := seriesMetadata()
	require.Len(t, metadata, 214)
	require.Equal(t, uint16(16), binary.LittleEndian.Uint16(metadata[0:2]))
	require.Equal(t, uint16(1), binary.LittleEndian.Uint16(metadata[2:4]))
	require.Equal(t, uint16(len(metadata)), binary.LittleEndian.Uint16(metadata[14:16]))
	require.Equal(t, uint16(151), binary.LittleEndian.Uint16(metadata[16:18]))
	require.Equal(t, byte(2), metadata[167])
	require.Equal(t, byte(messageInput), metadata[170])
	require.Equal(t, uint16(32), binary.LittleEndian.Uint16(metadata[171:173]))
	require.True(t, bytes.Contains(metadata,
		guidBytes("ECDDD2FE-D387-4294-BD96-1A712E3DC77D")))
	require.True(t, bytes.Contains(metadata,
		guidBytes("7A34CE77-7DE2-45C6-8CA4-0042C08BD94A")))
	share := bytes.Index(metadata,
		guidBytes("ECDDD2FE-D387-4294-BD96-1A712E3DC77D"))
	gamepad := bytes.Index(metadata,
		guidBytes("082E402C-07DF-45E1-A5AB-A3127AF197B5"))
	navigation := bytes.Index(metadata,
		guidBytes("B8F31FE7-7386-40E9-A9F8-2F21263ACFB7"))
	controller := bytes.Index(metadata,
		guidBytes("9776FF56-9BFD-4581-AD45-B645BBA526D6"))
	require.True(t, share < gamepad && gamepad < navigation &&
		navigation < controller,
		"interfaces must remain ordered from most specific to least specific")
}

func TestMetadataFragmentsUseDocumentedGIPHeaderForms(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)
	device.metadataSeq = 7

	device.mu.Lock()
	device.queueMetadataFragmentLocked(0)
	device.queueMetadataFragmentLocked(58)
	device.queueMetadataFragmentLocked(116)
	device.queueMetadataFragmentLocked(174)
	packets := append([][]byte(nil), device.queue...)
	device.mu.Unlock()

	require.Equal(t, []byte{messageMetadata, 0xF0, 7, 58, 0xD6, 1},
		packets[0][:6])
	require.Equal(t, []byte{messageMetadata, 0xA0, 7, 0xBA, 0, 58},
		packets[1][:6])
	require.Equal(t, []byte{messageMetadata, 0xA0, 7, 0xBA, 0, 116},
		packets[2][:6])
	require.Equal(t, []byte{messageMetadata, 0xB0, 7, 40, 0xAE, 1},
		packets[3][:6])
}

func TestMetadataAckQueuesRemainingFragmentsAndCompletion(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)
	device.metadataSeq = 9
	device.metadataSent = 58

	device.handleAck([]byte{0, messageMetadata, 0xF0, 58, 0, 0, 0, 156, 0})
	device.mu.Lock()
	require.Len(t, device.queue, 3)
	device.queue = device.queue[:0]
	device.mu.Unlock()

	device.handleAck([]byte{0, messageMetadata, 0xB0, 214, 0, 0, 0, 0, 0})
	device.mu.Lock()
	require.Equal(t, metadataComplete(9, 214), device.queue[0])
	device.mu.Unlock()
}
