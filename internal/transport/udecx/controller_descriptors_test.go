package udecx_test

import (
	"bytes"
	"testing"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	"github.com/Alia5/VIIPER/usb"
)

func TestNativeSnapshotsPreserveSupportedControllerTopologies(t *testing.T) {
	type factory func() (usb.Device, error)
	tests := map[string]factory{
		"DualSense": func() (usb.Device, error) { return dualsense.New(nil) },
		"DualSense Edge": func() (usb.Device, error) {
			return dualsense.NewEdge(nil)
		},
		"DualShock 4": func() (usb.Device, error) { return dualshock4.New(nil) },
		"Xbox 360":    func() (usb.Device, error) { return xbox360.New(nil) },
		"Switch 2 Pro": func() (usb.Device, error) {
			return ns2pro.New(nil)
		},
	}

	for name, construct := range tests {
		t.Run(name, func(t *testing.T) {
			dev, err := construct()
			if err != nil {
				t.Fatal(err)
			}
			desc := dev.GetDescriptor()
			if desc == nil {
				t.Fatal("controller returned no descriptor")
			}
			wantConfig, err := desc.ConfigurationBytes()
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := udecx.SnapshotDevice(0x100, 7, dev)
			if err != nil {
				t.Fatal(err)
			}

			var gotDevice, gotConfig []byte
			for _, record := range snapshot.Descriptors {
				payload := snapshot.DescriptorData[record.Offset : record.Offset+record.Length]
				switch record.Kind {
				case udecx.DescriptorDevice:
					gotDevice = payload
				case udecx.DescriptorConfiguration:
					gotConfig = payload
				}
			}
			if !bytes.Equal(gotDevice, desc.Bytes()) {
				t.Fatalf("native device descriptor changed: got=%x want=%x", gotDevice, desc.Bytes())
			}
			if !bytes.Equal(gotConfig, wantConfig) {
				t.Fatalf("native configuration changed: got=%x want=%x", gotConfig, wantConfig)
			}
		})
	}
}
