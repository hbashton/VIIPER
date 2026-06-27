package dualsense

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

func TestDualSenseUSBOutputReportDescriptorMatchesCapture(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	report, err := dev.GetDescriptor().Interfaces[5].HID.ReportBytes()
	if err != nil {
		t.Fatalf("ReportBytes returned error: %v", err)
	}

	capturedReport, err := hex.DecodeString(
		"05010905a1018501093009310932093509330934150026ff007508950681020600ff09209501810205010939150025073500463b016514750495018142650005091901290f150025017501950f81020600ff0921950d81020600ff0922150026ff0075089534810285020923952f9102850509339528b10285080934952fb102850909249513b102850a0925951ab10285200926953fb102852109279504b10285220940953fb10285800928953fb10285810929953fb1028582092a9509b1028583092b953fb1028584092c953fb1028585092d9502b10285a0092e9501b10285e0092f953fb10285f00930953fb10285f10931953fb10285f20932950fb10285f40935953fb10285f509369503b102c0")
	if err != nil {
		t.Fatalf("DecodeString returned error: %v", err)
	}
	if !bytes.Equal(report, capturedReport) {
		t.Fatalf("USB report descriptor does not match captured DualSense descriptor:\n got: % x\nwant: % x", report, capturedReport)
	}
}

func TestDualSenseDescriptorDoesNotAdvertiseEdgeFeatureReports(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	desc := dev.GetDescriptor()
	if desc.Device.IDProduct != DefaultPIDDS {
		t.Fatalf("unexpected DualSense PID: %#x", desc.Device.IDProduct)
	}
	if desc.Strings[2] != "DualSense Wireless Controller" {
		t.Fatalf("unexpected DualSense product string: %q", desc.Strings[2])
	}

	report, err := desc.Interfaces[5].HID.ReportBytes()
	if err != nil {
		t.Fatalf("ReportBytes returned error: %v", err)
	}

	edgeFeatureReport, err := hex.DecodeString("85600941953fb102")
	if err != nil {
		t.Fatalf("DecodeString returned error: %v", err)
	}
	if bytes.Contains(report, edgeFeatureReport) {
		t.Fatalf("normal DualSense descriptor advertises Edge feature report 0x60: % x", report)
	}
}

func TestDualSenseDescriptorAdvertisesExperimentalHapticsAudioEndpoint(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	desc := dev.GetDescriptor()
	if desc.Device.Speed != 3 || desc.Device.BcdDevice != 0x0100 {
		t.Fatalf("unexpected virtual USB speed/version: speed=%d bcd=%#x", desc.Device.Speed, desc.Device.BcdDevice)
	}
	if desc.Configuration.BConfigurationValue != 0x01 ||
		desc.Configuration.BMAttributes != 0xC0 ||
		desc.Configuration.BMaxPower != 0xFA {
		t.Fatalf("unexpected virtual USB configuration: %+v", desc.Configuration)
	}
	if desc.NumInterfaces() != 4 {
		t.Fatalf("unexpected interface count: got %d want 4", desc.NumInterfaces())
	}

	var foundAlt bool
	var foundEndpoint bool
	for _, iface := range desc.Interfaces {
		if iface.Descriptor.BInterfaceNumber == 1 &&
			iface.Descriptor.BAlternateSetting == 1 &&
			iface.Descriptor.BInterfaceClass == 0x01 &&
			iface.Descriptor.BInterfaceSubClass == 0x02 {
			foundAlt = true
			for _, ep := range iface.Endpoints {
				if ep.BEndpointAddress == EndpointHapticsAudioOut &&
					ep.BMAttributes&0x03 == 0x01 &&
					ep.WMaxPacketSize == USBHapticsAudioMaxPacketSize &&
					ep.BInterval == 4 &&
					bytes.Equal(ep.Trailing, []byte{0x00, 0x00}) {
					foundEndpoint = true
				}
			}
		}
	}
	if !foundAlt {
		t.Fatal("experimental haptics audio streaming altsetting was not found")
	}
	if !foundEndpoint {
		t.Fatal("experimental haptics audio OUT endpoint was not found")
	}

	var audioControlClassLength int
	var foundHeader bool
	var foundInputTerminal bool
	var foundFeatureUnit bool
	var foundOutputTerminal bool
	for _, iface := range desc.Interfaces {
		if iface.Descriptor.BInterfaceNumber != 0 || iface.Descriptor.BAlternateSetting != 0 {
			continue
		}

		for _, classDescriptor := range iface.ClassDescriptors {
			audioControlClassLength += len(classDescriptor.Bytes())
			raw := classDescriptor.Bytes()
			if len(raw) < 3 || raw[1] != 0x24 {
				continue
			}

			switch raw[2] {
			case 0x01:
				foundHeader = true
				if len(raw) < 7 || !bytes.Equal(raw[5:7], []byte{0x49, 0x00}) {
					t.Fatalf("unexpected AudioControl header descriptor: % x", raw)
				}
			case 0x02:
				if len(raw) < 4 || raw[3] != 0x01 {
					continue
				}
				foundInputTerminal = true
				if len(raw) < 10 || raw[7] != USBHapticsAudioChannels ||
					!bytes.Equal(raw[8:10], []byte{0x33, 0x00}) {
					t.Fatalf("unexpected haptics audio input terminal descriptor: % x", raw)
				}
			case 0x06:
				if len(raw) < 4 || raw[3] != 0x02 {
					continue
				}
				foundFeatureUnit = true
				if len(raw) < 6 || raw[3] != 0x02 || raw[4] != 0x01 {
					t.Fatalf("unexpected haptics audio feature unit descriptor: % x", raw)
				}
			case 0x03:
				if len(raw) < 4 || raw[3] != 0x03 {
					continue
				}
				foundOutputTerminal = true
				if len(raw) < 8 || raw[3] != 0x03 || raw[7] != 0x02 {
					t.Fatalf("unexpected haptics audio output terminal descriptor: % x", raw)
				}
			}
		}
	}
	if audioControlClassLength != 0x49 {
		t.Fatalf("unexpected AudioControl class descriptor length: got 0x%02x want 0x49", audioControlClassLength)
	}
	if !foundHeader || !foundInputTerminal || !foundFeatureUnit || !foundOutputTerminal {
		t.Fatalf("incomplete AudioControl topology: header=%t input=%t feature=%t output=%t",
			foundHeader, foundInputTerminal, foundFeatureUnit, foundOutputTerminal)
	}

	var foundFormat bool
	for _, iface := range desc.Interfaces {
		if iface.Descriptor.BInterfaceNumber != 1 || iface.Descriptor.BAlternateSetting != 1 {
			continue
		}

		for _, classDescriptor := range iface.ClassDescriptors {
			raw := classDescriptor.Bytes()
			if len(raw) == 11 && raw[2] == 0x02 {
				foundFormat = true
				if raw[4] != USBHapticsAudioChannels ||
					raw[5] != USBHapticsAudioBytesPerSample ||
					raw[6] != 0x10 ||
					!bytes.Equal(raw[8:11], []byte{0x80, 0xBB, 0x00}) {
					t.Fatalf("unexpected haptics audio format descriptor: % x", raw)
				}
			}
		}
	}
	if !foundFormat {
		t.Fatal("experimental haptics audio format descriptor was not found")
	}
}

func TestDualSenseEdgeDescriptorAdvertisesEdgeFeatureReports(t *testing.T) {
	dev, err := NewEdge(nil)
	if err != nil {
		t.Fatalf("NewEdge returned error: %v", err)
	}

	desc := dev.GetDescriptor()
	if desc.Device.IDProduct != DefaultPIDDSEdge {
		t.Fatalf("unexpected Edge PID: %#x", desc.Device.IDProduct)
	}
	if desc.Strings[2] != "DualSense Edge Wireless Controller" {
		t.Fatalf("unexpected Edge product string: %q", desc.Strings[2])
	}

	report, err := desc.Interfaces[5].HID.ReportBytes()
	if err != nil {
		t.Fatalf("ReportBytes returned error: %v", err)
	}

	edgeFeatureReport, err := hex.DecodeString("85600941953fb102")
	if err != nil {
		t.Fatalf("DecodeString returned error: %v", err)
	}
	if !bytes.Contains(report, edgeFeatureReport) {
		t.Fatalf("Edge descriptor does not advertise feature report 0x60: % x", report)
	}
}

func TestDualSenseOutputReportFromEndpoint(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	var got OutputState
	called := false
	dev.SetOutputCallback(func(out OutputState) {
		got = out
		called = true
	})

	report := make([]byte, OutputReportSize)
	report[0] = ReportIDOutput
	report[1] = 0x0F
	report[3] = 0x22
	report[4] = 0x88
	report[11] = 0x21
	report[12] = 0xFC
	report[13] = 0x03
	report[20] = 0x44
	report[22] = 0x25
	report[23] = 0x40
	report[24] = 0x05
	report[31] = 0x55

	dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, report)

	if !called {
		t.Fatal("expected output callback")
	}
	if got.RumbleSmall != 0x22 || got.RumbleLarge != 0x88 {
		t.Fatalf("unexpected rumble: small=%#x large=%#x", got.RumbleSmall, got.RumbleLarge)
	}
	if got.TriggerR2Mode != 0x21 || got.TriggerR2StartResistance != 0xFC ||
		got.TriggerR2EffectForce != 0x03 || got.TriggerR2Frequency != 0x44 {
		t.Fatalf("unexpected R2 trigger feedback: %#v", got)
	}
	if got.TriggerL2Mode != 0x25 || got.TriggerL2StartResistance != 0x40 ||
		got.TriggerL2EffectForce != 0x05 || got.TriggerL2Frequency != 0x55 {
		t.Fatalf("unexpected L2 trigger feedback: %#v", got)
	}
	if !bytes.Equal(got.RawOutputReport[:], report) {
		t.Fatalf("raw output report was not preserved:\n got: % x\nwant: % x", got.RawOutputReport, report)
	}
}

func TestDualSenseHapticsAudioOutBuildsSAxenseReports(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	var got OutputState
	dev.SetOutputCallback(func(out OutputState) {
		got = out
	})

	SetTrafficDiagnosticsEnabled(true, true)
	defer SetTrafficDiagnosticsEnabled(rawOutputLogEnabled, true)

	pcm := make([]byte, (BluetoothHapticsSampleSize/2)*USBHapticsAudioDownsample*USBHapticsAudioFrameSize)
	for outputFrame := 0; outputFrame < BluetoothHapticsSampleSize/2; outputFrame++ {
		for frame := 0; frame < USBHapticsAudioDownsample; frame++ {
			frameStart := (outputFrame*USBHapticsAudioDownsample + frame) * USBHapticsAudioFrameSize
			binary.LittleEndian.PutUint16(pcm[frameStart+4:frameStart+6], uint16(int16(outputFrame*256)))
			binary.LittleEndian.PutUint16(pcm[frameStart+6:frameStart+8], uint16(int16((outputFrame+1)*-256)))
		}
	}

	dev.HandleTransfer(context.Background(), EndpointHapticsAudioOut, usbip.DirOut, pcm)
	events := TrafficDiagnosticsSnapshot()

	var sawAudioOut bool
	var sawSAxense bool
	for _, event := range events {
		switch event.Source {
		case "audio-haptics-out":
			sawAudioOut = true
			if event.Length != len(pcm) {
				t.Fatalf("unexpected audio event length: got %d want %d", event.Length, len(pcm))
			}
		case "saxense-hid-0x32":
			sawSAxense = true
			if event.ReportID != "0x32" {
				t.Fatalf("unexpected generated report ID: %s", event.ReportID)
			}
			if event.Length != BluetoothHapticsReportSize {
				t.Fatalf("unexpected generated report length: got %d want %d", event.Length, BluetoothHapticsReportSize)
			}
		}
	}
	if !sawAudioOut {
		t.Fatal("expected audio-haptics-out diagnostic event")
	}
	if !sawSAxense {
		t.Fatal("expected generated SAxense HID 0x32 diagnostic event")
	}
	if got.BluetoothHapticsOutputReport[0] != BluetoothHapticsReportID {
		t.Fatalf("expected callback haptics report ID 0x%02x, got 0x%02x",
			BluetoothHapticsReportID,
			got.BluetoothHapticsOutputReport[0])
	}
}

func TestDualSenseCombinedHapticsAudioOutBuildsVDSReports(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	dev.combinedBluetoothFeedback = true

	var got OutputState
	dev.SetOutputCallback(func(out OutputState) {
		got = out
	})

	pcm := make([]byte, (BluetoothHapticsSampleSize/2)*USBHapticsAudioDownsample*USBHapticsAudioFrameSize)
	for outputFrame := 0; outputFrame < BluetoothHapticsSampleSize/2; outputFrame++ {
		for frame := 0; frame < USBHapticsAudioDownsample; frame++ {
			frameStart := (outputFrame*USBHapticsAudioDownsample + frame) * USBHapticsAudioFrameSize
			binary.LittleEndian.PutUint16(pcm[frameStart+4:frameStart+6], uint16(int16(outputFrame*256)))
			binary.LittleEndian.PutUint16(pcm[frameStart+6:frameStart+8], uint16(int16((outputFrame+1)*-256)))
		}
	}

	SetTrafficDiagnosticsEnabled(true, true)
	defer SetTrafficDiagnosticsEnabled(rawOutputLogEnabled, true)
	dev.HandleTransfer(context.Background(), EndpointHapticsAudioOut, usbip.DirOut, pcm)

	if got.BluetoothCombinedOutputReport[0] != BluetoothCombinedHapticsReportID {
		t.Fatalf("expected combined callback report ID 0x%02x, got 0x%02x",
			BluetoothCombinedHapticsReportID, got.BluetoothCombinedOutputReport[0])
	}
	if got.BluetoothCombinedOutputReport[11] != 0x90 ||
		got.BluetoothCombinedOutputReport[76] != 0x92 ||
		got.BluetoothCombinedOutputReport[142] != 0x93 {
		t.Fatalf("combined report did not contain vDS state, haptics, and speaker blocks: % x",
			got.BluetoothCombinedOutputReport[11:144])
	}

	var sawCombined bool
	for _, event := range TrafficDiagnosticsSnapshot() {
		if event.Source == "vds-hid-0x36" {
			sawCombined = true
			if event.ReportID != "0x36" || event.Length != BluetoothCombinedHapticsReportSize {
				t.Fatalf("unexpected combined traffic event: %#v", event)
			}
		}
	}
	if !sawCombined {
		t.Fatal("expected vds-hid-0x36 diagnostic event")
	}
}

func TestDualSenseAudioEndpointSamplingFrequencyControls(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	for _, request := range []uint8{
		audioClassRequestGetCurrent,
		audioClassRequestGetMinimum,
		audioClassRequestGetMaximum,
	} {
		got, handled := dev.HandleControl(audioClassEndpointIn, request, 0x0100, EndpointHapticsAudioOut, 3, nil)
		if !handled || !bytes.Equal(got, []byte{0x80, 0xBB, 0x00}) {
			t.Fatalf("unexpected sampling frequency response for request %#x: handled=%t response=% x", request, handled, got)
		}
	}

	got, handled := dev.HandleControl(audioClassEndpointIn, audioClassRequestGetResolution, 0x0100, EndpointHapticsAudioOut, 3, nil)
	if !handled || !bytes.Equal(got, []byte{0x00, 0x00, 0x00}) {
		t.Fatalf("unexpected sampling frequency resolution response: handled=%t response=% x", handled, got)
	}

	if _, handled = dev.HandleControl(audioClassEndpointOut, audioClassRequestSetCurrent, 0x0100, EndpointHapticsAudioOut, 3, []byte{0x80, 0xBB, 0x00}); !handled {
		t.Fatal("expected SET_CUR sampling frequency request to be accepted")
	}
}

func TestDualSenseHapticsSelectDoesNotUpdateCompatibleRumble(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	var got OutputState
	dev.SetOutputCallback(func(out OutputState) {
		got = out
	})

	rumbleReport := make([]byte, OutputReportSize)
	rumbleReport[0] = ReportIDOutput
	rumbleReport[1] = 0x01
	rumbleReport[3] = 0x22
	rumbleReport[4] = 0x88
	dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, rumbleReport)

	hapticsSelectReport := make([]byte, OutputReportSize)
	hapticsSelectReport[0] = ReportIDOutput
	hapticsSelectReport[1] = 0x02
	hapticsSelectReport[3] = 0xFF
	hapticsSelectReport[4] = 0xFF
	dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, hapticsSelectReport)

	if got.RumbleSmall != 0x22 || got.RumbleLarge != 0x88 {
		t.Fatalf("haptics-select-only report changed compatible rumble: small=%#x large=%#x", got.RumbleSmall, got.RumbleLarge)
	}
	if !bytes.Equal(got.RawOutputReport[:], hapticsSelectReport) {
		t.Fatalf("haptics-select raw report was not preserved:\n got: % x\nwant: % x", got.RawOutputReport, hapticsSelectReport)
	}
}

func TestDualSenseOutputSetReportWithoutReportId(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	var got OutputState
	called := false
	dev.SetOutputCallback(func(out OutputState) {
		got = out
		called = true
	})

	payload := make([]byte, OutputReportSize-1)
	payload[0] = 0x03
	payload[2] = 0x33
	payload[3] = 0x99

	_, handled := dev.HandleControl(hidClassOUT, hidSetReport,
		uint16(reportTypeOutput)<<8|uint16(ReportIDOutput),
		0, uint16(len(payload)), payload)

	if !handled {
		t.Fatal("expected SET_REPORT output to be handled")
	}
	if !called {
		t.Fatal("expected output callback")
	}
	if got.RumbleSmall != 0x33 || got.RumbleLarge != 0x99 {
		t.Fatalf("unexpected rumble: small=%#x large=%#x", got.RumbleSmall, got.RumbleLarge)
	}
}

func TestDualSenseOutputFlagsPreserveUnchangedTriggers(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	var got OutputState
	dev.SetOutputCallback(func(out OutputState) {
		got = out
	})

	triggerReport := make([]byte, OutputReportSize)
	triggerReport[0] = ReportIDOutput
	triggerReport[1] = 0x04
	triggerReport[11] = 0x21
	triggerReport[12] = 0xFC
	triggerReport[13] = 0x03
	triggerReport[20] = 0x44
	dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, triggerReport)

	rumbleReport := make([]byte, OutputReportSize)
	rumbleReport[0] = ReportIDOutput
	rumbleReport[1] = 0x03
	rumbleReport[3] = 0x22
	rumbleReport[4] = 0x88
	dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, rumbleReport)

	if got.RumbleSmall != 0x22 || got.RumbleLarge != 0x88 {
		t.Fatalf("unexpected rumble: small=%#x large=%#x", got.RumbleSmall, got.RumbleLarge)
	}
	if got.TriggerR2Mode != 0x21 || got.TriggerR2StartResistance != 0xFC ||
		got.TriggerR2EffectForce != 0x03 || got.TriggerR2Frequency != 0x44 {
		t.Fatalf("rumble-only report cleared R2 trigger feedback: %#v", got)
	}
}

func TestDualSenseTouchTrackingBytes(t *testing.T) {
	state := &InputState{}
	data, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}

	data[15] = 0x05
	data[20] = 0x86

	var decoded InputState
	if err := decoded.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary returned error: %v", err)
	}

	if !decoded.Touch1Active || decoded.Touch1Tracking != 0x05 {
		t.Fatalf("unexpected touch 1 status: active=%v tracking=%#x", decoded.Touch1Active, decoded.Touch1Tracking)
	}
	if decoded.Touch2Active || decoded.Touch2Tracking != 0x86 {
		t.Fatalf("unexpected touch 2 status: active=%v tracking=%#x", decoded.Touch2Active, decoded.Touch2Tracking)
	}

	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	decoded.Touch2Active = false
	report := dev.buildUSBInputReport(&decoded, &MetaState{})
	if report[33] != 0x05 {
		t.Fatalf("unexpected touch 1 report tracking byte: %#x", report[33])
	}
	if report[37] != 0x86 {
		t.Fatalf("unexpected touch 2 report tracking byte: %#x", report[37])
	}
	if report[41] == 0 {
		t.Fatal("expected touch packet counter to be populated")
	}
	if report[49] == 0x10 && report[50] == 0 && report[51] == 0 && report[52] == 0 {
		t.Fatal("unexpected legacy hard-coded status byte in report timestamp area")
	}
}

func TestDualSenseTouchTrackingZeroUsesActiveFallback(t *testing.T) {
	state := &InputState{
		Touch1Active:   true,
		Touch1Tracking: 0,
	}
	data, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}

	var decoded InputState
	if err := decoded.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary returned error: %v", err)
	}

	if !decoded.Touch1Active || decoded.Touch1Tracking != 1 {
		t.Fatalf("active touch without tracking id should use fallback: active=%v tracking=%#x", decoded.Touch1Active, decoded.Touch1Tracking)
	}

	state.Touch1Active = false
	data, err = state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	if data[15] != TouchInactiveMask {
		t.Fatalf("inactive touch should use inactive mask, got %#x", data[15])
	}
}

func TestDualSenseUSBInputReportDropsTransportMagic(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	state := NewInputState()
	state.GyroZ = int16(uint16(StreamFrameMagic0) | uint16(StreamFrameMagic1)<<8)
	state.AccelX = int16(uint16(StreamFrameMagic2) | uint16(StreamFrameMagic3)<<8)
	state.LX = 17
	state.RY = -42
	state.R2 = 200
	state.Buttons = ButtonCross

	report := dev.buildUSBInputReport(state, &MetaState{BatteryStatus: BatteryFullyCharged})
	if containsStreamMagic(report) {
		t.Fatalf("USB input report leaked transport magic: % x", report)
	}
	if dev.corruptUSBInputReports != 1 {
		t.Fatalf("expected one corrupted report reset, got %d", dev.corruptUSBInputReports)
	}
	if report[1] != 128 || report[2] != 128 || report[3] != 128 || report[4] != 128 ||
		report[5] != 0 || report[6] != 0 || report[8] != DPadUSBNeutral ||
		report[9] != 0 || report[10] != 0 {
		t.Fatalf("corrupted report was not reset to neutral controls: % x", report[:11])
	}
	if report[33] != TouchInactiveMask || report[37] != TouchInactiveMask {
		t.Fatalf("corrupted report was not reset to inactive touches: touch1=%#x touch2=%#x", report[33], report[37])
	}
	if report[53] != BatteryFullyCharged {
		t.Fatalf("neutral report should preserve battery byte, got %#x", report[53])
	}
}

func TestDualSenseExtendedFeedbackUsesNativeTriggerBlockSize(t *testing.T) {
	out := OutputState{
		RumbleSmall:              0x11,
		RumbleLarge:              0x22,
		TriggerR2Mode:            0x21,
		TriggerR2StartResistance: 0x33,
		TriggerR2PressedStrength: 0x44,
		TriggerR2Frequency:       0x55,
		TriggerL2Mode:            0x25,
		TriggerL2StartResistance: 0x66,
		TriggerL2PressedStrength: 0x77,
		TriggerL2Frequency:       0x88,
	}
	out.RawOutputReport[0] = ReportIDOutput
	out.RawOutputReport[1] = 0x02
	out.RawOutputReport[3] = 0x99

	data, err := out.MarshalExtendedBinary()
	if err != nil {
		t.Fatalf("MarshalExtendedBinary returned error: %v", err)
	}
	if len(data) != OutputStateExtSize {
		t.Fatalf("unexpected extended feedback length: %d", len(data))
	}
	if data[6] != 0x21 || data[7] != 0x33 || data[12] != 0x44 || data[15] != 0x55 {
		t.Fatalf("unexpected R2 trigger block: % x", data[6:17])
	}
	if data[17] != 0x25 || data[18] != 0x66 || data[23] != 0x77 || data[26] != 0x88 {
		t.Fatalf("unexpected L2 trigger block: % x", data[17:28])
	}

	var decoded OutputState
	if err := decoded.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary returned error: %v", err)
	}
	if decoded.TriggerR2Frequency != out.TriggerR2Frequency ||
		decoded.TriggerL2Frequency != out.TriggerL2Frequency {
		t.Fatalf("unexpected decoded frequencies: R2=%#x L2=%#x", decoded.TriggerR2Frequency, decoded.TriggerL2Frequency)
	}
	if !bytes.Equal(decoded.RawOutputReport[:], out.RawOutputReport[:]) {
		t.Fatalf("raw output report did not round-trip:\n got: % x\nwant: % x", decoded.RawOutputReport, out.RawOutputReport)
	}
}

func TestDualSenseCombinedExtendedFeedbackRoundTrips(t *testing.T) {
	out := OutputState{}
	out.RawOutputReport[0] = ReportIDOutput
	out.RawOutputReport[1] = 0x24
	out.BluetoothCombinedOutputReport[0] = BluetoothCombinedHapticsReportID
	out.BluetoothCombinedOutputReport[76] = 0x92
	out.BluetoothCombinedOutputReport[142] = 0x93

	data, err := out.MarshalCombinedExtendedBinary()
	if err != nil {
		t.Fatalf("MarshalCombinedExtendedBinary returned error: %v", err)
	}
	if len(data) != OutputStateCombinedExtSize {
		t.Fatalf("unexpected combined feedback length: got %d want %d", len(data), OutputStateCombinedExtSize)
	}

	var decoded OutputState
	if err := decoded.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary returned error: %v", err)
	}
	if !bytes.Equal(decoded.RawOutputReport[:], out.RawOutputReport[:]) {
		t.Fatalf("raw output report did not round-trip:\n got: % x\nwant: % x", decoded.RawOutputReport, out.RawOutputReport)
	}
	if !bytes.Equal(decoded.BluetoothCombinedOutputReport[:], out.BluetoothCombinedOutputReport[:]) {
		t.Fatalf("combined Bluetooth output report did not round-trip:\n got: % x\nwant: % x", decoded.BluetoothCombinedOutputReport, out.BluetoothCombinedOutputReport)
	}
}
