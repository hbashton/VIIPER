package viipertypes

import (
	"encoding/json"
	"testing"
)

func TestNativeUDEDeviceRemoveRequestRejectsAmbiguousJSON(t *testing.T) {
	valid := `{"devId":"1","transport":"native-ude","nativeUde":{"deviceId":"4294967297","deviceGeneration":1,"controllerSessionId":"17","controllerInstanceId":"ROOT\\VIIPERUDE\\0000","usb20PortNumber":1,"usb30PortNumber":0}}`
	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{"canonical", valid, true},
		{"unknown top-level", valid[:len(valid)-1] + `,"extra":1}`, false},
		{"unknown nested", `{"devId":"1","transport":"native-ude","nativeUde":{"deviceId":"4294967297","deviceGeneration":1,"controllerSessionId":"17","controllerInstanceId":"ROOT\\VIIPERUDE\\0000","usb20PortNumber":1,"usb30PortNumber":0,"extra":1}}`, false},
		{"noncanonical top-level case", `{"DevId":"1","transport":"native-ude","nativeUde":{"deviceId":"4294967297","deviceGeneration":1,"controllerSessionId":"17","controllerInstanceId":"ROOT\\VIIPERUDE\\0000","usb20PortNumber":1,"usb30PortNumber":0}}`, false},
		{"noncanonical nested case", `{"devId":"1","transport":"native-ude","nativeUde":{"DeviceId":"4294967297","deviceGeneration":1,"controllerSessionId":"17","controllerInstanceId":"ROOT\\VIIPERUDE\\0000","usb20PortNumber":1,"usb30PortNumber":0}}`, false},
		{"duplicate top-level", `{"devId":"1","devId":"2","transport":"native-ude","nativeUde":null}`, false},
		{"duplicate nested", `{"devId":"1","transport":"native-ude","nativeUde":{"deviceId":"4294967297","deviceId":"4294967298","deviceGeneration":1,"controllerSessionId":"17","controllerInstanceId":"ROOT\\VIIPERUDE\\0000","usb20PortNumber":1,"usb30PortNumber":0}}`, false},
		{"trailing value", valid + `{}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var request NativeUDEDeviceRemoveRequest
			err := json.Unmarshal([]byte(test.payload), &request)
			if test.valid && err != nil {
				t.Fatalf("canonical request rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("ambiguous request accepted")
			}
		})
	}
}
