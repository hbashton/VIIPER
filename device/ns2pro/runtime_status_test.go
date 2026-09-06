package ns2pro

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/inputpresentation"
)

func TestDecodeRuntimeStatusV1IsStrictAndComplete(t *testing.T) {
	valid := `{"version":1,"batteryLevel":0,"charging":false,` +
		`"externalPower":false,"batteryVolts":3125}`
	status, err := DecodeRuntimeStatusV1(valid)
	if err != nil {
		t.Fatalf("decode valid status: %v", err)
	}
	if status.BatteryLevel != 0 || status.Charging || status.ExternalPower ||
		status.BatteryVolts != 3125 {
		t.Fatalf("decoded status mismatch: %+v", status)
	}

	tests := []struct {
		name    string
		payload string
		want    error
	}{
		{"missing field", `{"version":1,"batteryLevel":1,"charging":false,"batteryVolts":3200}`, ErrIncompleteRuntimeStatus},
		{"wrong version", `{"version":2,"batteryLevel":1,"charging":false,"externalPower":false,"batteryVolts":3200}`, ErrInvalidRuntimeStatusVersion},
		{"level overflow", `{"version":1,"batteryLevel":10,"charging":false,"externalPower":false,"batteryVolts":3200}`, ErrInvalidRuntimeBatteryLevel},
		{"voltage low", `{"version":1,"batteryLevel":1,"charging":false,"externalPower":false,"batteryVolts":2499}`, ErrInvalidRuntimeBatteryVolts},
		{"unknown field", `{"version":1,"batteryLevel":1,"charging":false,"externalPower":false,"batteryVolts":3200,"future":1}`, nil},
		{"duplicate field", `{"version":1,"version":1,"batteryLevel":1,"charging":false,"externalPower":false,"batteryVolts":3200}`, nil},
		{"trailing value", valid + ` {}`, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeRuntimeStatusV1(test.payload)
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("error = %v, want %v", err, test.want)
				}
				return
			}
			if err == nil {
				t.Fatal("expected strict decoder rejection")
			}
		})
	}
}

func TestRuntimeStatusV1PreservesIdentityAndInputLifecycle(t *testing.T) {
	dev, err := New(&device.CreateOptions{DeviceSpecific: `{"serial_number":"SERIAL42","battery_level":9,` +
		`"charging":false,"external_power":true,"battery_volts":3800}`})
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	dev.protoMu.Lock()
	dev.usbReportsEnabled = true
	dev.protoMu.Unlock()
	state := InputState{
		Buttons: ButtonA,
		LX:      StickCenter,
		LY:      StickCenter,
		RX:      StickCenter,
		RY:      StickCenter,
	}
	if !dev.UpdateInputState(state) {
		t.Fatal("publish input state")
	}
	var selected [InputReportSize]byte
	claim := dev.ClaimInputPresentation(selected[:], time.Now())
	if !claim.Valid() {
		t.Fatal("expected active input claim")
	}
	before := dev.InputSchedulerSnapshot()

	status := RuntimeStatusV1{
		Version: RuntimeStatusVersionV1, BatteryLevel: 1,
		BatteryVolts: 3000,
	}
	if err := dev.SetRuntimeStatusV1(status); err != nil {
		t.Fatalf("set runtime status: %v", err)
	}
	after := dev.InputSchedulerSnapshot()
	if before.Generation != after.Generation || after.MandatoryNeutral ||
		after.Resynchronization {
		t.Fatalf("status update disturbed input lifecycle: before=%+v after=%+v",
			before, after)
	}
	if !dev.ResolveInputPresentation(claim, inputpresentation.OutcomeCommit,
		time.Now()) {
		t.Fatal("pre-status immutable claim did not complete")
	}

	var next [InputReportSize]byte
	if dev.BuildInputReportInto(next[:]) != InputReportSize {
		t.Fatal("build post-status report")
	}
	if next[2] != 1<<2 {
		t.Fatalf("post-status power byte = 0x%02x, want 0x04", next[2])
	}
	meta := dev.GetDeviceSpecificArgs()
	if meta["serial_number"] != "SERIAL42" {
		t.Fatalf("runtime status changed serial identity: %#v", meta)
	}
	if meta["battery_level"] != float64(1) ||
		meta["battery_volts"] != float64(3000) ||
		meta["external_power"] != false {
		t.Fatalf("runtime status was not published: %#v", meta)
	}
}

func TestCreationMetadataCanExplicitlyClearExternalPower(t *testing.T) {
	options := &device.CreateOptions{DeviceSpecific: `{"charging":false,"external_power":false}`}
	dev, err := New(options)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	encoded, err := json.Marshal(dev.GetDeviceSpecificArgs())
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var meta MetaState
	if err := json.Unmarshal(encoded, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.ExternalPower || meta.Charging {
		t.Fatalf("explicit false metadata was lost: %+v", meta)
	}
}
