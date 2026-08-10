package udecx

import (
	"context"
	"encoding/binary"
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
