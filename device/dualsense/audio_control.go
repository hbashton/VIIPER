package dualsense

import (
	"encoding/binary"
	"math"
	"sync"
)

const (
	audioClassRequestSetCurrent    = 0x01
	audioClassRequestGetCurrent    = 0x81
	audioClassRequestGetMinimum    = 0x82
	audioClassRequestGetMaximum    = 0x83
	audioClassRequestGetResolution = 0x84

	audioClassInterfaceOut = 0x21
	audioClassInterfaceIn  = 0xA1
	audioClassEndpointOut  = 0x22
	audioClassEndpointIn   = 0xA2

	audioControlMute              = 0x01
	audioControlVolume            = 0x02
	audioControlSamplingFrequency = 0x01

	audioEntitySpeakerFeatureUnit = 0x02
	audioEntityMicrophoneFeature  = 0x05

	// The physical-style UAC feature controls use signed Q8.8 decibels.
	audioSpeakerVolumeMinimum    int16 = -100 * 256
	audioSpeakerVolumeMaximum    int16 = 0
	audioSpeakerVolumeResolution int16 = 1 * 256
	audioSpeakerVolumeDefault    int16 = audioSpeakerVolumeMaximum

	audioMicrophoneVolumeMinimum    int16 = 0
	audioMicrophoneVolumeMaximum    int16 = 48 * 256
	audioMicrophoneVolumeResolution int16 = 0x007A
	audioMicrophoneVolumeDefault    int16 = audioMicrophoneVolumeMaximum

	// Five milliseconds is long enough to remove a hard discontinuity while
	// remaining imperceptible as control latency.
	audioGainRampFrames = USBMicrophoneSampleRate / 200
)

var audioGainBufferPool sync.Pool

type audioGainBuffer struct {
	data []byte
}

type audioFeatureState struct {
	mute          bool
	volume        int16
	minimum       int16
	maximum       int16
	resolution    int16
	defaultVolume int16
	gain          audioGainRamp
}

type audioGainRamp struct {
	current         float64
	target          float64
	framesRemaining int
}

func newAudioFeatureState(minimum, maximum, resolution, defaultVolume int16) audioFeatureState {
	return audioFeatureState{
		volume:        defaultVolume,
		minimum:       minimum,
		maximum:       maximum,
		resolution:    resolution,
		defaultVolume: defaultVolume,
		gain: audioGainRamp{
			current: 1.0,
			target:  1.0,
		},
	}
}

func newSpeakerAudioFeatureState() audioFeatureState {
	return newAudioFeatureState(
		audioSpeakerVolumeMinimum,
		audioSpeakerVolumeMaximum,
		audioSpeakerVolumeResolution,
		audioSpeakerVolumeDefault,
	)
}

func newMicrophoneAudioFeatureState() audioFeatureState {
	return newAudioFeatureState(
		audioMicrophoneVolumeMinimum,
		audioMicrophoneVolumeMaximum,
		audioMicrophoneVolumeResolution,
		audioMicrophoneVolumeDefault,
	)
}

func (s *audioFeatureState) setMute(mute bool) {
	if s.mute == mute {
		return
	}
	s.mute = mute
	s.beginGainTransition()
}

func (s *audioFeatureState) setVolume(volume int16) {
	volume = max(s.minimum, min(s.maximum, volume))
	if s.volume == volume {
		return
	}
	s.volume = volume
	s.beginGainTransition()
}

func (s *audioFeatureState) beginGainTransition() {
	s.gain.target = s.targetGain()
	s.gain.framesRemaining = audioGainRampFrames
}

func (s *audioFeatureState) resetStreamGain() {
	s.gain.target = s.targetGain()
	s.gain.current = s.gain.target
	s.gain.framesRemaining = 0
}

// targetGain is relative to the feature unit's default value. In particular,
// the microphone advertises the DualSense-style 0..+48 dB hardware range while
// its +48 dB default remains unity for the already calibrated decoded PCM path.
func (s *audioFeatureState) targetGain() float64 {
	if s.mute {
		return 0
	}
	return math.Pow(10, float64(int(s.volume)-int(s.defaultVolume))/(256.0*20.0))
}

func (s *audioFeatureState) needsPCMProcessing() bool {
	return s.gain.framesRemaining != 0 || s.gain.current != 1.0 || s.gain.target != 1.0
}

// applyPCM applies one master feature-unit gain to interleaved signed S16LE
// PCM. The caller must serialize access to the feature state. The returned
// release function must be called once the synchronous consumer has copied the
// returned bytes.
func (s *audioFeatureState) applyPCM(src []byte, channels int) ([]byte, func()) {
	if len(src) == 0 || channels <= 0 || !s.needsPCMProcessing() {
		return src, nil
	}

	buffer := acquireAudioGainBuffer(len(src))
	dst := buffer.data
	copy(dst, src)
	s.applyPCMInPlace(dst, channels)

	return dst, func() { releaseAudioGainBuffer(buffer) }
}

// applyPCMInPlace is used for freshly allocated USB capture packets. Applying
// microphone controls here, after the jitter buffer, makes a control change
// affect every not-yet-presented sample, including PCM queued before the SET.
func (s *audioFeatureState) applyPCMInPlace(pcm []byte, channels int) {
	if len(pcm) == 0 || channels <= 0 || !s.needsPCMProcessing() {
		return
	}

	frameSize := channels * 2
	for frameOffset := 0; frameOffset+frameSize <= len(pcm); frameOffset += frameSize {
		gain := s.gain.next()
		for channel := 0; channel < channels; channel++ {
			offset := frameOffset + channel*2
			sample := int16(binary.LittleEndian.Uint16(pcm[offset : offset+2]))
			scaled := int64(math.Round(float64(sample) * gain))
			scaled = max(int64(math.MinInt16), min(int64(math.MaxInt16), scaled))
			binary.LittleEndian.PutUint16(pcm[offset:offset+2], uint16(int16(scaled)))
		}
	}
}

func (r *audioGainRamp) next() float64 {
	if r.framesRemaining <= 0 {
		r.current = r.target
		return r.current
	}

	r.current += (r.target - r.current) / float64(r.framesRemaining)
	r.framesRemaining--
	if r.framesRemaining == 0 {
		r.current = r.target
	}
	return r.current
}

func acquireAudioGainBuffer(length int) *audioGainBuffer {
	var buffer *audioGainBuffer
	if pooled := audioGainBufferPool.Get(); pooled != nil {
		buffer = pooled.(*audioGainBuffer)
	} else {
		buffer = &audioGainBuffer{}
	}
	if cap(buffer.data) < length {
		buffer.data = make([]byte, length)
	} else {
		buffer.data = buffer.data[:length]
	}
	return buffer
}

func releaseAudioGainBuffer(buffer *audioGainBuffer) {
	if buffer != nil {
		buffer.data = buffer.data[:0]
		audioGainBufferPool.Put(buffer)
	}
}

func (d *DualSense) handleAudioControlRequest(
	bmRequestType, bRequest uint8,
	wValue, wIndex, wLength uint16,
	data []byte,
) ([]byte, bool) {
	if response, handled := d.handleAudioFeatureControlRequest(
		bmRequestType, bRequest, wValue, wIndex, wLength, data,
	); handled {
		return response, true
	}

	return handleAudioEndpointControlRequest(
		bmRequestType, bRequest, wValue, wIndex, wLength, data,
	)
}

func (d *DualSense) handleAudioFeatureControlRequest(
	bmRequestType, bRequest uint8,
	wValue, wIndex, wLength uint16,
	data []byte,
) ([]byte, bool) {
	if uint8(wIndex) != InterfaceAudioControl || uint8(wValue) != 0 {
		return nil, false
	}

	entity := uint8(wIndex >> 8)
	selector := uint8(wValue >> 8)

	d.mtx.Lock()
	defer d.mtx.Unlock()

	var state *audioFeatureState
	switch entity {
	case audioEntitySpeakerFeatureUnit:
		state = &d.speakerAudioFeature
	case audioEntityMicrophoneFeature:
		state = &d.microphoneAudioFeature
	default:
		return nil, false
	}

	switch bmRequestType {
	case audioClassInterfaceIn:
		switch selector {
		case audioControlMute:
			if bRequest != audioClassRequestGetCurrent || wLength != 1 {
				return nil, false
			}
			if state.mute {
				return []byte{1}, true
			}
			return []byte{0}, true
		case audioControlVolume:
			if wLength != 2 {
				return nil, false
			}
			var value int16
			switch bRequest {
			case audioClassRequestGetCurrent:
				value = state.volume
			case audioClassRequestGetMinimum:
				value = state.minimum
			case audioClassRequestGetMaximum:
				value = state.maximum
			case audioClassRequestGetResolution:
				value = state.resolution
			default:
				return nil, false
			}
			return int16LittleEndian(value), true
		}
	case audioClassInterfaceOut:
		if bRequest != audioClassRequestSetCurrent {
			return nil, false
		}
		switch selector {
		case audioControlMute:
			if wLength != 1 || len(data) != 1 {
				return nil, false
			}
			state.setMute(data[0] != 0)
			return nil, true
		case audioControlVolume:
			if wLength != 2 || len(data) != 2 {
				return nil, false
			}
			state.setVolume(int16(binary.LittleEndian.Uint16(data)))
			return nil, true
		}
	}

	return nil, false
}

// handleAudioEndpointControlRequest is a compatibility path for hosts that
// probe sampling frequency despite the fixed 48 kHz format and the endpoint's
// advertised lack of a sampling-frequency control.
func handleAudioEndpointControlRequest(
	bmRequestType, bRequest uint8,
	wValue, wIndex, wLength uint16,
	data []byte,
) ([]byte, bool) {
	endpoint := uint8(wIndex)
	if uint8(wIndex>>8) != 0 ||
		(endpoint != EndpointHapticsAudioOut && endpoint != EndpointMicrophoneIn) ||
		uint8(wValue>>8) != audioControlSamplingFrequency || uint8(wValue) != 0 ||
		wLength != 3 {
		return nil, false
	}

	switch bmRequestType {
	case audioClassEndpointIn:
		switch bRequest {
		case audioClassRequestGetCurrent, audioClassRequestGetMinimum, audioClassRequestGetMaximum:
			return []byte{0x80, 0xBB, 0x00}, true
		case audioClassRequestGetResolution:
			return []byte{0x00, 0x00, 0x00}, true
		}
	case audioClassEndpointOut:
		if bRequest == audioClassRequestSetCurrent &&
			len(data) == 3 && data[0] == 0x80 && data[1] == 0xBB && data[2] == 0x00 {
			return nil, true
		}
	}

	return nil, false
}

func int16LittleEndian(value int16) []byte {
	result := make([]byte, 2)
	binary.LittleEndian.PutUint16(result, uint16(value))
	return result
}
