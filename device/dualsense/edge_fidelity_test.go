package dualsense

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Independent reference: the physical Edge USB descriptor captured by
// LtChipotle in ShadowBlip/InputPlumber, src/drivers/dualsense/report_descriptor.rs.
// Counts below exclude the report ID. Standard DualSense has different lengths.
func TestEdgeDescriptorUsesPhysicalReportLengths(t *testing.T) {
	for _, edge := range []bool{false, true} {
		for _, gamepadOnly := range []bool{false, true} {
			desc := makeDescriptor(edge)
			if gamepadOnly {
				desc = makeGamepadOnlyDescriptor(edge)
			}
			for _, iface := range desc.Interfaces {
				if iface.HID == nil {
					continue
				}
				report, err := iface.HID.ReportBytes()
				if err != nil {
					t.Fatal(err)
				}
				outputCount, featureCount := byte(47), byte(15)
				if edge {
					outputCount, featureCount = 63, 52
				}
				if !bytes.Contains(report, []byte{0x85, 0x02, 0x09, 0x23, 0x95, outputCount, 0x91, 0x02}) {
					t.Errorf("edge=%t gamepadOnly=%t: output report 0x02 has the wrong length", edge, gamepadOnly)
				}
				if !bytes.Contains(report, []byte{0x85, 0xF2, 0x09, 0x32, 0x95, featureCount, 0xB1, 0x02}) {
					t.Errorf("edge=%t gamepadOnly=%t: feature report 0xF2 has the wrong length", edge, gamepadOnly)
				}
			}
		}
	}
	// Edge construction must not mutate the shared standard-device descriptor.
	base := makeDescriptor(false)
	raw, err := base.Interfaces[5].HID.ReportBytes()
	if err != nil || !bytes.Contains(raw, []byte{0x85, 0x02, 0x09, 0x23, 0x95, 47, 0x91, 0x02}) {
		t.Fatal("Edge descriptor construction changed the standard DualSense descriptor")
	}
}

func TestEdgeAuxiliaryButtonsSurviveV5DecodeAndUSBEncoding(t *testing.T) {
	for bits := uint32(0); bits < 256; bits++ {
		if bits&8 != 0 {
			continue
		} // reserved bit is not published by DS4Windows
		var packet [InputStateSize]byte
		binary.LittleEndian.PutUint32(packet[4:8], bits<<16)
		var state InputState
		if err := state.UnmarshalBinary(packet[:]); err != nil {
			t.Fatal(err)
		}
		var report [InputReportSize]byte
		if !encodeUSBInputReportInto(&state, 0, 1, 1, 1, true, report[:]) {
			t.Fatalf("rejected valid Edge button byte %02X", bits)
		}
		if report[10] != byte(bits) || report[8] != DPadUSBNeutral {
			t.Fatalf("Edge extra-button mismatch: buttons=%02X report=% X", bits, report)
		}
	}
}

func TestEdgeDoesNotAcknowledgeUnsupportedProfileWrites(t *testing.T) {
	dev, err := NewEdge(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []byte{0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x68,
		0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7A, 0x7B} {
		payload := make([]byte, 64)
		payload[0] = id
		if _, handled := dev.HandleControl(hidClassOUT, hidSetReport,
			uint16(reportTypeFeature)<<8|uint16(id), 3, 64, payload); handled {
			t.Errorf("unimplemented Edge feature write 0x%02X reported success", id)
		}
	}
}

func TestVirtualUSBStatusPreservesHeadsetAndMuteBits(t *testing.T) {
	for _, edge := range []bool{false, true} {
		for physical := 0; physical <= 255; physical++ {
			state := InputState{PhysicalMetadataValid: true, PhysicalMetadataEdgeLayout: edge}
			state.PhysicalInputMetadata[physicalMetadataConnectState] = byte(physical)
			var report [InputReportSize]byte
			encodeUSBInputMetadata(report[:], &state, 123, edge, 0)
			want := byte(physical&7) | dualSenseUSBConnectStateWired
			if report[54] != want {
				t.Fatalf("edge=%t physical=0x%02X: virtual status=0x%02X, want 0x%02X", edge, physical, report[54], want)
			}
		}
	}
}

func TestEdgePaddedOutputPreservesBothCompleteTriggerBlocks(t *testing.T) {
	dev, err := NewEdge(nil)
	if err != nil {
		t.Fatal(err)
	}
	var got OutputState
	dev.SetOutputCallback(func(state OutputState) { got = state })
	report := make([]byte, 64)
	report[0], report[1] = ReportIDOutput, 0x0C
	for i := 11; i < 33; i++ {
		report[i] = byte(i + 37)
	}
	if !dev.handleOutputReport(report) {
		t.Fatal("valid padded Edge output was rejected")
	}
	if !bytes.Equal(got.RawOutputReport[11:33], report[11:33]) {
		t.Fatal("Edge padding handling changed the complete native trigger blocks")
	}
	if len(got.RawOutputReport) != 48 || OutputStateCombinedBluetoothOffset != 76 {
		t.Fatal("Edge USB padding changed the established V5 feedback contract")
	}
}
