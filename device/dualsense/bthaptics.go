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
