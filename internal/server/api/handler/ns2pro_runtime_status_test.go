package handler_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/device/ns2pro"
	handlerTest "github.com/Alia5/VIIPER/internal/_testing"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/api/handler"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/virtualbus"
)

func TestBusDeviceNS2ProRuntimeStatusV1UpdatesExactDevice(t *testing.T) {
	const busID = 60322
	var controller *ns2pro.NS2Pro
	addr, _, done := handlerTest.StartAPIServer(t,
		func(r *api.Router, server *usb.Server, _ *api.Server) {
			bus, err := virtualbus.NewWithBusID(busID)
			if err != nil {
				t.Fatalf("create bus: %v", err)
			}
			if err := server.AddBus(bus); err != nil {
				t.Fatalf("add bus: %v", err)
			}
			controller, err = ns2pro.New(nil)
			if err != nil {
				t.Fatalf("create ns2pro: %v", err)
			}
			if _, err := bus.Add(controller); err != nil {
				t.Fatalf("add ns2pro: %v", err)
			}
			r.Register("bus/{busId}/{devId}/ns2pro-status-v1",
				handler.BusDeviceNS2ProRuntimeStatusV1(server))
		})
	defer done()

	client := viiperclient.NewTransport(addr)
	payload := `{"version":1,"batteryLevel":5,"charging":false,` +
		`"externalPower":false,"batteryVolts":3175}`
	line, err := client.Do("bus/{busId}/{devId}/ns2pro-status-v1", payload,
		map[string]string{"busId": "60322", "devId": "1"})
	if err != nil {
		t.Fatalf("status request: %v", err)
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["version"] != float64(1) || response["updated"] != true {
		t.Fatalf("unexpected response: %#v", response)
	}
	meta := controller.GetDeviceSpecificArgs()
	if meta["battery_level"] != float64(5) ||
		meta["battery_volts"] != float64(3175) ||
		meta["external_power"] != false {
		t.Fatalf("status did not reach exact device: %#v", meta)
	}

	bad := strings.Replace(payload, `"version":1`, `"version":2`, 1)
	line, err = client.Do("bus/{busId}/{devId}/ns2pro-status-v1", bad,
		map[string]string{"busId": "60322", "devId": "1"})
	if err != nil {
		t.Fatalf("invalid status transport: %v", err)
	}
	if !strings.Contains(line, `"status":400`) {
		t.Fatalf("invalid status was not rejected: %s", line)
	}
	meta = controller.GetDeviceSpecificArgs()
	if meta["battery_level"] != float64(5) {
		t.Fatalf("rejected status mutated device: %#v", meta)
	}
}
