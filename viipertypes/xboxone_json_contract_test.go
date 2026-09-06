package viipertypes

import (
	"encoding/json"
	"testing"
)

func TestXboxOneNamedDTOsPreserveClosedJSONContract(t *testing.T) {
	// Synthetic values exercise all fields and full-width generation identities.
	const wire = `{"version":1,"identityAuthorizationGranted":true,"identity":{"vendorId":61453,"productId":48877,"deviceReleaseBcd":513,"deviceId":18446744073709551615,"firmwareMajor":1,"firmwareMinor":2,"firmwareBuild":3,"firmwareRevision":4,"hardwareMajor":5,"hardwareMinor":6},"usb":{"maxPower2mA":250,"outIntervalMs":4,"inIntervalMs":4},"strings":{"manufacturer":"Synthetic test","product":"Test controller","serial":"fixture"},"feedback":{"source":1,"personaGeneration":9007199254740993,"deviceGeneration":18446744073709551614,"transportGeneration":3,"ownershipEpoch":4,"timeToLiveMicroseconds":500000},"importDeviceId":18446744073709551613,"localTimeoutMilliseconds":5000}`
	var request XboxOneAuthorizedCreateRequestV1
	if err := json.Unmarshal([]byte(wire), &request); err != nil {
		t.Fatal(err)
	}
	if request.Identity.DeviceID != ^uint64(0) || request.Feedback.PersonaGeneration != 9007199254740993 {
		t.Fatal("full-width identity lost")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != wire {
		t.Fatalf("JSON wire shape changed:\n%s", encoded)
	}
}
