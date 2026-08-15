package udecx

import (
	"fmt"
	"sort"

	"github.com/Alia5/VIIPER/usb"
)

const defaultDevicePendingOperations = 512

// EndpointDescriptorForNativeUdeCx translates the scheduling fields which
// USBHUB3 interprets using high-speed rules even when UdeCx is told that the
// emulated device is full speed. Without this presentation translation,
// Windows rejects full-speed audio ISO
// bInterval=1 before an URB ever reaches the client driver.
//
// This is a UdeCx presentation adapter only.  The controller's logical USB
// descriptor remains unchanged, so the device engine continues to produce and
// consume the proven media payloads at its original cadence.
func EndpointDescriptorForNativeUdeCx(speed DeviceSpeed, endpoint usb.EndpointDescriptor) (usb.EndpointDescriptor, error) {
	if endpoint.BMAttributes&0x03 == 0x01 {
		switch speed {
		case DeviceSpeedLow:
			return usb.EndpointDescriptor{}, fmt.Errorf(
				"native UdeCx low-speed endpoint 0x%02x cannot be isochronous",
				endpoint.BEndpointAddress)
		case DeviceSpeedFull:
			// Windows supports full-speed isochronous endpoints only at one
			// transfer per frame. UdeCx must see the equivalent high-speed
			// exponent (eight microframes = bInterval 4).
			if endpoint.BInterval != 1 {
				return usb.EndpointDescriptor{}, fmt.Errorf(
					"native UdeCx full-speed ISO endpoint 0x%02x has unsupported bInterval %d",
					endpoint.BEndpointAddress, endpoint.BInterval)
			}
			endpoint.BInterval = 4
			return endpoint, nil
		case DeviceSpeedHigh, DeviceSpeedSuper:
			// The Windows USB stack supports HS/SS ISO periods of one, two,
			// four, or eight microframes. Larger exponents are not a safe
			// UdeCx contract.
			if endpoint.BInterval == 0 || endpoint.BInterval > 4 {
				return usb.EndpointDescriptor{}, fmt.Errorf(
					"native UdeCx high-speed ISO endpoint 0x%02x has unsupported bInterval %d",
					endpoint.BEndpointAddress, endpoint.BInterval)
			}
		}
	}
	if speed != DeviceSpeedLow && speed != DeviceSpeedFull {
		return endpoint, nil
	}

	switch endpoint.BMAttributes & 0x03 {
	case 0x02: // Bulk: USBHUB3 validates the pipe as high speed.
		endpoint.WMaxPacketSize = 512
	case 0x03: // Interrupt: milliseconds -> the next HS microframe exponent.
		if endpoint.BInterval == 0 {
			return usb.EndpointDescriptor{}, fmt.Errorf(
				"native UdeCx full-speed interrupt endpoint 0x%02x has zero bInterval",
				endpoint.BEndpointAddress)
		}
		microframes := uint32(endpoint.BInterval) * 8
		interval := uint8(1)
		period := uint32(1)
		for interval < 16 && period < microframes {
			interval++
			period <<= 1
		}
		endpoint.BInterval = interval
	}
	return endpoint, nil
}

func configurationDescriptorForNativeUdeCx(desc *usb.Descriptor) ([]byte, error) {
	projected := *desc
	projected.Interfaces = append([]usb.InterfaceConfig(nil), desc.Interfaces...)
	for interfaceIndex := range projected.Interfaces {
		logical := desc.Interfaces[interfaceIndex]
		projected.Interfaces[interfaceIndex].Endpoints = append(
			[]usb.EndpointDescriptor(nil), logical.Endpoints...)
		for endpointIndex, endpoint := range logical.Endpoints {
			adapted, err := EndpointDescriptorForNativeUdeCx(
				DeviceSpeed(desc.Device.Speed), endpoint)
			if err != nil {
				return nil, err
			}
			projected.Interfaces[interfaceIndex].Endpoints[endpointIndex] = adapted
		}
	}
	return projected.ConfigurationBytes()
}

// SnapshotDevice builds the immutable descriptor payload used to create one
// native UdeCx child. It consumes the same logical usb.Descriptor object as the
// existing USB/IP server; only the UdeCx-required full-speed endpoint schedule
// projection above may differ in the immutable native snapshot.
func SnapshotDevice(deviceID uint64, generation uint32, dev usb.Device) (CreateDevice, error) {
	if dev == nil || dev.GetDescriptor() == nil {
		return CreateDevice{}, fmt.Errorf("snapshot native UDE device: nil USB device")
	}
	desc := dev.GetDescriptor()
	deviceDescriptor := desc.Bytes()
	configurationDescriptor, err := configurationDescriptorForNativeUdeCx(desc)
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
		if uint16(index) == MicrosoftOS10StringIndex && desc.MicrosoftOS10 != nil {
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
			MicrosoftOS10StringIndex,
			0,
			desc.MicrosoftOS10.StringDescriptor())
	}

	if _, err := message.MarshalBinary(); err != nil {
		return CreateDevice{}, fmt.Errorf("snapshot native UDE descriptors: %w", err)
	}
	return message, nil
}
