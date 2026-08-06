package xboxseries

import (
	"encoding/binary"
	"strings"
)

// seriesMetadata builds the documented GIP 1.0 gamepad metadata and adds the
// Console Function Map interface used by the Series controller Share button.
func seriesMetadata() []byte {
	device := buildDeviceMetadata()
	messages := append(buildMessageMetadata(0x20, 32, 0x10), buildMessageMetadata(0x09, 9, 0x08)...)
	total := 16 + len(device) + 1 + len(messages)

	out := make([]byte, 16, total)
	put16(out, 0, 16)
	put16(out, 2, 1) // metadata major
	put16(out, 4, 0) // metadata minor
	put16(out, 14, uint16(total))
	out = append(out, device...)
	out = append(out, 2) // message count
	out = append(out, messages...)
	return out
}

func buildDeviceMetadata() []byte {
	const headerSize = 22
	firmware := []byte{1, 1, 0, 1, 0} // count; major 1, minor 1
	audio := []byte{0}
	inCommands := []byte{6, 1, 2, 3, 4, 6, 7}
	outCommands := []byte{5, 1, 4, 5, 6, 10}
	preferred := encodeStrings([]string{"Windows.Xbox.Input.Gamepad"})
	interfaces := encodeGUIDs([]string{
		// GIP requires most-specific to least-specific ordering. Keep the
		// Series Share function map ahead of the generic gamepad contracts.
		"ECDDD2FE-D387-4294-BD96-1A712E3DC77D",
		"082E402C-07DF-45E1-A5AB-A3127AF197B5",
		"B8F31FE7-7386-40E9-A9F8-2F21263ACFB7",
		"9776FF56-9BFD-4581-AD45-B645BBA526D6",
		// Official Windows USB security opt-out. Windows 10 21H2 and newer
		// succeed the exchange locally when this interface is advertised.
		"7A34CE77-7DE2-45C6-8CA4-0042C08BD94A",
	})

	sections := [][]byte{firmware, audio, inCommands, outCommands, preferred, interfaces}
	offsets := make([]uint16, len(sections))
	size := headerSize
	for i, section := range sections {
		offsets[i] = uint16(size)
		size += len(section)
	}
	out := make([]byte, headerSize, size)
	put16(out, 0, uint16(size))
	for i, offset := range offsets {
		put16(out, 2+i*2, offset)
	}
	// SupportedHidDescriptorOffset and all reserved fields remain zero.
	for _, section := range sections {
		out = append(out, section...)
	}
	return out
}

func buildMessageMetadata(messageType byte, length uint16, flags uint32) []byte {
	out := make([]byte, 23)
	put16(out, 0, 23)
	out[2] = messageType
	put16(out, 3, length)
	put16(out, 5, 1) // custom data
	binary.LittleEndian.PutUint32(out[7:11], flags)
	return out
}

func encodeStrings(values []string) []byte {
	out := []byte{byte(len(values))}
	for _, value := range values {
		b := []byte(value)
		entry := make([]byte, 2+len(b))
		put16(entry, 0, uint16(len(b)))
		copy(entry[2:], b)
		out = append(out, entry...)
	}
	return out
}

func encodeGUIDs(values []string) []byte {
	out := []byte{byte(len(values))}
	for _, value := range values {
		out = append(out, guidBytes(value)...)
	}
	return out
}

// Matches System.Guid.ToByteArray(): the first three fields are little-endian.
func guidBytes(value string) []byte {
	hex := strings.ReplaceAll(value, "-", "")
	raw := make([]byte, 16)
	for i := 0; i < 16; i++ {
		raw[i] = fromHex(hex[i*2])<<4 | fromHex(hex[i*2+1])
	}
	return []byte{raw[3], raw[2], raw[1], raw[0], raw[5], raw[4], raw[7], raw[6], raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14], raw[15]}
}

func fromHex(v byte) byte {
	if v >= '0' && v <= '9' {
		return v - '0'
	}
	if v >= 'A' && v <= 'F' {
		return v - 'A' + 10
	}
	return v - 'a' + 10
}

func put16(dst []byte, offset int, value uint16) {
	binary.LittleEndian.PutUint16(dst[offset:offset+2], value)
}
