package dualsense

import (
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usb/hid"
)

var defaultDescriptor = usb.Descriptor{
	Device: usb.DeviceDescriptor{
		BcdUSB:             0x0200,
		BDeviceClass:       0x00,
		BDeviceSubClass:    0x00,
		BDeviceProtocol:    0x00,
		BMaxPacketSize0:    0x40,
		IDVendor:           DefaultVID,
		IDProduct:          DefaultPIDDS,
		BcdDevice:          0x0101,
		IManufacturer:      0x01,
		IProduct:           0x02,
		ISerialNumber:      0x00,
		BNumConfigurations: 0x01,
		Speed:              2, // Full speed
	},
	Associations: []usb.InterfaceAssociationDescriptor{
		{
			BFirstInterface:   0x01,
			BInterfaceCount:   0x02,
			BFunctionClass:    0x01, // Audio
			BFunctionSubClass: 0x01, // AudioControl
			BFunctionProtocol: 0x00,
			IFunction:         0x00,
		},
	},
	Interfaces: []usb.InterfaceConfig{
		{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber:   0x00,
				BAlternateSetting:  0x00,
				BNumEndpoints:      0x02,
				BInterfaceClass:    0x03, // HID
				BInterfaceSubClass: 0x00,
				BInterfaceProtocol: 0x00,
				IInterface:         0x00,
			},
			HID: &usb.HIDFunction{
				Descriptor: usb.HIDDescriptor{
					BcdHID:       0x0111,
					BCountryCode: 0x00,
					Descriptors: []usb.HIDSubDescriptor{
						{Type: usb.ReportDescType},
					},
				},
				ReportDescriptor: hid.ReportDescriptor{Items: []hid.Item{
					hid.UsagePage{Page: hid.UsagePageGenericDesktop},
					hid.Usage{Usage: hid.UsageGamePad},
					hid.Collection{
						Kind: hid.CollectionApplication,
						Items: []hid.Item{
							hid.ReportID{ID: ReportIDInput},
							hid.Usage{Usage: hid.UsageX},
							hid.Usage{Usage: hid.UsageY},
							hid.Usage{Usage: hid.UsageZ},
							hid.Usage{Usage: hid.UsageRz},
							hid.Usage{Usage: hid.UsageRx},
							hid.Usage{Usage: hid.UsageRy},
							hid.LogicalMinimum{Min: 0},
							hid.LogicalMaximum{Max: 255},
							hid.ReportSize{Bits: 8},
							hid.ReportCount{Count: 6},
							hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainAbs},

							hid.UsagePage{Page: 0xFF00},
							hid.Usage{Usage: 0x20},
							hid.ReportCount{Count: 1},
							hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainAbs},

							hid.UsagePage{Page: hid.UsagePageGenericDesktop},
							hid.Usage{Usage: 0x39},
							hid.LogicalMinimum{Min: 0},
							hid.LogicalMaximum{Max: 7},
							hid.PhysicalMinimum{Min: 0},
							hid.PhysicalMaximum{Max: 315},
							hid.Unit{Value: 0x14},
							hid.ReportSize{Bits: 4},
							hid.ReportCount{Count: 1},
							hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainNullState},
							hid.Unit{Value: 0},

							hid.UsagePage{Page: hid.UsagePageButton},
							hid.UsageMinimum{Min: 0x01},
							hid.UsageMaximum{Max: 0x0F},
							hid.LogicalMinimum{Min: 0},
							hid.LogicalMaximum{Max: 1},
							hid.ReportSize{Bits: 1},
							hid.ReportCount{Count: 15},
							hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainAbs},

							hid.UsagePage{Page: 0xFF00},
							hid.Usage{Usage: 0x21},
							hid.ReportCount{Count: 13},
							hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainAbs},

							hid.UsagePage{Page: 0xFF00},
							hid.Usage{Usage: 0x22},
							hid.LogicalMinimum{Min: 0},
							hid.LogicalMaximum{Max: 255},
							hid.ReportSize{Bits: 8},
							hid.ReportCount{Count: 52},
							hid.Input{Flags: hid.MainData | hid.MainVar | hid.MainAbs},

							hid.ReportID{ID: ReportIDOutput},
							hid.Usage{Usage: 0x23},
							hid.ReportCount{Count: OutputReportSize - 1},
							hid.Output{Flags: hid.MainData | hid.MainVar | hid.MainAbs},

							hid.ReportID{ID: featureIDCalibration}, hid.Usage{Usage: 0x33}, hid.ReportCount{Count: 40}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x08}, hid.Usage{Usage: 0x34}, hid.ReportCount{Count: 47}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: featureIDPairing}, hid.Usage{Usage: 0x24}, hid.ReportCount{Count: 19}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x0A}, hid.Usage{Usage: 0x25}, hid.ReportCount{Count: 26}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: featureIDFirmware}, hid.Usage{Usage: 0x26}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x21}, hid.Usage{Usage: 0x27}, hid.ReportCount{Count: 4}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x22}, hid.Usage{Usage: 0x40}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x80}, hid.Usage{Usage: 0x28}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x81}, hid.Usage{Usage: 0x29}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x82}, hid.Usage{Usage: 0x2A}, hid.ReportCount{Count: 9}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x83}, hid.Usage{Usage: 0x2B}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x84}, hid.Usage{Usage: 0x2C}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x85}, hid.Usage{Usage: 0x2D}, hid.ReportCount{Count: 2}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0xA0}, hid.Usage{Usage: 0x2E}, hid.ReportCount{Count: 1}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0xE0}, hid.Usage{Usage: 0x2F}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0xF0}, hid.Usage{Usage: 0x30}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0xF1}, hid.Usage{Usage: 0x31}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0xF2}, hid.Usage{Usage: 0x32}, hid.ReportCount{Count: 15}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0xF4}, hid.Usage{Usage: 0x35}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0xF5}, hid.Usage{Usage: 0x36}, hid.ReportCount{Count: 3}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},

							hid.ReportID{ID: 0x60}, hid.Usage{Usage: 0x41}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x61}, hid.Usage{Usage: 0x42}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x62}, hid.Usage{Usage: 0x43}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x63}, hid.Usage{Usage: 0x44}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x64}, hid.Usage{Usage: 0x45}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x65}, hid.Usage{Usage: 0x46}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x68}, hid.Usage{Usage: 0x47}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x70}, hid.Usage{Usage: 0x48}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x71}, hid.Usage{Usage: 0x49}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x72}, hid.Usage{Usage: 0x4A}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x73}, hid.Usage{Usage: 0x4B}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x74}, hid.Usage{Usage: 0x4C}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x75}, hid.Usage{Usage: 0x4D}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x76}, hid.Usage{Usage: 0x4E}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x77}, hid.Usage{Usage: 0x4F}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x78}, hid.Usage{Usage: 0x50}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x79}, hid.Usage{Usage: 0x51}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x7A}, hid.Usage{Usage: 0x52}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
							hid.ReportID{ID: 0x7B}, hid.Usage{Usage: 0x53}, hid.ReportCount{Count: 63}, hid.Feature{Flags: hid.MainData | hid.MainVar | hid.MainAbs},
						}},
				}},
			},
			Endpoints: []usb.EndpointDescriptor{
				{
					BEndpointAddress: EndpointIn,
					BMAttributes:     0x03, // Interrupt
					WMaxPacketSize:   64,
					BInterval:        2,
				},
				{
					BEndpointAddress: EndpointOut,
					BMAttributes:     0x03, // Interrupt
					WMaxPacketSize:   64,
					BInterval:        2,
				},
			},
		},
		{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber:   0x01,
				BAlternateSetting:  0x00,
				BNumEndpoints:      0x00,
				BInterfaceClass:    0x01, // Audio
				BInterfaceSubClass: 0x01, // AudioControl
				BInterfaceProtocol: 0x00,
				IInterface:         0x00,
			},
			ClassDescriptors: []usb.ClassSpecificDescriptor{
				{
					DescriptorType: 0x24, // CS_INTERFACE
					// UAC1 Header: subtype HEADER, ADC 1.00, total class
					// descriptor length, one streaming interface (#2).
					Payload: usb.Data{0x01, 0x00, 0x01, 0x1E, 0x00, 0x01, 0x02},
				},
				{
					DescriptorType: 0x24, // CS_INTERFACE
					// Input Terminal: USB streaming source, 4 channels.
					Payload: usb.Data{0x02, 0x01, 0x01, 0x01, 0x00, USBHapticsAudioChannels, 0x0F, 0x00, 0x00, 0x00},
				},
				{
					DescriptorType: 0x24, // CS_INTERFACE
					// Output Terminal: speaker/haptics sink, source terminal 1.
					Payload: usb.Data{0x03, 0x02, 0x01, 0x03, 0x00, 0x01, 0x00},
				},
			},
		},
		{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber:   0x02,
				BAlternateSetting:  0x00,
				BNumEndpoints:      0x00,
				BInterfaceClass:    0x01, // Audio
				BInterfaceSubClass: 0x02, // AudioStreaming
				BInterfaceProtocol: 0x00,
				IInterface:         0x00,
			},
		},
		{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber:   0x02,
				BAlternateSetting:  0x01,
				BNumEndpoints:      0x01,
				BInterfaceClass:    0x01, // Audio
				BInterfaceSubClass: 0x02, // AudioStreaming
				BInterfaceProtocol: 0x00,
				IInterface:         0x00,
			},
			ClassDescriptors: []usb.ClassSpecificDescriptor{
				{
					DescriptorType: 0x24, // CS_INTERFACE
					// AS General: terminal link 1, PCM.
					Payload: usb.Data{0x01, 0x01, 0x01, 0x00, 0x01},
				},
				{
					DescriptorType: 0x24, // CS_INTERFACE
					// Format Type I: 4-channel, 16-bit PCM, one discrete
					// sample rate = 48000 Hz. Games expect the wired
					// DualSense haptics path to look like a standard
					// 4-channel USB audio render endpoint; VIIPER downsamples
					// channels 3/4 to the SAxense 3 kHz Bluetooth HID stream.
					Payload: usb.Data{0x02, 0x01, USBHapticsAudioChannels, USBHapticsAudioBytesPerSample, 0x10, 0x01, 0x80, 0xBB, 0x00},
				},
			},
			Endpoints: []usb.EndpointDescriptor{
				{
					BEndpointAddress: EndpointHapticsAudioOut,
					BMAttributes:     0x09, // Isochronous, adaptive, data endpoint.
					WMaxPacketSize:   USBHapticsAudioPacketSize,
					BInterval:        1,
					ClassDescriptors: []usb.ClassSpecificDescriptor{
						{
							DescriptorType: 0x25, // CS_ENDPOINT
							// EP General: no sampling-frequency or pitch controls.
							Payload: usb.Data{0x01, 0x00, 0x00, 0x00, 0x00},
						},
					},
				},
			},
		},
	},
	Strings: map[uint8]string{
		0: "\u0409", // LangID: en-US (0x0409)
		1: "Sony Interactive Entertainment",
		2: "Wireless Controller",
	},
}

func makeDescriptor(edge bool) usb.Descriptor {
	desc := defaultDescriptor
	desc.Interfaces = append([]usb.InterfaceConfig(nil), defaultDescriptor.Interfaces...)
	desc.Strings = make(map[uint8]string, len(defaultDescriptor.Strings))
	for k, v := range defaultDescriptor.Strings {
		desc.Strings[k] = v
	}

	if len(desc.Interfaces) > 0 && desc.Interfaces[0].HID != nil {
		hidFunction := *desc.Interfaces[0].HID
		reportDescriptor := hidFunction.ReportDescriptor
		reportDescriptor.Items = append([]hid.Item(nil), reportDescriptor.Items...)
		for i, item := range reportDescriptor.Items {
			collection, ok := item.(hid.Collection)
			if !ok {
				continue
			}

			collection.Items = append([]hid.Item(nil), collection.Items...)
			if !edge {
				collection.Items = withoutEdgeFeatureReports(collection.Items)
			}

			reportDescriptor.Items[i] = collection
		}

		hidFunction.ReportDescriptor = reportDescriptor
		desc.Interfaces[0].HID = &hidFunction
	}

	desc.Device.IDProduct = DefaultPIDDS
	if edge {
		desc.Device.IDProduct = DefaultPIDDSEdge
		desc.Strings[2] = "DualSense Edge Wireless Controller"
	}

	return desc
}

func withoutEdgeFeatureReports(items []hid.Item) []hid.Item {
	filtered := make([]hid.Item, 0, len(items))
	for i := 0; i < len(items); i++ {
		reportID, ok := items[i].(hid.ReportID)
		if ok && isEdgeFeatureReport(reportID.ID) && i+3 < len(items) {
			if _, ok := items[i+1].(hid.Usage); ok {
				if _, ok := items[i+2].(hid.ReportCount); ok {
					if _, ok := items[i+3].(hid.Feature); ok {
						i += 3
						continue
					}
				}
			}
		}

		filtered = append(filtered, items[i])
	}

	return filtered
}

func isEdgeFeatureReport(reportID uint8) bool {
	switch reportID {
	case 0x60, 0x61, 0x62, 0x63, 0x64, 0x65,
		0x68,
		0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7A, 0x7B:
		return true
	default:
		return false
	}
}
