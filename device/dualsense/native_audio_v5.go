package dualsense

const (
	dualSenseV5SpeakerFrames      = 480
	dualSenseV5SpeakerChannels    = 2
	dualSenseV5SpeakerFrameSize   = dualSenseV5SpeakerChannels * USBHapticsAudioBytesPerSample
	dualSenseV5SpeakerPayloadSize = dualSenseV5SpeakerFrames * dualSenseV5SpeakerFrameSize
)

// appendDualSenseV5Speaker appends the raw front stereo pair from a native
// four-channel USB generation. V5 deliberately preserves the endpoint's
// 48 kHz clock and publishes exact 480-frame (10 ms) generations. Rear
// channels are assembled independently into 512-frame haptics generations.
func appendDualSenseV5Speaker(dst, src []byte) []byte {
	frames := len(src) / USBHapticsAudioFrameSize
	if frames == 0 {
		return dst
	}

	start := len(dst)
	dst = append(dst, make([]byte, frames*dualSenseV5SpeakerFrameSize)...)
	copyDualSenseV5SpeakerChannels(dst[start:],
		src[:frames*USBHapticsAudioFrameSize])
	return dst
}

// copyDualSenseV5SpeakerChannels copies the front stereo pair from the native
// four-channel USB endpoint. Rear channels remain on the haptics clock.
func copyDualSenseV5SpeakerChannels(dst, src []byte) int {
	frames := min(len(src)/USBHapticsAudioFrameSize,
		len(dst)/dualSenseV5SpeakerFrameSize)
	for frame := 0; frame < frames; frame++ {
		source := frame * USBHapticsAudioFrameSize
		destination := frame * dualSenseV5SpeakerFrameSize
		copy(dst[destination:destination+dualSenseV5SpeakerFrameSize],
			src[source:source+dualSenseV5SpeakerFrameSize])
	}
	return frames * dualSenseV5SpeakerFrameSize
}
