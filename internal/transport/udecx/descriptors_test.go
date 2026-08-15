package udecx

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/Alia5/VIIPER/usb"
)

type snapshotDevice struct{ descriptor usb.Descriptor }

func (d *snapshotDevice) HandleTransfer(context.Context, uint32, uint32, []byte) []byte {
	return nil
}
func (d *snapshotDevice) GetDescriptor() *usb.Descriptor        { return &d.descriptor }
func (d *snapshotDevice) GetDeviceSpecificArgs() map[string]any { return nil }

func TestSnapshotDevicePreservesDescriptorBytes(t *testing.T) {
	dev := &snapshotDevice{descriptor: usb.Descriptor{
		Device: usb.DeviceDescriptor{
			BcdUSB: 0x0200, BMaxPacketSize0: 64, IDVendor: 0x054c,
			IDProduct: 0x0ce6, BNumConfigurations: 1, Speed: uint32(DeviceSpeedHigh),
		},
		Interfaces: []usb.InterfaceConfig{{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber: 0, BNumEndpoints: 1, BInterfaceClass: 3,
			},
			Endpoints: []usb.EndpointDescriptor{{
				BEndpointAddress: 0x84, BMAttributes: 3, WMaxPacketSize: 64, BInterval: 4,
			}},
		}},
		Strings: map[uint8]string{0: "\u0409", 2: "Controller"},
	}}

	snapshot, err := SnapshotDevice(7, 3, dev)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DeviceID != 7 || snapshot.Generation != 3 || snapshot.Speed != DeviceSpeedHigh {
		t.Fatalf("unexpected identity: %+v", snapshot)
	}
	if len(snapshot.Descriptors) != 4 {
		t.Fatalf("descriptor count=%d want=4", len(snapshot.Descriptors))
	}
	if snapshot.Descriptors[0].Kind != DescriptorDevice || snapshot.Descriptors[1].Kind != DescriptorConfiguration ||
		snapshot.Descriptors[2].Index != 0 || snapshot.Descriptors[3].Index != 2 {
		t.Fatalf("unexpected descriptor ordering: %+v", snapshot.Descriptors)
	}
	config := snapshot.DescriptorData[snapshot.Descriptors[1].Offset : snapshot.Descriptors[1].Offset+snapshot.Descriptors[1].Length]
	if got := binary.LittleEndian.Uint16(config[2:4]); got != uint16(len(config)) {
		t.Fatalf("configuration total length=%d want=%d", got, len(config))
	}
}

func TestEndpointDescriptorForNativeUdeCxMatchesUSBHubSchedulingContract(t *testing.T) {
	tests := []struct {
		name      string
		speed     DeviceSpeed
		endpoint  usb.EndpointDescriptor
		wantMax   uint16
		wantIntvl uint8
		wantError bool
	}{
		{
			name: "full-speed ISO one frame becomes eight microframes", speed: DeviceSpeedFull,
			endpoint: usb.EndpointDescriptor{BEndpointAddress: 0x01, BMAttributes: 0x09, WMaxPacketSize: 132, BInterval: 1},
			wantMax:  132, wantIntvl: 4,
		},
		{
			name: "full-speed one millisecond interrupt", speed: DeviceSpeedFull,
			endpoint: usb.EndpointDescriptor{BEndpointAddress: 0x84, BMAttributes: 0x03, WMaxPacketSize: 64, BInterval: 1},
			wantMax:  64, wantIntvl: 4,
		},
		{
			name: "full-speed five millisecond interrupt rounds up", speed: DeviceSpeedFull,
			endpoint: usb.EndpointDescriptor{BEndpointAddress: 0x03, BMAttributes: 0x03, WMaxPacketSize: 64, BInterval: 5},
			wantMax:  64, wantIntvl: 7,
		},
		{
			name: "full-speed bulk uses USBHUB3 high-speed packet size", speed: DeviceSpeedFull,
			endpoint: usb.EndpointDescriptor{BEndpointAddress: 0x82, BMAttributes: 0x02, WMaxPacketSize: 64},
			wantMax:  512,
		},
		{
			name: "high-speed DualSense ISO is unchanged", speed: DeviceSpeedHigh,
			endpoint: usb.EndpointDescriptor{BEndpointAddress: 0x02, BMAttributes: 0x09, WMaxPacketSize: 196, BInterval: 4},
			wantMax:  196, wantIntvl: 4,
		},
		{
			name: "low-speed ISO is impossible", speed: DeviceSpeedLow,
			endpoint:  usb.EndpointDescriptor{BEndpointAddress: 0x81, BMAttributes: 0x01, WMaxPacketSize: 8, BInterval: 1},
			wantError: true,
		},
		{
			name: "Windows rejects non-one-frame full-speed ISO", speed: DeviceSpeedFull,
			endpoint:  usb.EndpointDescriptor{BEndpointAddress: 0x01, BMAttributes: 0x01, WMaxPacketSize: 32, BInterval: 2},
			wantError: true,
		},
		{
			name: "Windows rejects high-speed ISO exponent above four", speed: DeviceSpeedHigh,
			endpoint:  usb.EndpointDescriptor{BEndpointAddress: 0x81, BMAttributes: 0x01, WMaxPacketSize: 32, BInterval: 5},
			wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := tc.endpoint
			got, err := EndpointDescriptorForNativeUdeCx(tc.speed, tc.endpoint)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
			if tc.wantError {
				return
			}
			if got.WMaxPacketSize != tc.wantMax || got.BInterval != tc.wantIntvl {
				t.Fatalf("projected endpoint max/intvl=%d/%d want=%d/%d",
					got.WMaxPacketSize, got.BInterval, tc.wantMax, tc.wantIntvl)
			}
			if !reflect.DeepEqual(tc.endpoint, original) {
				t.Fatal("logical endpoint descriptor was mutated")
			}
		})
	}
}

func TestSnapshotDeviceProjectsFullSpeedEndpointScheduleWithoutMutatingDevice(t *testing.T) {
	dev := &snapshotDevice{descriptor: usb.Descriptor{
		Device: usb.DeviceDescriptor{
			BcdUSB: 0x0200, BMaxPacketSize0: 64, IDVendor: 0x054c,
			IDProduct: 0x09cc, BNumConfigurations: 1, Speed: uint32(DeviceSpeedFull),
		},
		Interfaces: []usb.InterfaceConfig{{
			Descriptor: usb.InterfaceDescriptor{BInterfaceNumber: 0, BNumEndpoints: 2},
			Endpoints: []usb.EndpointDescriptor{
				{BEndpointAddress: 0x84, BMAttributes: 0x03, WMaxPacketSize: 64, BInterval: 1},
				{BEndpointAddress: 0x01, BMAttributes: 0x09, WMaxPacketSize: 132, BInterval: 1},
			},
		}},
	}}
	original, err := dev.descriptor.ConfigurationBytes()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotDevice(9, 4, dev)
	if err != nil {
		t.Fatal(err)
	}
	config := snapshot.DescriptorData[snapshot.Descriptors[1].Offset:(snapshot.Descriptors[1].Offset + snapshot.Descriptors[1].Length)]
	// Configuration (9) + interface (9), then the two seven-byte endpoints.
	if config[18+6] != 4 || config[25+6] != 4 {
		t.Fatalf("projected endpoint intervals=%d/%d want=4/4", config[24], config[31])
	}
	after, err := dev.descriptor.ConfigurationBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("native snapshot mutated the controller's logical descriptor")
	}
}

func TestSnapshotDevicePublishesMicrosoftOS10ReservedString(t *testing.T) {
	msOS := &usb.MicrosoftOS10Descriptor{VendorCode: 0x20, CompatibleID: "WINUSB"}
	dev := &snapshotDevice{descriptor: usb.Descriptor{
		Device: usb.DeviceDescriptor{
			BcdUSB: 0x0200, BMaxPacketSize0: 64, IDVendor: 0x057e,
			IDProduct: 0x2073, BNumConfigurations: 1, Speed: uint32(DeviceSpeedHigh),
		},
		Interfaces: []usb.InterfaceConfig{{
			Descriptor: usb.InterfaceDescriptor{BInterfaceNumber: 0},
		}},
		MicrosoftOS10: msOS,
		Strings: map[uint8]string{
			0:    "\u0409",
			1:    "Nintendo",
			0xEE: "must not shadow the Microsoft descriptor",
		},
	}}

	snapshot, err := SnapshotDevice(8, 2, dev)
	if err != nil {
		t.Fatal(err)
	}
	var matches []DescriptorRecord
	for _, record := range snapshot.Descriptors {
		if record.Kind == DescriptorString && record.Index == MicrosoftOS10StringIndex {
			matches = append(matches, record)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("Microsoft OS string count=%d want=1", len(matches))
	}
	record := matches[0]
	if record.LanguageID != 0 {
		t.Fatalf("Microsoft OS string language=%#x want=0", record.LanguageID)
	}
	got := snapshot.DescriptorData[record.Offset : record.Offset+record.Length]
	want := msOS.StringDescriptor()
	if len(got) != MicrosoftOS10StringLength || MicrosoftOS10VendorCodeOffset != len(got)-2 {
		t.Fatalf("Microsoft OS string layout length=%d vendor-offset=%d", len(got), MicrosoftOS10VendorCodeOffset)
	}
	if got[MicrosoftOS10VendorCodeOffset] != msOS.EffectiveVendorCode() || got[len(got)-1] != 0 {
		t.Fatalf("Microsoft OS string vendor/pad=%#x/%#x", got[MicrosoftOS10VendorCodeOffset], got[len(got)-1])
	}
	if string(got) != string(want) {
		t.Fatalf("Microsoft OS string=%x want=%x", got, want)
	}
}
