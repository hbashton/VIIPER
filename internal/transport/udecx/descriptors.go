package udecx

import (
	"fmt"
	"sort"

	"github.com/Alia5/VIIPER/usb"
)

const defaultDevicePendingOperations = 512

// SnapshotDevice builds the immutable descriptor payload used to create one
// native UdeCx child. It intentionally consumes the same usb.Descriptor object
// as the existing USB/IP server so switching transports cannot silently change
// a controller's VID/PID, HID reports, audio topology, or string descriptors.
func SnapshotDevice(deviceID uint64, generation uint32, dev usb.Device) (CreateDevice, error) {
	if dev == nil || dev.GetDescriptor() == nil {
		return CreateDevice{}, fmt.Errorf("snapshot native UDE device: nil USB device")
	}
	desc := dev.GetDescriptor()
	deviceDescriptor := desc.Bytes()
	configurationDescriptor, err := desc.ConfigurationBytes()
	if err != nil {
		return CreateDevice{}, fmt.Errorf("snapshot native UDE configuration: %w", err)
	}

	message := CreateDevice{
		DeviceID:             deviceID,
		Generation:           generation,
		Speed:                DeviceSpeed(desc.Device.Speed),
		MaxPendingOperations: defaultDevicePendingOperations,
	}
	appendDescriptor := func(kind DescriptorKind, index, languageID uint16, data []byte) {
		offset := uint32(len(message.DescriptorData))
		message.DescriptorData = append(message.DescriptorData, data...)
		message.Descriptors = append(message.Descriptors, DescriptorRecord{
			Kind: kind, Index: index, LanguageID: languageID,
			Offset: offset, Length: uint32(len(data)),
		})
	}
	appendDescriptor(DescriptorDevice, 0, 0, deviceDescriptor)
	appendDescriptor(DescriptorConfiguration, 0, 0, configurationDescriptor)

	indices := make([]int, 0, len(desc.Strings))
	for index := range desc.Strings {
		indices = append(indices, int(index))
	}
	sort.Ints(indices)
	for _, value := range indices {
		index := uint8(value)
		// The Microsoft OS 1.0 descriptor owns the reserved 0xEE string
		// exactly as it does on the USB/IP control path. Never publish a
		// conflicting ordinary string at that index.
		if index == 0xEE && desc.MicrosoftOS10 != nil {
			continue
		}
		languageID := uint16(0x0409)
		if index == 0 {
			languageID = 0
		}
		appendDescriptor(
			DescriptorString, uint16(index), languageID,
			usb.EncodeStringDescriptor(desc.Strings[index]))
	}
	if desc.MicrosoftOS10 != nil {
		appendDescriptor(
			DescriptorString,
			0xEE,
			0,
			desc.MicrosoftOS10.StringDescriptor())
	}

	if _, err := message.MarshalBinary(); err != nil {
		return CreateDevice{}, fmt.Errorf("snapshot native UDE descriptors: %w", err)
	}
	return message, nil
}
