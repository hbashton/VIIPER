package udecx_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/keyboard"
	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	"github.com/Alia5/VIIPER/usb"
)

func TestSnapshotDeviceCoversEveryProductionControllerTopology(t *testing.T) {
	tests := []struct {
		name string
		new  func() (usb.Device, error)
	}{
		{"Xbox360", func() (usb.Device, error) { return xbox360.New(nil) }},
		{"DualShock4", func() (usb.Device, error) { return dualshock4.New(nil) }},
		{"DualSense", func() (usb.Device, error) { return dualsense.New(nil) }},
		{"DualSenseEdge", func() (usb.Device, error) { return dualsense.NewEdge(nil) }},
		{"Switch2Pro", func() (usb.Device, error) { return ns2pro.New(nil) }},
		{"Keyboard", func() (usb.Device, error) { return keyboard.New(nil) }},
		{"Mouse", func() (usb.Device, error) { return mouse.New(nil) }},
	}

	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dev, err := tc.new()
			if err != nil {
				t.Fatal(err)
			}
			desc := dev.GetDescriptor()
			if desc == nil || len(desc.Interfaces) == 0 {
				t.Fatal("production controller has no USB interfaces")
			}
			snapshot, err := udecx.SnapshotDevice(uint64(index+1), 1, dev)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := snapshot.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			if got := binary.LittleEndian.Uint32(raw[8:12]); got != uint32(len(raw)) {
				t.Fatalf("native create size=%d want=%d", got, len(raw))
			}

			configuration, err := desc.ConfigurationBytes()
			if err != nil {
				t.Fatal(err)
			}
			var nativeConfiguration []byte
			for _, record := range snapshot.Descriptors {
				if record.Kind == udecx.DescriptorConfiguration {
					nativeConfiguration = snapshot.DescriptorData[record.Offset : record.Offset+record.Length]
					break
				}
			}
			if desc.Device.Speed >= uint32(udecx.DeviceSpeedHigh) {
				if !bytes.Equal(nativeConfiguration, configuration) {
					t.Fatal("native UDE snapshot changed a high-speed production USB topology")
				}
			} else if bytes.Equal(nativeConfiguration, configuration) {
				t.Fatal("native UDE snapshot omitted the required USBHUB3 full-speed endpoint projection")
			}
		})
	}
}
