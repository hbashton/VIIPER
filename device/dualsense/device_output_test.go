package dualsense

import (
	"context"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

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
