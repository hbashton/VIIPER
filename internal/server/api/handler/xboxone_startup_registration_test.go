package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/api/auth"
	pusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

func startupPayload(token string) string {
	return `{"version":1,"removalToken":"` + token + `"}`
}

func startXboxStartupAPI(t *testing.T, fixture *xboxRemovalFactoryFixture, handler api.StreamHandlerFunc) {
	t.Helper()
	fixture.api.Config().Password = "xbox-startup-test-only"
	fixture.api.Router().RegisterStream("bus/{busId}/{deviceid}", xboxone.ProductionStreamHandler)
	fixture.api.Router().RegisterStream("bus/{busId}/{deviceid}/stream-authorized-xboxone", handler)
	require.NoError(t, fixture.api.Start())
	t.Cleanup(fixture.api.Close)
}

func openXboxStartupConn(t *testing.T, fixture *xboxRemovalFactoryFixture, deviceID, payload string, authenticated, legacy, ready bool) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fixture.api.Addr(), time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	if authenticated {
		key, err := auth.DeriveKey(fixture.api.Config().Password)
		require.NoError(t, err)
		clientNonce, serverNonce, err := auth.HandleAuthHandshake(bufio.NewReader(conn), conn, key, true)
		require.NoError(t, err)
		conn, err = auth.WrapConn(conn, auth.DeriveSessionKey(key, serverNonce, clientNonce), auth.Client)
		require.NoError(t, err)
	}
	path := fmt.Sprintf("bus/%d/%s", fixture.busID, deviceID)
	if !legacy {
		path += "/stream-authorized-xboxone"
	}
	if payload != "" {
		path += " " + payload
	}
	wire := append([]byte(path), 0)
	if ready {
		wire = append(wire, xboxStartupFrame(1, 0, nil)...)
	}
	_, err = conn.Write(wire)
	require.NoError(t, err)
	return conn
}

func xboxStartupFrame(kind byte, revision uint64, payload []byte) []byte {
	wire := make([]byte, 16+len(payload))
	copy(wire, "X1BR")
	wire[4], wire[5] = 1, kind
	binary.LittleEndian.PutUint16(wire[6:], uint16(len(payload)))
	binary.LittleEndian.PutUint64(wire[8:], revision)
	copy(wire[16:], payload)
	return wire
}

func expectStartupReady(t *testing.T, conn net.Conn) {
	t.Helper()
	ack := make([]byte, 16)
	_, err := io.ReadFull(conn, ack)
	require.NoError(t, err)
	require.Equal(t, xboxStartupFrame(0x81, 0, nil), ack)
}

func expectStartupRefused(t *testing.T, conn net.Conn, token string) {
	t.Helper()
	wire, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.NotContains(t, string(wire), token)
	var response struct {
		Status int `json:"status"`
	}
	require.NoError(t, json.Unmarshal(wire, &response), "%s", wire)
	require.GreaterOrEqual(t, response.Status, 400)
}

func TestXboxStartupDispatchRequiresExactCapabilityWithoutDisplacingReadyConsumer(t *testing.T) {
	fixture := newXboxRemovalFactoryFixture(t, 63201)
	created := fixture.create(t, 801)
	startXboxStartupAPI(t, fixture, xboxone.ProductionStreamHandler)
	valid := startupPayload(created.RemovalToken)
	owner := openXboxStartupConn(t, fixture, created.DevID, valid, true, false, true)
	expectStartupReady(t, owner) // ConsumerReady was pipelined with the management line.
	for _, test := range []struct {
		name, payload         string
		authenticated, legacy bool
	}{
		{"missing", "", true, false},
		{"wrong", startupPayload(strings.Repeat("00", 32)), true, false},
		{"duplicate", strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1), true, false},
		{"case", strings.Replace(valid, "removalToken", "RemovalToken", 1), true, false},
		{"legacy numeric", valid, true, true},
		{"unauthenticated", valid, false, false},
		{"duplicate valid owner", valid, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := openXboxStartupConn(t, fixture, created.DevID, test.payload, test.authenticated, test.legacy, false)
			expectStartupRefused(t, conn, created.RemovalToken)
		})
	}
	state := make([]byte, xboxone.SemanticInputWireSize)
	require.NoError(t, xboxone.EncodeSemanticInputWireV1Into(state, xboxone.InputStateV1{A: true}))
	_, err := owner.Write(xboxStartupFrame(2, 2, state))
	require.NoError(t, err)
	ack := make([]byte, 17)
	_, err = io.ReadFull(owner, ack)
	require.NoError(t, err)
	require.Equal(t, xboxStartupFrame(0x82, 2, []byte{0}), ack,
		"original consumer must remain live and reject input before its dormant USB persona is imported")
}

func TestXboxStartupOldTokenCannotBindReusedAddress(t *testing.T) {
	fixture := newXboxRemovalFactoryFixture(t, 63202)
	old := fixture.create(t, 802)
	removed, err := fixture.remove(old.DevID, old.RemovalToken)
	require.NoError(t, err)
	require.True(t, removed)
	next := fixture.create(t, 803)
	require.Equal(t, old.DevID, next.DevID)
	startXboxStartupAPI(t, fixture, xboxone.ProductionStreamHandler)
	stale := openXboxStartupConn(t, fixture, old.DevID, startupPayload(old.RemovalToken), true, false, false)
	expectStartupRefused(t, stale, old.RemovalToken)
	current := openXboxStartupConn(t, fixture, next.DevID, startupPayload(next.RemovalToken), true, false, true)
	expectStartupReady(t, current)
}

func TestXboxStartupRevalidatesAfterDeferredHandlerDispatch(t *testing.T) {
	fixture := newXboxRemovalFactoryFixture(t, 63203)
	old := fixture.create(t, 804)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once, releaseOnce sync.Once
	unpause := func() { releaseOnce.Do(func() { close(release) }) }
	defer unpause()
	startXboxStartupAPI(t, fixture, func(conn net.Conn, dev *pusb.Device, logger *slog.Logger) error {
		paused := false
		once.Do(func() { paused = true; close(entered) })
		if paused {
			<-release
		}
		err := xboxone.ProductionStreamHandler(conn, dev, logger)
		if paused {
			finished <- err
		}
		return err
	})
	oldConn := openXboxStartupConn(t, fixture, old.DevID, startupPayload(old.RemovalToken), true, false, true)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler not dispatched")
	}
	require.NoError(t, fixture.server.RemoveDeviceByID(fixture.busID, old.DevID))
	next := fixture.create(t, 805)
	unpause()
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("stale dispatch did not retire")
	}
	wire, _ := io.ReadAll(oldConn)
	require.False(t, bytes.Contains(wire, []byte("X1BR")), "stale handler cannot acknowledge readiness")
	current := openXboxStartupConn(t, fixture, next.DevID, startupPayload(next.RemovalToken), true, false, true)
	expectStartupReady(t, current)
}

func TestXboxActivationRejectsStaleReceiptBeforeAttachAndRevalidatesAfterAttach(t *testing.T) {
	fixture := newXboxRemovalFactoryFixture(t, 63204)
	fixture.api.Config().AutoAttachLocalClient = true
	old := fixture.create(t, 806)
	startXboxStartupAPI(t, fixture, xboxone.ProductionStreamHandler)
	owner := openXboxStartupConn(t, fixture, old.DevID, startupPayload(old.RemovalToken), true, false, true)
	expectStartupReady(t, owner)
	previous := attachLocalhostClientWithResult
	t.Cleanup(func() { attachLocalhostClientWithResult = previous })
	attached := 0
	var successor xboxOneAuthorizedCreateResponse
	attachLocalhostClientWithResult = func(_ context.Context, meta *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.AutoAttachResult, error) {
		attached++
		alias, err := usbip.ExportBusID(*meta)
		require.NoError(t, err)
		require.Equal(t, old.USBIPBusID, alias)
		// This would deadlock if activation held its registration read fence
		// across attach I/O. Its captured alias must remain old throughout.
		require.NoError(t, fixture.server.RemoveDeviceByID(fixture.busID, old.DevID))
		successor = fixture.create(t, 807)
		return api.AutoAttachResult{USBIPPort: 7}, nil
	}
	call := func(token string) error {
		response := &api.Response{}
		err := BusDeviceActivateAuthorizedXboxOne(fixture.server, fixture.api)(&api.Request{
			Ctx: context.Background(), Authenticated: true,
			Params:  map[string]string{"busId": fmt.Sprint(fixture.busID), "devId": old.DevID},
			Payload: startupPayload(token),
		}, response, fixture.logger)
		require.Empty(t, response.JSON)
		return err
	}
	require.Error(t, call(strings.Repeat("00", 32)))
	require.Zero(t, attached)
	require.Error(t, call(old.RemovalToken))
	require.Equal(t, 1, attached)
	require.Equal(t, old.DevID, successor.DevID)
	require.NotEqual(t, old.USBIPBusID, successor.USBIPBusID)
	require.Error(t, call(old.RemovalToken))
	require.Equal(t, 1, attached)
	current := openXboxStartupConn(t, fixture, successor.DevID, startupPayload(successor.RemovalToken), true, false, true)
	expectStartupReady(t, current)
}

type startupPausedReadConn struct {
	net.Conn
	read, release chan struct{}
	once          sync.Once
}

func (conn *startupPausedReadConn) Read(buffer []byte) (int, error) {
	n, err := conn.Conn.Read(buffer)
	conn.once.Do(func() { close(conn.read); <-conn.release })
	return n, err
}

func (conn *startupPausedReadConn) VIIPERAuthenticated() bool {
	return conn.Conn.(interface{ VIIPERAuthenticated() bool }).VIIPERAuthenticated()
}

func (conn *startupPausedReadConn) VIIPERAuthorizeXboxOneRegistration(device *xboxone.AuthorizedDormantRetainedUSBDevice, operation func() error) error {
	return conn.Conn.(interface {
		VIIPERAuthorizeXboxOneRegistration(*xboxone.AuthorizedDormantRetainedUSBDevice, func() error) error
	}).VIIPERAuthorizeXboxOneRegistration(device, operation)
}

func TestXboxStartupRevalidatesConsumerReadyAfterBufferedRead(t *testing.T) {
	fixture := newXboxRemovalFactoryFixture(t, 63205)
	old := fixture.create(t, 808)
	read, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var dispatch, releaseOnce sync.Once
	unpause := func() { releaseOnce.Do(func() { close(release) }) }
	defer unpause()
	startXboxStartupAPI(t, fixture, func(conn net.Conn, dev *pusb.Device, logger *slog.Logger) error {
		paused := false
		dispatch.Do(func() { paused = true })
		if !paused {
			return xboxone.ProductionStreamHandler(conn, dev, logger)
		}
		err := xboxone.ProductionStreamHandler(&startupPausedReadConn{Conn: conn, read: read, release: release}, dev, logger)
		finished <- err
		return err
	})
	oldConn := openXboxStartupConn(t, fixture, old.DevID, startupPayload(old.RemovalToken), true, false, true)
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("ready frame not read")
	}
	require.NoError(t, fixture.server.RemoveDeviceByID(fixture.busID, old.DevID))
	next := fixture.create(t, 809)
	unpause()
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("stale ready did not retire")
	}
	wire, _ := io.ReadAll(oldConn)
	require.False(t, bytes.Contains(wire, []byte("X1BR")))
	current := openXboxStartupConn(t, fixture, next.DevID, startupPayload(next.RemovalToken), true, false, true)
	expectStartupReady(t, current)
}

func TestXboxActivationConsumerLossBeforeAttachReturnsCannotAcknowledgeSuccess(t *testing.T) {
	fixture := newXboxRemovalFactoryFixture(t, 63206)
	fixture.api.Config().AutoAttachLocalClient = true
	created := fixture.create(t, 810)
	startXboxStartupAPI(t, fixture, xboxone.ProductionStreamHandler)
	owner := openXboxStartupConn(t, fixture, created.DevID, startupPayload(created.RemovalToken), true, false, true)
	expectStartupReady(t, owner)
	admission, err := api.SelectAuthorizedXboxOneRegistration(fixture.server, fmt.Sprint(fixture.busID), created.DevID, startupPayload(created.RemovalToken))
	require.NoError(t, err)
	previous := attachLocalhostClientWithResult
	t.Cleanup(func() { attachLocalhostClientWithResult = previous })
	attachLocalhostClientWithResult = func(_ context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.AutoAttachResult, error) {
		require.NoError(t, owner.Close())
		require.Eventually(t, func() bool { return !admission.Device().ProductionBrokerConsumerReady() }, time.Second, time.Millisecond)
		return api.AutoAttachResult{USBIPPort: 7}, nil
	}
	response := &api.Response{}
	err = BusDeviceActivateAuthorizedXboxOne(fixture.server, fixture.api)(&api.Request{
		Ctx: context.Background(), Authenticated: true,
		Params:  map[string]string{"busId": fmt.Sprint(fixture.busID), "devId": created.DevID},
		Payload: startupPayload(created.RemovalToken),
	}, response, fixture.logger)
	require.Error(t, err)
	require.Empty(t, response.JSON)
}
