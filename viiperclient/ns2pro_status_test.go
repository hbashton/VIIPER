package viiperclient_test

import (
	"encoding/json"
	"testing"

	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/viipertypes"
)

func TestUpdateNS2ProRuntimeStatusV1UsesNarrowVersionedContract(t *testing.T) {
	var gotPath string
	var gotPayload string
	var gotParams map[string]string
	transport := viiperclient.NewMockTransport(func(path string, payload any,
		params map[string]string,
	) (string, error) {
		gotPath = path
		gotPayload, _ = payload.(string)
		gotParams = params
		return `{"version":1,"updated":true}`, nil
	})
	client := viiperclient.WithTransport(transport)
	status := viipertypes.NS2ProRuntimeStatusV1{
		Version: 1, BatteryLevel: 5, BatteryVolts: 3175,
	}
	response, err := client.UpdateNS2ProRuntimeStatusV1(42, "7", status)
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if response.Version != 1 || !response.Updated {
		t.Fatalf("invalid response: %+v", response)
	}
	if gotPath != "bus/{busId}/{devId}/ns2pro-status-v1" ||
		gotParams["busId"] != "42" || gotParams["devId"] != "7" {
		t.Fatalf("wrong endpoint: %q %#v", gotPath, gotParams)
	}
	var wire map[string]any
	if err := json.Unmarshal([]byte(gotPayload), &wire); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(wire) != 5 || wire["version"] != float64(1) ||
		wire["batteryLevel"] != float64(5) ||
		wire["batteryVolts"] != float64(3175) ||
		wire["charging"] != false || wire["externalPower"] != false {
		t.Fatalf("unexpected narrow payload: %#v", wire)
	}
}

func TestUpdateNS2ProRuntimeStatusV1RejectsInvalidBeforeTransport(t *testing.T) {
	calls := 0
	client := viiperclient.WithTransport(viiperclient.NewMockTransport(
		func(string, any, map[string]string) (string, error) {
			calls++
			return `{"version":1,"updated":true}`, nil
		}))
	invalid := []viipertypes.NS2ProRuntimeStatusV1{
		{Version: 2, BatteryLevel: 5, BatteryVolts: 3175},
		{Version: 1, BatteryLevel: 10, BatteryVolts: 3175},
		{Version: 1, BatteryLevel: 5, BatteryVolts: 2499},
	}
	for _, status := range invalid {
		if _, err := client.UpdateNS2ProRuntimeStatusV1(1, "1", status); err == nil {
			t.Fatalf("accepted invalid status: %+v", status)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid status reached transport %d times", calls)
	}
}
