package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/server/api"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/viipertypes"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

type xboxRemovalFactoryFixture struct {
	server *serverusb.Server
	api    *api.Server
	busID  uint32
	logger *slog.Logger
}

func newXboxRemovalFactoryFixture(t *testing.T, busID uint32, connectionTimeout ...time.Duration) *xboxRemovalFactoryFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	config := serverusb.ServerConfig{Addr: "127.0.0.1:0",
		RetainedImportAuthorityID: 0x4453345758423031, BusCleanupTimeout: time.Hour}
	require.LessOrEqual(t, len(connectionTimeout), 1)
	if len(connectionTimeout) == 1 {
		config.ConnectionTimeout = connectionTimeout[0]
	}
	server := serverusb.New(config, logger, nil)
	bus, err := virtualbus.NewWithBusID(busID)
	require.NoError(t, err)
	require.NoError(t, server.AddBus(bus))
	apiServer := api.New(server, "127.0.0.1:0", api.ServerConfig{DeviceHandlerConnectTimeout: time.Hour}, logger)
	originalClock := xboxOneHostMonotonicMicroseconds
	xboxOneHostMonotonicMicroseconds = func() (uint64, bool) { return 10_000, true }
	t.Cleanup(func() {
		xboxOneHostMonotonicMicroseconds = originalClock
		if server.GetBus(busID) != nil {
			_ = server.RemoveBus(busID)
		}
		_ = server.Close()
	})
	return &xboxRemovalFactoryFixture{server: server, api: apiServer, busID: busID, logger: logger}
}

func (fixture *xboxRemovalFactoryFixture) create(t *testing.T, importID uint64) xboxOneAuthorizedCreateResponse {
	t.Helper()
	var request viipertypes.XboxOneAuthorizedCreateRequestV1
	request.Version, request.IdentityAuthorizationGranted = 1, true
	request.Identity.VendorID, request.Identity.ProductID = 0xf00d, 0xbeef
	request.Identity.DeviceReleaseBCD, request.Identity.DeviceID = 0x0102, 0x0000fffb01020304
	request.Identity.FirmwareMajor, request.Identity.FirmwareMinor = 1, 2
	request.Identity.FirmwareBuild, request.Identity.FirmwareRevision = 3, 4
	request.Identity.HardwareMajor, request.Identity.HardwareMinor = 5, 6
	request.USB.MaxPower2mA, request.USB.OUTIntervalMS, request.USB.INIntervalMS = 0x32, 4, 4
	request.Strings.Manufacturer, request.Strings.Product = "VIIPER test", "Exact removal test pad"
	request.Strings.Serial = "0000fffb01020304a1b2c3d4e5f60708"
	request.Feedback.Source = uint8(controllerfeedback.SourceXboxOneVirtualDevice)
	request.Feedback.PersonaGeneration, request.Feedback.DeviceGeneration = 1, 2
	request.Feedback.TransportGeneration, request.Feedback.OwnershipEpoch = 3, 4
	request.Feedback.TimeToLiveMicroseconds = 250_000
	request.ImportDeviceID, request.LocalTimeoutMilliseconds = importID, 100
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	response := &api.Response{}
	require.NoError(t, BusDeviceAddAuthorizedXboxOne(fixture.server, fixture.api)(
		&api.Request{Ctx: context.Background(), Authenticated: true,
			Params: map[string]string{"id": fmt.Sprint(fixture.busID)}, Payload: string(payload)}, response, fixture.logger))
	var created xboxOneAuthorizedCreateResponse
	require.NoError(t, json.Unmarshal([]byte(response.JSON), &created))
	require.True(t, xboxone.ValidProductionRemovalToken(created.RemovalToken))
	return created
}

func (fixture *xboxRemovalFactoryFixture) remove(deviceID, token string) (bool, error) {
	response := &api.Response{}
	err := BusDeviceRemoveAuthorizedXboxOne(fixture.server)(
		&api.Request{Ctx: context.Background(), Authenticated: true,
			Params:  map[string]string{"busId": fmt.Sprint(fixture.busID), "devId": deviceID},
			Payload: `{"version":1,"removalToken":"` + token + `"}`}, response, fixture.logger)
	if err != nil {
		return false, err
	}
	var removed struct {
		Version uint16 `json:"version"`
		Removed bool   `json:"removed"`
	}
	if err := json.Unmarshal([]byte(response.JSON), &removed); err != nil {
		return false, err
	}
	if removed.Version != 1 {
		return false, fmt.Errorf("invalid response version")
	}
	return removed.Removed, nil
}

func TestXboxOneRemovalRequiresAuthenticationBeforeParsing(t *testing.T) {
	err := BusDeviceRemoveAuthorizedXboxOne(nil)(&api.Request{}, &api.Response{}, nil)
	require.Equal(t, 401, err.(viipertypes.APIError).Status)
}

func TestXboxOneRemovalStrictSchemaNeverEchoesCapability(t *testing.T) {
	token := strings.Repeat("ab", 32)
	valid := `{"version":1,"removalToken":"` + token + `"}`
	for name, payload := range map[string]string{
		"missing": "", "empty": `{}`, "null": `null`, "array": `[]`,
		"truncated": valid[:len(valid)-1], "trailing": valid + `{}`,
		"trailing scalar": valid + `1`, "too large": valid + strings.Repeat(" ", 257),
		"version missing":     `{"removalToken":"` + token + `"}`,
		"version null":        `{"version":null,"removalToken":"` + token + `"}`,
		"version string":      `{"version":"1","removalToken":"` + token + `"}`,
		"version fractional":  strings.Replace(valid, `:1`, `:1.0`, 1),
		"version unsupported": strings.Replace(valid, `:1`, `:2`, 1),
		"duplicate version":   strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"duplicate token":     valid[:len(valid)-1] + `,"removalToken":"` + token + `"}`,
		"case folded version": strings.Replace(valid, `version`, `Version`, 1),
		"case folded token":   strings.Replace(valid, `removalToken`, `RemovalToken`, 1),
		"unknown key":         valid[:len(valid)-1] + `,"` + token + `":true}`,
		"token missing":       `{"version":1}`, "token null": `{"version":1,"removalToken":null}`,
		"token number":    `{"version":1,"removalToken":1}`,
		"token uppercase": strings.Replace(valid, token, strings.ToUpper(token), 1),
		"token short":     strings.Replace(valid, token, token[:63], 1),
		"token long":      strings.Replace(valid, token, token+"a", 1),
		"token nonhex":    strings.Replace(valid, token, strings.Repeat("z", 64), 1),
	} {
		t.Run(name, func(t *testing.T) {
			err := BusDeviceRemoveAuthorizedXboxOne(nil)(&api.Request{Authenticated: true,
				Params: map[string]string{"busId": "1", "devId": "1"}, Payload: payload}, &api.Response{}, nil)
			require.Error(t, err)
			require.Equal(t, 400, err.(viipertypes.APIError).Status)
			require.NotContains(t, err.Error(), token)
		})
	}
	decoded, err := decodeXboxOneRemovalRequest(" \n" + valid + " \t")
	require.NoError(t, err)
	require.Equal(t, token, decoded)
}

func TestXboxOneRemovalCreateCapabilityIsNotListed(t *testing.T) {
	fixture := newXboxRemovalFactoryFixture(t, 63002)
	first, second := fixture.create(t, 101), fixture.create(t, 102)
	require.NotEqual(t, first.RemovalToken, second.RemovalToken)
	response := &api.Response{}
	require.NoError(t, BusDevicesList(fixture.server)(&api.Request{
		Params: map[string]string{"id": "63002"}}, response, fixture.logger))
	require.NotContains(t, response.JSON, "removalToken")
	require.NotContains(t, response.JSON, first.RemovalToken)
	require.NotContains(t, response.JSON, second.RemovalToken)
	removed, err := fixture.remove(second.DevID, first.RemovalToken)
	require.NoError(t, err)
	require.False(t, removed)
	require.Len(t, fixture.server.GetBus(fixture.busID).Devices(), 2)
}

func TestXboxOneRemovalStaleAddressCannotRemoveSuccessor(t *testing.T) {
	for _, reuseBus := range []bool{false, true} {
		t.Run(fmt.Sprint("reuseBus=", reuseBus), func(t *testing.T) {
			fixture := newXboxRemovalFactoryFixture(t, 63003)
			first := fixture.create(t, 201)
			removed, err := fixture.remove(first.DevID, first.RemovalToken)
			require.NoError(t, err)
			require.True(t, removed)
			if reuseBus {
				require.NoError(t, fixture.server.RemoveBus(fixture.busID))
				bus, err := virtualbus.NewWithBusID(fixture.busID)
				require.NoError(t, err)
				require.NoError(t, fixture.server.AddBus(bus))
			}
			second := fixture.create(t, 202)
			require.Equal(t, first.DevID, second.DevID)
			require.NotEqual(t, first.RemovalToken, second.RemovalToken)
			removed, err = fixture.remove(first.DevID, first.RemovalToken)
			require.NoError(t, err)
			require.False(t, removed)
			require.Len(t, fixture.server.GetBus(fixture.busID).Devices(), 1)
			removed, err = fixture.remove(second.DevID, second.RemovalToken)
			require.NoError(t, err)
			require.True(t, removed)
			removed, err = fixture.remove(second.DevID, second.RemovalToken)
			require.NoError(t, err)
			require.False(t, removed)
		})
	}
}

func TestXboxOneRemovalConcurrentRequestsAreIdempotent(t *testing.T) {
	fixture := newXboxRemovalFactoryFixture(t, 63004)
	created := fixture.create(t, 301)
	const count = 16
	var wait sync.WaitGroup
	errs := make(chan error, count)
	for range count {
		wait.Go(func() { _, err := fixture.remove(created.DevID, created.RemovalToken); errs <- err })
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Empty(t, fixture.server.GetBus(fixture.busID).Devices())
}
