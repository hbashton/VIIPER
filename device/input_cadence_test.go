package device_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	devicebase "github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/keyboard"
	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/device/xboxseries"
	"github.com/Alia5/VIIPER/internal/server/api"
	usbdesc "github.com/Alia5/VIIPER/usb"
	"github.com/stretchr/testify/require"
)

var identityTestSequence atomic.Uint64

func TestControllerInputDescriptorsAdvertiseOneMillisecondMaximum(t *testing.T) {
	ds4, err := dualshock4.New(nil)
	require.NoError(t, err)
	ds5, err := dualsense.New(nil)
	require.NoError(t, err)

	tests := []struct {
		name       string
		descriptor *usbdesc.Descriptor
		address    uint8
		interval   uint8
	}{
		{name: "Xbox 360", descriptor: descriptorPtr(xbox360.MakeDescriptor()), address: 0x81, interval: 1},
		{name: "Xbox Series X|S", descriptor: descriptorPtr(xboxseries.MakeDescriptor()), address: 0x81, interval: 4},
		{name: "DualShock 4", descriptor: ds4.GetDescriptor(), address: dualshock4.EndpointIn, interval: 1},
		// A high-speed interval of four is 2^(4-1) 125-us microframes = 1 ms.
		{name: "DualSense", descriptor: ds5.GetDescriptor(), address: dualsense.EndpointIn, interval: 4},
		{name: "Switch 2 Pro", descriptor: descriptorPtr(ns2pro.MakeDescriptor()), address: ns2pro.EndpointHIDIn, interval: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, iface := range test.descriptor.Interfaces {
				for _, endpoint := range iface.Endpoints {
					if endpoint.BEndpointAddress == test.address {
						require.Equal(t, test.interval, endpoint.BInterval)
						return
					}
				}
			}
			t.Fatalf("input endpoint 0x%02x not found", test.address)
		})
	}
}

func descriptorPtr(descriptor usbdesc.Descriptor) *usbdesc.Descriptor {
	return &descriptor
}

func TestConcurrentInputPublishersNeverBlock(t *testing.T) {
	xbox, err := xbox360.New(nil)
	require.NoError(t, err)
	series, err := xboxseries.New(nil)
	require.NoError(t, err)
	ds4, err := dualshock4.New(nil)
	require.NoError(t, err)
	ds5, err := dualsense.New(nil)
	require.NoError(t, err)
	switchPro, err := ns2pro.New(nil)
	require.NoError(t, err)
	keys, err := keyboard.New(nil)
	require.NoError(t, err)
	pointer, err := mouse.New(nil)
	require.NoError(t, err)

	xboxState := xbox360.InputState{}
	seriesState := xboxseries.InputState{}
	ds4State := dualshock4.InputState{}
	ds5State := dualsense.InputState{}
	switchState := ns2pro.InputState{}
	keyboardState := keyboard.InputState{}
	mouseState := mouse.InputState{}
	tests := []struct {
		name   string
		update func()
	}{
		{name: "Xbox 360", update: func() { xbox.UpdateInputState(xboxState) }},
		{name: "Xbox Series X|S", update: func() { series.UpdateInputState(seriesState) }},
		{name: "DualShock 4", update: func() { ds4.UpdateInputState(&ds4State) }},
		{name: "DualSense", update: func() { ds5.UpdateInputState(&ds5State) }},
		{name: "Switch 2 Pro", update: func() { switchPro.UpdateInputState(switchState) }},
		{name: "keyboard", update: func() { keys.UpdateInputState(keyboardState) }},
		{name: "mouse", update: func() { pointer.UpdateInputState(mouseState) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const publishers = 64
			start := make(chan struct{})
			var publishersDone sync.WaitGroup
			publishersDone.Add(publishers)
			for i := 0; i < publishers; i++ {
				go func() {
					defer publishersDone.Done()
					<-start
					test.update()
				}()
			}
			close(start)
			done := make(chan struct{})
			go func() {
				publishersDone.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("concurrent latest-state publishers blocked")
			}
		})
	}
}

func TestConcurrentControllerIdentityReservationsAreUnique(t *testing.T) {
	tests := []struct {
		name       string
		deviceType string
	}{
		{name: "DualShock 4", deviceType: "dualshock4"},
		{name: "DualSense", deviceType: dualsense.DeviceTypeCombinedAudioDuplexV5},
		{name: "Switch 2 Pro", deviceType: "ns2pro"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registration := api.GetRegistration(test.deviceType)
			require.NotNil(t, registration)
			sequence := identityTestSequence.Add(1)
			baseSerial := fmt.Sprintf("T%013X00", sequence)
			deviceSpecific := fmt.Sprintf(
				`{"serial_number":%q,"mac_address":"02:00:%02X:%02X:%02X:00"}`,
				baseSerial, byte(sequence>>16), byte(sequence>>8), byte(sequence))
			const devices = 8
			serials := make(chan string, devices)
			errors := make(chan error, devices)
			var created sync.WaitGroup
			created.Add(devices)
			for i := 0; i < devices; i++ {
				go func() {
					defer created.Done()
					controller, err := registration.CreateDevice(
						&devicebase.CreateOptions{DeviceSpecific: deviceSpecific})
					if err != nil {
						errors <- err
						return
					}
					serial, ok := controller.GetDeviceSpecificArgs()["serial_number"].(string)
					if !ok || serial == "" {
						errors <- fmt.Errorf("device did not expose a serial number")
						return
					}
					serials <- serial
				}()
			}
			created.Wait()
			close(errors)
			close(serials)
			for err := range errors {
				require.NoError(t, err)
			}
			unique := make(map[string]struct{}, devices)
			for serial := range serials {
				unique[serial] = struct{}{}
			}
			require.Len(t, unique, devices)
		})
	}
}
