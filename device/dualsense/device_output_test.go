package dualsense

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

func TestDualSenseUSBOutputReportDescriptorMatchesCapture(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	report, err := dev.GetDescriptor().Interfaces[0].HID.ReportBytes()
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
	if desc.Strings[2] != "Wireless Controller" {
		t.Fatalf("unexpected DualSense product string: %q", desc.Strings[2])
	}

	report, err := desc.Interfaces[0].HID.ReportBytes()
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

	report, err := desc.Interfaces[0].HID.ReportBytes()
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
}
