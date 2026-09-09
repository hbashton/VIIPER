//go:build cgo

package main

import (
	"reflect"
	"runtime/cgo"
	"testing"

	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/internal/inputpresentation"
)

// callSetNS2ProDeviceState builds the cgo-defined arguments through reflection.
// Go does not permit import "C" directly in a _test.go file, but reflecting
// the real exported function still exercises its cgo handle conversion, state
// conversion, and returned status rather than a parallel test-only helper.
func callSetNS2ProDeviceState(t *testing.T, handle cgo.Handle,
	buttons uint32,
) bool {
	t.Helper()
	function := reflect.ValueOf(SetNS2ProDeviceState)
	functionType := function.Type()
	handleValue := reflect.New(functionType.In(0)).Elem()
	handleValue.SetUint(uint64(handle))
	stateValue := reflect.New(functionType.In(1)).Elem()
	stateValue.FieldByName("Buttons").SetUint(uint64(buttons))
	stateValue.FieldByName("LX").SetUint(uint64(ns2pro.StickCenter))
	stateValue.FieldByName("LY").SetUint(uint64(ns2pro.StickCenter))
	stateValue.FieldByName("RX").SetUint(uint64(ns2pro.StickCenter))
	stateValue.FieldByName("RY").SetUint(uint64(ns2pro.StickCenter))
	result := function.Call([]reflect.Value{handleValue, stateValue})
	if len(result) != 1 || result[0].Kind() != reflect.Bool {
		t.Fatalf("unexpected SetNS2ProDeviceState result signature: %v", result)
	}
	return result[0].Bool()
}

func callSetNS2ProRuntimeStatusV1(t *testing.T, handle cgo.Handle,
	version uint16, level uint8, volts uint16,
) bool {
	t.Helper()
	function := reflect.ValueOf(SetNS2ProRuntimeStatusV1)
	functionType := function.Type()
	handleValue := reflect.New(functionType.In(0)).Elem()
	handleValue.SetUint(uint64(handle))
	statusValue := reflect.New(functionType.In(1)).Elem()
	statusValue.FieldByName("Version").SetUint(uint64(version))
	statusValue.FieldByName("BatteryLevel").SetUint(uint64(level))
	statusValue.FieldByName("BatteryVolts").SetUint(uint64(volts))
	result := function.Call([]reflect.Value{handleValue, statusValue})
	if len(result) != 1 || result[0].Kind() != reflect.Bool {
		t.Fatalf("unexpected SetNS2ProRuntimeStatusV1 result signature: %v",
			result)
	}
	return result[0].Bool()
}

func TestSetNS2ProDeviceStatePropagatesSchedulerRejection(t *testing.T) {
	dev, err := ns2pro.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	handle := cgo.NewHandle(&deviceHandleWrapper{device: dev})
	defer handle.Delete()

	for index := 0; index < inputpresentation.FixedReportTransitionCapacity; index++ {
		buttons := ns2pro.ButtonA
		if index%2 != 0 {
			buttons = ns2pro.ButtonB
		}
		if !callSetNS2ProDeviceState(t, handle, buttons) {
			t.Fatalf("state %d was rejected before journal capacity", index)
		}
	}
	if callSetNS2ProDeviceState(t, handle, ns2pro.ButtonA) {
		t.Fatal("exported C API masked fail-closed scheduler overflow as success")
	}
}

func TestSetNS2ProRuntimeStatusV1ValidatesAndUpdatesMetadata(t *testing.T) {
	dev, err := ns2pro.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	handle := cgo.NewHandle(&deviceHandleWrapper{device: dev})
	defer handle.Delete()

	if !callSetNS2ProRuntimeStatusV1(t, handle, 1, 5, 3175) {
		t.Fatal("valid exported runtime status was rejected")
	}
	meta := dev.GetDeviceSpecificArgs()
	if meta["battery_level"] != float64(5) ||
		meta["battery_volts"] != float64(3175) {
		t.Fatalf("exported runtime status was not applied: %#v", meta)
	}
	if callSetNS2ProRuntimeStatusV1(t, handle, 2, 9, 3400) {
		t.Fatal("unsupported runtime status version was accepted")
	}
	meta = dev.GetDeviceSpecificArgs()
	if meta["battery_level"] != float64(5) {
		t.Fatalf("rejected exported status mutated metadata: %#v", meta)
	}
}
