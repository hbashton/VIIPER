package dualsense

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

func TestDualSenseAudioFeatureControlsMatchDS5Bridge(t *testing.T) {
	constructors := map[string]func(*testing.T) *DualSense{
		"DualSense": func(t *testing.T) *DualSense {
			dev, err := New(nil)
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}
			return dev
		},
		"DualSense Edge": func(t *testing.T) *DualSense {
			dev, err := NewEdge(nil)
			if err != nil {
				t.Fatalf("NewEdge returned error: %v", err)
			}
			return dev
		},
	}

	for name, construct := range constructors {
		t.Run(name, func(t *testing.T) {
			dev := construct(t)
			assertAudioFeatureState(t, dev, audioEntitySpeakerFeatureUnit,
				audioSpeakerVolumeDefault, audioSpeakerVolumeMinimum,
				audioSpeakerVolumeMaximum, audioSpeakerVolumeResolution)
			assertAudioFeatureState(t, dev, audioEntityMicrophoneFeature,
				audioMicrophoneVolumeDefault, audioMicrophoneVolumeMinimum,
				audioMicrophoneVolumeMaximum, audioMicrophoneVolumeResolution)

			setAudioMute(t, dev, audioEntitySpeakerFeatureUnit, true)
			got, handled := dev.HandleControl(audioClassInterfaceIn,
				audioClassRequestGetCurrent, uint16(audioControlMute)<<8,
				uint16(audioEntitySpeakerFeatureUnit)<<8, 1, nil)
			if !handled || !bytes.Equal(got, []byte{1}) {
				t.Fatalf("speaker mute did not round-trip: handled=%t got=% x", handled, got)
			}

			setAudioVolume(t, dev, audioEntitySpeakerFeatureUnit, -50*256)
			assertAudioControlValue(t, dev, audioEntitySpeakerFeatureUnit,
				audioClassRequestGetCurrent, -50*256)

			// Out-of-range values are safely clamped to the exact advertised range.
			setAudioVolume(t, dev, audioEntitySpeakerFeatureUnit, -32768)
			assertAudioControlValue(t, dev, audioEntitySpeakerFeatureUnit,
				audioClassRequestGetCurrent, audioSpeakerVolumeMinimum)
			setAudioVolume(t, dev, audioEntityMicrophoneFeature, -1)
			assertAudioControlValue(t, dev, audioEntityMicrophoneFeature,
				audioClassRequestGetCurrent, audioMicrophoneVolumeMinimum)
		})
	}
}

func TestDualSenseAudioControlHandlersAgreeWithBothDescriptors(t *testing.T) {
	constructors := map[string]func(*testing.T) *DualSense{
		"DualSense": func(t *testing.T) *DualSense {
			dev, err := New(nil)
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}
			return dev
		},
		"DualSense Edge": func(t *testing.T) *DualSense {
			dev, err := NewEdge(nil)
			if err != nil {
				t.Fatalf("NewEdge returned error: %v", err)
			}
			return dev
		},
	}

	for name, construct := range constructors {
		t.Run(name, func(t *testing.T) {
			desc := construct(t).GetDescriptor()
			controlInterface, ok := desc.Interface(InterfaceAudioControl)
			if !ok || controlInterface.Descriptor.BInterfaceClass != 0x01 ||
				controlInterface.Descriptor.BInterfaceSubClass != 0x01 {
				t.Fatalf("audio-control interface mismatch: %+v", controlInterface.Descriptor)
			}

			features := map[uint8]byte{}
			for _, classDescriptor := range controlInterface.ClassDescriptors {
				payload := classDescriptor.Payload
				if classDescriptor.DescriptorType == 0x24 && len(payload) >= 5 && payload[0] == 0x06 {
					features[payload[1]] = payload[4]
				}
			}
			for _, entity := range []uint8{
				audioEntitySpeakerFeatureUnit,
				audioEntityMicrophoneFeature,
			} {
				if controls, ok := features[entity]; !ok || controls&0x03 != 0x03 {
					t.Fatalf("entity %d does not advertise master mute+volume: controls=%#x present=%t",
						entity, controls, ok)
				}
			}

			for _, endpointAddress := range []uint8{
				EndpointHapticsAudioOut,
				EndpointMicrophoneIn,
			} {
				endpointFound := false
				for _, iface := range desc.Interfaces {
					for _, endpoint := range iface.Endpoints {
						if endpoint.BEndpointAddress != endpointAddress {
							continue
						}
						endpointFound = true
						if len(endpoint.ClassDescriptors) != 1 ||
							len(endpoint.ClassDescriptors[0].Payload) < 2 ||
							endpoint.ClassDescriptors[0].Payload[0] != 0x01 ||
							endpoint.ClassDescriptors[0].Payload[1] != 0x00 {
							t.Fatalf("endpoint %#x unexpectedly advertises sample-frequency control: %+v",
								endpointAddress, endpoint.ClassDescriptors)
						}
					}
				}
				if !endpointFound {
					t.Fatalf("descriptor omitted audio endpoint %#x", endpointAddress)
				}
			}
		})
	}
}

func TestDualSenseAudioFeatureControlsRejectMalformedRequests(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	requests := []struct {
		name                 string
		bm, request          uint8
		value, index, length uint16
		data                 []byte
	}{
		{"per-channel control", audioClassInterfaceIn, audioClassRequestGetCurrent,
			uint16(audioControlVolume)<<8 | 1, uint16(audioEntitySpeakerFeatureUnit) << 8, 2, nil},
		{"wrong AC interface", audioClassInterfaceIn, audioClassRequestGetCurrent,
			uint16(audioControlVolume) << 8, uint16(audioEntitySpeakerFeatureUnit)<<8 | 1, 2, nil},
		{"unknown entity", audioClassInterfaceIn, audioClassRequestGetCurrent,
			uint16(audioControlVolume) << 8, 7 << 8, 2, nil},
		{"short volume get", audioClassInterfaceIn, audioClassRequestGetCurrent,
			uint16(audioControlVolume) << 8, uint16(audioEntitySpeakerFeatureUnit) << 8, 1, nil},
		{"short volume set data", audioClassInterfaceOut, audioClassRequestSetCurrent,
			uint16(audioControlVolume) << 8, uint16(audioEntitySpeakerFeatureUnit) << 8, 2, []byte{0}},
		{"mute range query", audioClassInterfaceIn, audioClassRequestGetMinimum,
			uint16(audioControlMute) << 8, uint16(audioEntitySpeakerFeatureUnit) << 8, 1, nil},
	}

	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			if response, handled := dev.handleAudioControlRequest(
				request.bm, request.request, request.value, request.index,
				request.length, request.data,
			); handled || response != nil {
				t.Fatalf("malformed request was handled: handled=%t response=% x", handled, response)
			}
		})
	}
}

func TestDualSenseAudioGainDefaultsAreNeutralAndChangesRamp(t *testing.T) {
	frame := make([]byte, audioGainRampFrames*USBHapticsAudioFrameSize)
	for offset := 0; offset < len(frame); offset += 2 {
		binary.LittleEndian.PutUint16(frame[offset:offset+2], uint16(int16(10000)))
	}

	speaker := newSpeakerAudioFeatureState()
	processed, release := speaker.applyPCM(frame, USBHapticsAudioChannels)
	if release != nil || !bytes.Equal(processed, frame) {
		t.Fatal("default speaker feature changed the established PCM level")
	}

	speaker.setMute(true)
	processed, release = speaker.applyPCM(frame, USBHapticsAudioChannels)
	if release == nil {
		t.Fatal("muted speaker PCM was not processed")
	}
	defer release()
	first := int16(binary.LittleEndian.Uint16(processed[:2]))
	lastOffset := (audioGainRampFrames - 1) * USBHapticsAudioFrameSize
	last := int16(binary.LittleEndian.Uint16(processed[lastOffset : lastOffset+2]))
	if first <= 0 || first >= 10000 || last != 0 {
		t.Fatalf("speaker mute ramp was discontinuous: first=%d last=%d", first, last)
	}

	microphone := newMicrophoneAudioFeatureState()
	microphone.setVolume(audioMicrophoneVolumeMinimum)
	micFrame := make([]byte, audioGainRampFrames*USBMicrophoneChannels*2)
	for offset := 0; offset < len(micFrame); offset += 2 {
		binary.LittleEndian.PutUint16(micFrame[offset:offset+2], uint16(int16(10000)))
	}
	micProcessed, micRelease := microphone.applyPCM(micFrame, USBMicrophoneChannels)
	if micRelease != nil || !bytes.Equal(micProcessed, micFrame) {
		t.Fatal("physical-style microphone gain control attenuated client capture PCM")
	}

	microphone.setMute(true)
	micProcessed, micRelease = microphone.applyPCM(micFrame, USBMicrophoneChannels)
	if micRelease == nil {
		t.Fatal("microphone mute did not process PCM")
	}
	defer micRelease()
	micFirst := int16(binary.LittleEndian.Uint16(micProcessed[:2]))
	micLastOffset := (audioGainRampFrames - 1) * USBMicrophoneChannels * 2
	micLast := int16(binary.LittleEndian.Uint16(micProcessed[micLastOffset : micLastOffset+2]))
	if micFirst <= 0 || micFirst >= 10000 || micLast != 0 {
		t.Fatalf("microphone mute ramp was unexpected: first=%d last=%d", micFirst, micLast)
	}
}

func TestDualSenseAudioGainSaturatesS16(t *testing.T) {
	state := newSpeakerAudioFeatureState()
	state.gain.current = 2
	state.gain.target = 2
	pcm := make([]byte, USBHapticsAudioFrameSize)
	binary.LittleEndian.PutUint16(pcm[0:2], uint16(int16(20000)))
	negative := int16(-20000)
	binary.LittleEndian.PutUint16(pcm[2:4], uint16(negative))

	processed, release := state.applyPCM(pcm, USBHapticsAudioChannels)
	if release == nil {
		t.Fatal("amplified PCM was not processed")
	}
	defer release()
	if got := int16(binary.LittleEndian.Uint16(processed[0:2])); got != 32767 {
		t.Fatalf("positive sample did not saturate: %d", got)
	}
	if got := int16(binary.LittleEndian.Uint16(processed[2:4])); got != -32768 {
		t.Fatalf("negative sample did not saturate: %d", got)
	}
}

func TestDualSenseMicrophoneControlAppliesAtUSBPresentation(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)

	frame := make([]byte, USBMicrophoneClientFrameSize)
	for offset := 0; offset < len(frame); offset += 2 {
		binary.LittleEndian.PutUint16(frame[offset:offset+2], uint16(int16(10000)))
	}
	for range microphoneTargetClientFrames {
		dev.QueueMicrophonePCMFrame(frame)
	}

	// Change the feature control only after the raw PCM is already buffered.
	// The queued samples must still observe the new value at USB presentation.
	setAudioMute(t, dev, audioEntityMicrophoneFeature, true)
	framesPresented := 0
	var first, last int16
	for framesPresented < audioGainRampFrames {
		packet := dev.HandleTransfer(context.Background(),
			uint32(EndpointMicrophoneIn&0x0F), usbip.DirIn, nil)
		if len(packet) < USBMicrophoneChannels*2 {
			t.Fatalf("microphone returned a short packet: %d", len(packet))
		}
		if framesPresented == 0 {
			first = int16(binary.LittleEndian.Uint16(packet[:2]))
		}
		lastOffset := len(packet) - USBMicrophoneChannels*2
		last = int16(binary.LittleEndian.Uint16(packet[lastOffset : lastOffset+2]))
		framesPresented += len(packet) / (USBMicrophoneChannels * 2)
	}
	if first <= 0 || first >= 10000 || last != 0 {
		t.Fatalf("buffered microphone PCM missed capture-time mute ramp: first=%d last=%d", first, last)
	}
	if got := int16(binary.LittleEndian.Uint16(frame[:2])); got != 10000 {
		t.Fatalf("microphone enqueue source was modified: %d", got)
	}
}

func TestDualSenseAudioLifecycleDropsInactiveAndStalePCM(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	const bytesPerHapticsReport = (BluetoothHapticsSampleSize / 2) *
		USBHapticsAudioDownsample * USBHapticsAudioFrameSize
	half := make([]byte, bytesPerHapticsReport/2)
	speakerCallbacks := 0
	speakerResets := 0
	feedbackCallbacks := 0
	dev.SetSpeakerCallback(func([]byte) { speakerCallbacks++ })
	dev.SetSpeakerResetCallback(func() { speakerResets++ })
	dev.SetOutputCallback(func(OutputState) { feedbackCallbacks++ })

	dev.HandleTransfer(context.Background(), EndpointHapticsAudioOut, usbip.DirOut, half)
	if speakerCallbacks != 0 || len(dev.hapticsPCM) != 0 {
		t.Fatal("inactive render endpoint accepted PCM")
	}

	dev.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
	dev.HandleTransfer(context.Background(), EndpointHapticsAudioOut, usbip.DirOut, half)
	if speakerCallbacks != 1 || len(dev.hapticsPCM) != len(half) {
		t.Fatal("active render endpoint did not retain its partial current generation")
	}

	dev.SetInterfaceAltSetting(InterfaceHapticsAudio, 0)
	dev.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
	dev.HandleTransfer(context.Background(), EndpointHapticsAudioOut, usbip.DirOut, half)
	if feedbackCallbacks != 0 || len(dev.hapticsPCM) != len(half) {
		t.Fatal("stale haptics PCM crossed an interface close/reopen boundary")
	}

	dev.ResetEndpoint(EndpointHapticsAudioOut)
	if len(dev.hapticsPCM) != 0 || speakerResets != 4 {
		t.Fatalf("speaker endpoint reset did not clear transport state: buffered=%d resets=%d",
			len(dev.hapticsPCM), speakerResets)
	}

	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	micFrame := make([]byte, USBMicrophoneClientFrameSize)
	for range microphoneTargetClientFrames {
		dev.QueueMicrophonePCMFrame(micFrame)
	}
	if state := dev.microphoneBuffer.State(); state.QueuedBytes == 0 {
		t.Fatal("microphone precondition did not queue PCM")
	}
	dev.ResetEndpoint(EndpointMicrophoneIn)
	if !dev.microphoneInterfaceActive || dev.microphoneBuffer.State().QueuedBytes != 0 {
		t.Fatal("microphone pipe reset changed alt state or retained PCM")
	}
}

func TestDualSenseFixedSampleFrequencyCompatibilityIsStrict(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	for _, endpoint := range []uint16{EndpointHapticsAudioOut, EndpointMicrophoneIn} {
		got, handled := dev.HandleControl(audioClassEndpointIn,
			audioClassRequestGetCurrent, uint16(audioControlSamplingFrequency)<<8,
			endpoint, 3, nil)
		if !handled || !bytes.Equal(got, []byte{0x80, 0xBB, 0x00}) {
			t.Fatalf("48 kHz query failed for endpoint %#x: handled=%t got=% x", endpoint, handled, got)
		}
		if _, handled = dev.handleAudioControlRequest(audioClassEndpointOut,
			audioClassRequestSetCurrent, uint16(audioControlSamplingFrequency)<<8,
			endpoint, 3, []byte{0x44, 0xAC, 0x00}); handled {
			t.Fatalf("endpoint %#x accepted unsupported 44.1 kHz", endpoint)
		}
	}
}

func assertAudioFeatureState(t *testing.T, dev *DualSense, entity uint8,
	current, minimum, maximum, resolution int16,
) {
	t.Helper()
	got, handled := dev.HandleControl(audioClassInterfaceIn,
		audioClassRequestGetCurrent, uint16(audioControlMute)<<8,
		uint16(entity)<<8, 1, nil)
	if !handled || !bytes.Equal(got, []byte{0}) {
		t.Fatalf("entity %d default mute: handled=%t got=% x", entity, handled, got)
	}
	assertAudioControlValue(t, dev, entity, audioClassRequestGetCurrent, current)
	assertAudioControlValue(t, dev, entity, audioClassRequestGetMinimum, minimum)
	assertAudioControlValue(t, dev, entity, audioClassRequestGetMaximum, maximum)
	assertAudioControlValue(t, dev, entity, audioClassRequestGetResolution, resolution)
}

func assertAudioControlValue(t *testing.T, dev *DualSense, entity, request uint8, want int16) {
	t.Helper()
	got, handled := dev.HandleControl(audioClassInterfaceIn, request,
		uint16(audioControlVolume)<<8, uint16(entity)<<8, 2, nil)
	if !handled || len(got) != 2 || int16(binary.LittleEndian.Uint16(got)) != want {
		t.Fatalf("entity %d request %#x: handled=%t got=% x want=%d", entity, request, handled, got, want)
	}
}

func setAudioMute(t *testing.T, dev *DualSense, entity uint8, mute bool) {
	t.Helper()
	value := byte(0)
	if mute {
		value = 1
	}
	if _, handled := dev.HandleControl(audioClassInterfaceOut,
		audioClassRequestSetCurrent, uint16(audioControlMute)<<8,
		uint16(entity)<<8, 1, []byte{value}); !handled {
		t.Fatalf("entity %d mute SET_CUR was not handled", entity)
	}
}

func setAudioVolume(t *testing.T, dev *DualSense, entity uint8, volume int16) {
	t.Helper()
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, uint16(volume))
	if _, handled := dev.HandleControl(audioClassInterfaceOut,
		audioClassRequestSetCurrent, uint16(audioControlVolume)<<8,
		uint16(entity)<<8, 2, data); !handled {
		t.Fatalf("entity %d volume SET_CUR was not handled", entity)
	}
}
