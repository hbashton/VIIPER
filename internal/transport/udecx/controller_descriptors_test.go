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
			if desc.Device.Speed >= uint32(udecx.DeviceSpeedHigh) {
				if !bytes.Equal(gotConfig, wantConfig) {
					t.Fatalf("native high-speed configuration changed: got=%x want=%x", gotConfig, wantConfig)
				}
			} else {
				assertFullSpeedUdeCxProjection(t, wantConfig, gotConfig)
			}
		})
	}
}

func assertFullSpeedUdeCxProjection(t *testing.T, logical, projected []byte) {
	t.Helper()
	if len(logical) != len(projected) {
		t.Fatalf("projected configuration length=%d want=%d", len(projected), len(logical))
	}
	restored := append([]byte(nil), projected...)
	for offset := 0; offset < len(logical); {
		length := int(logical[offset])
		if length < 2 || offset+length > len(logical) || projected[offset] != logical[offset] ||
			projected[offset+1] != logical[offset+1] {
			t.Fatalf("invalid or reordered descriptor at offset %d", offset)
		}
		if logical[offset+1] == usb.EndpointDescType {
			transferType := logical[offset+3] & 0x03
			logicalMax := uint16(logical[offset+4]) | uint16(logical[offset+5])<<8
			logicalInterval := logical[offset+6]
			wantMax, wantInterval := logicalMax, logicalInterval
			switch transferType {
			case 0x01:
				if logicalInterval != 1 {
					t.Fatalf("production full-speed ISO interval=%d want=1", logicalInterval)
				}
				wantInterval = 4
			case 0x02:
				wantMax = 512
			case 0x03:
				microframes := uint32(logicalInterval) * 8
				wantInterval = 1
				for period := uint32(1); wantInterval < 16 && period < microframes; period <<= 1 {
					wantInterval++
				}
			}
			gotMax := uint16(projected[offset+4]) | uint16(projected[offset+5])<<8
			if gotMax != wantMax || projected[offset+6] != wantInterval {
				t.Fatalf("endpoint %#x projected max/interval=%d/%d want=%d/%d",
					logical[offset+2], gotMax, projected[offset+6], wantMax, wantInterval)
			}
			restored[offset+4], restored[offset+5] = logical[offset+4], logical[offset+5]
			restored[offset+6] = logicalInterval
		}
		offset += length
	}
	if !bytes.Equal(restored, logical) {
		t.Fatal("native full-speed projection changed fields other than endpoint scheduling")
	}
}
