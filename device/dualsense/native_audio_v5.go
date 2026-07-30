package dualsense

import "encoding/binary"

const (
	dualSenseV5SourceFrames       = 512
	dualSenseV5SpeakerFrames      = 480
	dualSenseV5SpeakerChannels    = 2
	dualSenseV5SpeakerFrameSize   = dualSenseV5SpeakerChannels * USBHapticsAudioBytesPerSample
	dualSenseV5SpeakerPayloadSize = dualSenseV5SpeakerFrames * dualSenseV5SpeakerFrameSize
)

// resampleDualSenseV5Speaker converts the front stereo pair from one native
// 48 kHz, four-channel 512-frame USB generation into the exact 480-frame
// speaker generation consumed by the physical DualSense media interval. The
// 16:15 rational phase returns to zero after every 512 input frames, so
// consecutive calls form one continuous 45 kHz source timeline without a
// per-USB-packet phase reset.
//
// This is the integer equivalent of the DS5Dongle linear 512 -> 480 reference
// resampler. Rear-left/rear-right are intentionally ignored here; their
// independent 48 kHz -> 3 kHz conversion is performed by
// copyUSBHapticsChannelsToBluetoothSample.
func resampleDualSenseV5Speaker(dst, src []byte) int {
	if len(dst) < dualSenseV5SpeakerPayloadSize ||
		len(src) < dualSenseV5SourceFrames*USBHapticsAudioFrameSize {
		return 0
	}

	for outputFrame := 0; outputFrame < dualSenseV5SpeakerFrames; outputFrame++ {
		// sourcePosition = outputFrame * 512 / 480. Retaining the rational
		// remainder avoids floating-point phase differences across platforms.
		positionNumerator := outputFrame * dualSenseV5SourceFrames
		sourceFrame := positionNumerator / dualSenseV5SpeakerFrames
		fraction := positionNumerator % dualSenseV5SpeakerFrames
		nextSourceFrame := min(sourceFrame+1, dualSenseV5SourceFrames-1)

		for channel := 0; channel < dualSenseV5SpeakerChannels; channel++ {
			sourceOffset := sourceFrame*USBHapticsAudioFrameSize +
				channel*USBHapticsAudioBytesPerSample
			nextOffset := nextSourceFrame*USBHapticsAudioFrameSize +
				channel*USBHapticsAudioBytesPerSample
			first := int32(int16(binary.LittleEndian.Uint16(
				src[sourceOffset : sourceOffset+USBHapticsAudioBytesPerSample])))
			second := int32(int16(binary.LittleEndian.Uint16(
				src[nextOffset : nextOffset+USBHapticsAudioBytesPerSample])))
			interpolated := (first*int32(dualSenseV5SpeakerFrames-fraction) +
				second*int32(fraction)) / int32(dualSenseV5SpeakerFrames)

			destinationOffset := outputFrame*dualSenseV5SpeakerFrameSize +
				channel*USBHapticsAudioBytesPerSample
			binary.LittleEndian.PutUint16(
				dst[destinationOffset:destinationOffset+USBHapticsAudioBytesPerSample],
				uint16(int16(interpolated)))
		}
	}

	return dualSenseV5SpeakerPayloadSize
}
