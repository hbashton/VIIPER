package dualshock4

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescriptorExposesNativeDS4AudioTopology(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)

	desc := dev.GetDescriptor()
	assert.Equal(t, uint8(4), desc.NumInterfaces())
	assert.Equal(t, uint8(InterfaceHID), desc.Interfaces[len(desc.Interfaces)-1].Descriptor.BInterfaceNumber)
	require.Len(t, desc.Interfaces[0].ClassDescriptors, 7)
	assert.Len(t, desc.Interfaces[0].ClassDescriptors[2].Payload, 8)

	var speakerEndpointFound bool
	var microphoneEndpointFound bool
	for _, iface := range desc.Interfaces {
		for _, endpoint := range iface.Endpoints {
			switch endpoint.BEndpointAddress {
			case EndpointAudioOut:
				speakerEndpointFound = true
				assert.Equal(t, uint8(0x09), endpoint.BMAttributes)
				assert.Equal(t, uint16(USBSpeakerMaxPacketSize), endpoint.WMaxPacketSize)
			case EndpointMicrophoneIn:
				microphoneEndpointFound = true
				assert.Equal(t, uint8(0x05), endpoint.BMAttributes)
				assert.Equal(t, uint16(USBMicrophoneMaxPacketSize), endpoint.WMaxPacketSize)
			}
		}
	}

	assert.True(t, speakerEndpointFound)
	assert.True(t, microphoneEndpointFound)

	configurationLength := 9
	for _, iface := range desc.Interfaces {
		configurationLength += 9
		if iface.HID != nil {
			configurationLength += 9
		}
		for _, classDescriptor := range iface.ClassDescriptors {
			configurationLength += 2 + len(classDescriptor.Payload)
		}
		for _, endpoint := range iface.Endpoints {
			configurationLength += 7 + len(endpoint.Trailing)
			for _, classDescriptor := range endpoint.ClassDescriptors {
				configurationLength += 2 + len(classDescriptor.Payload)
			}
		}
	}
	assert.Equal(t, 225, configurationLength)
}

func TestAudioSamplingFrequencyControlsMatchDS4Hardware(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)

	speaker, handled := dev.HandleControl(audioClassEndpointIn, audioClassRequestGetCurrent,
		uint16(audioControlSamplingFrequency)<<8, EndpointAudioOut, 3, nil)
	require.True(t, handled)
	assert.Equal(t, []byte{0x00, 0x7D, 0x00}, speaker)

	microphone, handled := dev.HandleControl(audioClassEndpointIn, audioClassRequestGetCurrent,
		uint16(audioControlSamplingFrequency)<<8, EndpointMicrophoneIn, 3, nil)
	require.True(t, handled)
	assert.Equal(t, []byte{0x80, 0x3E, 0x00}, microphone)
}

func TestAudioInterfacesTrackAlternateSettings(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)

	dev.SetInterfaceAltSetting(InterfaceSpeaker, 1)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	state := dev.GetDeviceSpecificArgs()
	assert.Equal(t, true, state["speakerInterfaceActive"])
	assert.Equal(t, true, state["microphoneInterfaceActive"])

	microphone := dev.HandleTransfer(context.Background(), uint32(EndpointMicrophoneIn), usbip.DirIn, nil)
	assert.Len(t, microphone, USBMicrophonePacketSize)
	assert.Equal(t, make([]byte, USBMicrophonePacketSize), microphone)
}

func TestSpeakerTransferIsForwardedWithoutLoopbackCapture(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	dev.SetInterfaceAltSetting(InterfaceSpeaker, 1)

	forwarded := make(chan []byte, 1)
	dev.SetSpeakerCallback(func(pcm []byte) { forwarded <- pcm })
	pcm := make([]byte, 128)
	for index := range pcm {
		pcm[index] = byte(index)
	}

	dev.HandleTransfer(context.Background(), uint32(EndpointAudioOut),
		usbip.DirOut, pcm)
	got := <-forwarded
	assert.Equal(t, pcm, got)

	// The callback owns a copy; reusing the USB/IP buffer must not mutate it.
	pcm[0] = 0xFF
	assert.Equal(t, byte(0), got[0])
	dev.SetSpeakerCallback(nil)
}

func TestDuplexWriterFramesSpeakerPCM(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualShock4OutputWriter(server, StreamFrameVersionV3)
	go writer.Run()

	pcm := []byte{0x00, 0x01, 0xFE, 0xFF}
	writer.EnqueueAudio(StreamFrameSpeakerPCM, pcm)
	header := make([]byte, StreamFrameV2HeaderSize)
	_, err := io.ReadFull(client, header)
	require.NoError(t, err)
	payload := make([]byte, binary.LittleEndian.Uint16(header[6:8]))
	_, err = io.ReadFull(client, payload)
	require.NoError(t, err)

	assert.Equal(t, []byte{StreamFrameMagic0, StreamFrameMagic1,
		StreamFrameMagic2, StreamFrameMagic3}, header[:4])
	assert.Equal(t, byte(StreamFrameVersionV3), header[4])
	assert.Equal(t, byte(StreamFrameSpeakerPCM), header[5])
	assert.Equal(t, uint32(0), binary.LittleEndian.Uint32(header[8:12]))
	assert.Equal(t, dualShock4FramedStreamCRC(header[4:12], payload),
		binary.LittleEndian.Uint32(header[12:16]))
	assert.Equal(t, pcm, payload)

	require.NoError(t, client.Close())
	writer.Stop()
}

func TestFramedStreamQueuesDS4MicrophonePCM(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)

	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- readDualShock4InputStream(server, dev,
			slog.New(slog.NewTextHandler(io.Discard, nil)), true,
			StreamFrameVersionV2)
	}()

	input, err := NewInputState().MarshalBinary()
	require.NoError(t, err)
	microphone := make([]byte, USBMicrophoneClientFrameSize)
	for index := range microphone {
		microphone[index] = byte(index)
	}

	_, err = client.Write(makeDualShock4StreamFrame(StreamFrameInputState, 0, input))
	require.NoError(t, err)
	_, err = client.Write(makeDualShock4StreamFrame(StreamFrameMicrophonePCM, 1, microphone))
	require.NoError(t, err)
	require.NoError(t, client.Close())
	require.NoError(t, <-done)

	packet := dev.HandleTransfer(context.Background(),
		uint32(EndpointMicrophoneIn&0x0F), usbip.DirIn, nil)
	require.Len(t, packet, USBMicrophonePacketSize)
	assert.Equal(t, microphone[:USBMicrophonePacketSize], packet)
}

func makeDualShock4StreamFrame(frameType byte, sequence uint32,
	payload []byte) []byte {
	header := make([]byte, StreamFrameV2HeaderSize)
	header[0] = StreamFrameMagic0
	header[1] = StreamFrameMagic1
	header[2] = StreamFrameMagic2
	header[3] = StreamFrameMagic3
	header[4] = StreamFrameVersionV2
	header[5] = frameType
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(payload)))
	binary.LittleEndian.PutUint32(header[8:12], sequence)
	binary.LittleEndian.PutUint32(header[12:16],
		dualShock4FramedStreamCRC(header[4:12], payload))
	return append(header, payload...)
}
