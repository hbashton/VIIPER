package dualsense

import (
	"encoding/binary"
	"errors"
)

const (
	BluetoothHapticsReportID   = 0x32
	BluetoothHapticsReportSize = 141
	BluetoothHapticsSampleSize = 64
	BluetoothHapticsSampleRate = 3000

	// BluetoothCombinedHapticsReportID is the transport used by vDS for
	// controller audio/haptics. Unlike the legacy 0x32 packet, it carries the
	// current output state and haptics sample in one HID transaction. The fixed
	// 398-byte size is part of the DualSense Bluetooth framing.
	BluetoothCombinedHapticsReportID   = 0x36
	BluetoothCombinedHapticsReportSize = 398
	BluetoothCombinedStateSize         = 63
	BluetoothCombinedHapticsOffset     = 78
	BluetoothCombinedSpeakerOffset     = 142
	// DS5Dongle exposes the five packet-0x11 buffer fields as a 16-127
	// setting. Its default is 64, which retains a noticeably delayed haptics
	// queue when the virtual USB stream is already paced in realtime. Keep the
	// stream clock unchanged, but request the smallest documented queue from
	// the physical controller.
	BluetoothCombinedLowLatencyBufferLength = 16

	USBHapticsAudioSampleRate     = 48000
	USBHapticsAudioChannels       = 4
	USBHapticsAudioBytesPerSample = 2
	USBHapticsAudioFrameSize      = USBHapticsAudioChannels * USBHapticsAudioBytesPerSample
	USBHapticsAudioPacketFrames   = USBHapticsAudioSampleRate / 1000
	USBHapticsAudioPacketSize     = USBHapticsAudioPacketFrames * USBHapticsAudioFrameSize
	// The captured hardware descriptor advertises 392 bytes even though a
	// nominal 1 ms 48 kHz, four-channel S16 packet carries 384 bytes.
	USBHapticsAudioMaxPacketSize = 392
	USBHapticsAudioDownsample    = USBHapticsAudioSampleRate / BluetoothHapticsSampleRate

	BluetoothOutputReportID   = 0x31
	BluetoothOutputReportSize = 78

	bluetoothHapticsCRCSeed = 0xEADA2D49
)

var ErrInvalidBluetoothHapticsSample = errors.New("dualsense bluetooth haptics sample must be exactly 64 bytes")
var ErrInvalidUSBOutputReport = errors.New("dualsense USB output report must be report 0x02 with at least 48 bytes")

// defaultBluetoothCombinedState is the vDS default DualSense output state.
// The remaining 16 bytes of the 63-byte state are intentionally zero. A game
// output report replaces the first 47 bytes when one is available.
var defaultBluetoothCombinedState = [BluetoothCombinedStateSize]byte{
	0xfd, 0xf7, 0x00, 0x00, 0x7f, 0x64, 0xff, 0x09, 0x00, 0x0f, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x0a, 0x07, 0x00, 0x00, 0x02, 0x01, 0x00, 0xff, 0xd7, 0x00,
}

// BuildBluetoothHapticsReport builds the DualSense Bluetooth HID report used by
// SAxense to stream 3 kHz stereo 8-bit haptics PCM to a paired controller.
func BuildBluetoothHapticsReport(sequence uint8, intervalIndex uint8, sample []byte) ([]byte, error) {
	if len(sample) != BluetoothHapticsSampleSize {
		return nil, ErrInvalidBluetoothHapticsSample
	}

	report := make([]byte, BluetoothHapticsReportSize)
	report[0] = BluetoothHapticsReportID
	report[1] = (sequence & 0x0F) << 4

	// Packet 0x11: haptics stream control. The last byte is incremented for
	// each 64-byte PCM interval by SAxense.
	report[2] = 0x91
	report[3] = 0x07
	report[4] = 0xFE
	report[9] = 0xFF
	report[10] = intervalIndex

	// Packet 0x12: 64 bytes of signed 8-bit stereo PCM.
	report[11] = 0x92
	report[12] = BluetoothHapticsSampleSize
	copy(report[13:13+BluetoothHapticsSampleSize], sample)

	binary.LittleEndian.PutUint32(report[BluetoothHapticsReportSize-4:], dualSenseBluetoothCRC32(report[:BluetoothHapticsReportSize-4]))
	return report, nil
}

// BuildBluetoothCombinedHapticsReport builds the vDS-compatible 0x36 report.
//
// Real DualSense Bluetooth traffic combines state, haptics, and speaker data
// into this report. VIIPER owns the virtual USB haptics stream, while
// DS4Windows injects its already-encoded 200-byte Opus frame before forwarding
// the report to a physical controller. Leaving the speaker block empty here is
// intentional: fabricated Opus padding can make the controller reject the
// entire haptics packet.
func BuildBluetoothCombinedHapticsReport(sequence uint8, packetSequence uint8, sample []byte, rawOutputReport []byte) ([]byte, error) {
	if len(sample) != BluetoothHapticsSampleSize {
		return nil, ErrInvalidBluetoothHapticsSample
	}

	report := make([]byte, BluetoothCombinedHapticsReportSize)
	report[0] = BluetoothCombinedHapticsReportID
	report[1] = (sequence & 0x0F) << 4

	// Packet 0x11 starts the Bluetooth audio/haptics stream. This is the same
	// header and 64-byte interval contract used by vDS.
	report[2] = 0x91
	report[3] = 0x07
	report[4] = 0xFE
	report[5] = BluetoothCombinedLowLatencyBufferLength
	report[6] = BluetoothCombinedLowLatencyBufferLength
	report[7] = BluetoothCombinedLowLatencyBufferLength
	report[8] = BluetoothCombinedLowLatencyBufferLength
	report[9] = BluetoothCombinedLowLatencyBufferLength
	report[10] = packetSequence

	// Packet 0x10 is the 63-byte DualSense output state. Start from vDS's
	// known-good state, then retain the game's native USB effect payload.
	state := defaultBluetoothCombinedState
	if len(rawOutputReport) >= OutputReportSize && rawOutputReport[0] == ReportIDOutput {
		copy(state[:OutputReportSize-1], rawOutputReport[1:OutputReportSize])
	}
	report[11] = 0x90
	report[12] = BluetoothCombinedStateSize
	copy(report[13:13+BluetoothCombinedStateSize], state[:])

	// Packet 0x12 is the 64-byte signed 8-bit stereo haptics payload.
	report[76] = 0x92
	report[77] = BluetoothHapticsSampleSize
	copy(report[BluetoothCombinedHapticsOffset:BluetoothCombinedHapticsOffset+BluetoothHapticsSampleSize], sample)

	// Packet 0x13 is the optional 200-byte Opus speaker lane. It is explicitly
	// empty here; zero-filled bytes masquerading as Opus cause the controller to
	// reject the whole packet on some firmware revisions.
	report[BluetoothCombinedSpeakerOffset] = 0x93
	report[BluetoothCombinedSpeakerOffset+1] = 0

	binary.LittleEndian.PutUint32(report[BluetoothCombinedHapticsReportSize-4:], dualSenseBluetoothCRC32(report[:BluetoothCombinedHapticsReportSize-4]))
	return report, nil
}

// BuildBluetoothOutputReportFromUSBOutput maps a native USB DualSense output
// report 0x02 into the Bluetooth report 0x31 shape used by Sony HID-over-BT.
//
// HIDMaestro's DualSense profiles describe this as USB bytes 1-47
// ("effectPayload") shifted to Bluetooth bytes 3-49, with byte 1 carrying the
// rolling BT tag, byte 2 carrying the BT flag 0x10, and bytes 74-77 carrying a
// Sony CRC32 over prefix [0xA2, 0x31] plus bytes 1-73.
func BuildBluetoothOutputReportFromUSBOutput(sequence uint8, usbReport []byte) ([]byte, error) {
	if len(usbReport) < OutputReportSize || usbReport[0] != ReportIDOutput {
		return nil, ErrInvalidUSBOutputReport
	}

	report := make([]byte, BluetoothOutputReportSize)
	report[0] = BluetoothOutputReportID
	report[1] = (sequence & 0x0F) << 4
	report[2] = 0x10
	copy(report[3:50], usbReport[1:OutputReportSize])

	crcInput := make([]byte, 0, 2+73)
	crcInput = append(crcInput, 0xA2, BluetoothOutputReportID)
	crcInput = append(crcInput, report[1:74]...)
	binary.LittleEndian.PutUint32(report[74:78], dualSenseBluetoothCRC32(crcInput))
	return report, nil
}

func dualSenseBluetoothCRC32(data []byte) uint32 {
	crc := ^uint32(bluetoothHapticsCRCSeed)
	for _, b := range data {
		crc ^= uint32(b)
		for i := 0; i < 8; i++ {
			mask := uint32(0)
			if crc&1 != 0 {
				mask = 0xEDB88320
			}
			crc = (crc >> 1) ^ mask
		}
	}

	return ^crc
}
