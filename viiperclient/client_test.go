package viiperclient_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/viipertypes"

	"github.com/stretchr/testify/assert"
)

// testClient constructs a client backed by a simple in-memory responder.
// responses maps full, already-filled paths (after path param substitution) to raw JSON payloads.
// If err is non-nil, every request returns that error, simulating dial failures.
func testClient(responses map[string]string, err error) *viiperclient.Client {
	return viiperclient.WithTransport(viiperclient.NewMockTransport(func(path string, _ any, _ map[string]string) (string, error) {
		if err != nil {
			return "", err
		}
		if out, ok := responses[path]; ok {
			return out, nil
		}
		return "", nil
	}))
}

func TestHighLevelClient(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(responses map[string]string) (err error)
		call       func(c *viiperclient.Client) (any, error)
		wantErr    string
		assertFunc func(t *testing.T, got any)
	}{
		{
			name:  "bus create success",
			setup: func(responses map[string]string) error { responses["bus/create"] = `{"busId":42}`; return nil },
			call:  func(c *viiperclient.Client) (any, error) { return c.BusCreate(42) },
			assertFunc: func(t *testing.T, got any) {
				_, ok := got.(*viipertypes.BusCreateResponse)
				assert.True(t, ok, "expected *viipertypes.BusCreateResponse type")
			},
		},
		{
			name: "bus create error structured",
			setup: func(responses map[string]string) error {
				responses["bus/create"] = `{"status":400,"title":"Bad Request","detail":"invalid busId"}`
				return nil
			},
			call:    func(c *viiperclient.Client) (any, error) { return c.BusCreate(0) },
			wantErr: "400 Bad Request: invalid busId",
		},
		{
			name: "devices list",
			setup: func(responses map[string]string) error {
				responses["bus/{id}/list"] = `{"devices":[{"busId":1,"devId":"1","vid":"0x1234","pid":"0xabcd","type":"x"}]}`
				return nil
			},
			call:       func(c *viiperclient.Client) (any, error) { return c.DevicesList(1) },
			assertFunc: func(t *testing.T, got any) { assert.NotNil(t, got) },
		},
		{
			name:    "transport failure",
			setup:   func(responses map[string]string) error { return errors.New("dial fail") },
			call:    func(c *viiperclient.Client) (any, error) { return c.BusList() },
			wantErr: "dial fail",
		},
		{
			name:    "blank response error",
			setup:   func(responses map[string]string) error { return nil },
			call:    func(c *viiperclient.Client) (any, error) { return c.BusList() },
			wantErr: "empty response",
		},
		{
			name:  "devices list empty",
			setup: func(responses map[string]string) error { responses["bus/{id}/list"] = `{"devices":[]}`; return nil },
			call:  func(c *viiperclient.Client) (any, error) { return c.DevicesList(1) },
			assertFunc: func(t *testing.T, got any) {
				resp := got.(*viipertypes.DevicesListResponse)
				assert.Len(t, resp.Devices, 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := map[string]string{}
			errInject := error(nil)
			if tt.setup != nil {
				if e := tt.setup(responses); e != nil {
					errInject = e
				}
			}
			c := testClient(responses, errInject)
			got, err := tt.call(c)
			if tt.wantErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			assert.NoError(t, err)
			if tt.assertFunc != nil {
				tt.assertFunc(t, got)
			}
		})
	}
}

func TestContextCancellation(t *testing.T) {
	c := viiperclient.WithTransport(viiperclient.NewTransport("127.0.0.1:9")) // address irrelevant due to early cancel
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.BusListCtx(ctx)
	assert.Error(t, err)
}

func TestDeviceRemoveRegisteredUsesTransportScopedAuthority(t *testing.T) {
	tests := []struct {
		name     string
		device   *viipertypes.Device
		wantPath string
		wantErr  string
	}{
		{
			name: "native exact receipt",
			device: &viipertypes.Device{
				BusID: 1, DevID: "1", Transport: "native-ude",
				NativeUDE: &viipertypes.NativeUDEDeviceInfo{
					DeviceID: "4294967297", DeviceGeneration: 2,
					ControllerSessionID: "17", ControllerInstanceID: `ROOT\VIIPERUDE\0000`,
					USB20PortNumber: 1,
				},
			},
			wantPath: "bus/{id}/remove-native",
		},
		{
			name: "usbip legacy id",
			device: &viipertypes.Device{
				BusID: 1, DevID: "1", Transport: "usbip",
			},
			wantPath: "bus/{id}/remove",
		},
		{
			name: "native missing receipt",
			device: &viipertypes.Device{
				BusID: 1, DevID: "1", Transport: "native-ude",
			},
			wantErr: "exact correlation receipt",
		},
		{
			name: "unknown transport",
			device: &viipertypes.Device{
				BusID: 1, DevID: "1", Transport: "future",
			},
			wantErr: "unsupported device transport",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotPath string
			var gotPayload any
			client := viiperclient.WithTransport(viiperclient.NewMockTransport(
				func(path string, payload any, _ map[string]string) (string, error) {
					gotPath, gotPayload = path, payload
					return `{"busId":1,"devId":"1"}`, nil
				},
			))
			_, err := client.DeviceRemoveRegistered(test.device)
			if test.wantErr != "" {
				assert.ErrorContains(t, err, test.wantErr)
				assert.Empty(t, gotPath)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, test.wantPath, gotPath)
			if test.device.Transport == "native-ude" {
				request, ok := gotPayload.(viipertypes.NativeUDEDeviceRemoveRequest)
				assert.True(t, ok)
				assert.Equal(t, test.device.DevID, request.DevID)
				assert.Equal(t, test.device.NativeUDE, request.NativeUDE)
			}
		})
	}
}
