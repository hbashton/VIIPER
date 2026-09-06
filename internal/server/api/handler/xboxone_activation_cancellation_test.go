package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

func readyXboxActivationFixture(t *testing.T, busID uint32) (*xboxRemovalFactoryFixture, xboxOneAuthorizedCreateResponse) {
	t.Helper()
	fixture := newXboxRemovalFactoryFixture(t, busID)
	fixture.api.Config().AutoAttachLocalClient = true
	created := fixture.create(t, 901)
	startXboxStartupAPI(t, fixture, xboxone.ProductionStreamHandler)
	owner := openXboxStartupConn(t, fixture, created.DevID, startupPayload(created.RemovalToken), true, false, true)
	expectStartupReady(t, owner)
	previous := attachLocalhostClientWithResult
	t.Cleanup(func() { attachLocalhostClientWithResult = previous })
	return fixture, created
}

func callXboxActivation(fixture *xboxRemovalFactoryFixture, created xboxOneAuthorizedCreateResponse, ctx context.Context) (*api.Response, error) {
	response := &api.Response{}
	err := BusDeviceActivateAuthorizedXboxOne(fixture.server, fixture.api)(&api.Request{
		Ctx: ctx, Authenticated: true,
		Params:  map[string]string{"busId": fmt.Sprint(fixture.busID), "devId": created.DevID},
		Payload: startupPayload(created.RemovalToken),
	}, response, fixture.logger)
	return response, err
}

func TestXboxActivationPreCanceledRequestDoesNotConsumeReadyRegistration(t *testing.T) {
	fixture, created := readyXboxActivationFixture(t, 63207)
	attached := 0
	attachLocalhostClientWithResult = func(ctx context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.AutoAttachResult, error) {
		attached++
		require.NoError(t, ctx.Err())
		return api.AutoAttachResult{USBIPPort: 7}, nil
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{nil, canceled} {
		response, err := callXboxActivation(fixture, created, ctx)
		require.Error(t, err)
		require.Empty(t, response.JSON)
		require.Zero(t, attached)
		require.Len(t, fixture.server.GetBus(fixture.busID).Devices(), 1)
	}
	// A request rejected before reservation did not mutate the valid owner.
	response, err := callXboxActivation(fixture, created, context.Background())
	require.NoError(t, err)
	require.Contains(t, response.JSON, `"usbipPort":7`)
	require.Equal(t, 1, attached)
	require.Len(t, fixture.server.GetBus(fixture.busID).Devices(), 1,
		"the handler's own deferred context cancellation must not retire a committed activation")
}

func TestXboxActivationCancellationAfterNativeSubmissionRetiresExactRegistration(t *testing.T) {
	for _, nativeFailure := range []bool{false, true} {
		t.Run(fmt.Sprint("nativeFailure=", nativeFailure), func(t *testing.T) {
			fixture, created := readyXboxActivationFixture(t, 63208)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attached := 0
			attachLocalhostClientWithResult = func(activationCtx context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.AutoAttachResult, error) {
				attached++
				require.NotEqual(t, ctx, activationCtx, "activation owns its bounded child context")
				_, bounded := activationCtx.Deadline()
				require.True(t, bounded)
				cancel()
				require.ErrorIs(t, activationCtx.Err(), context.Canceled)
				if nativeFailure {
					return api.AutoAttachResult{}, errors.New("native cancellation completed")
				}
				return api.AutoAttachResult{USBIPPort: 7}, nil // Native success won cancellation.
			}
			response, err := callXboxActivation(fixture, created, ctx)
			require.ErrorContains(t, err, "activation canceled")
			require.Empty(t, response.JSON)
			require.Empty(t, fixture.server.GetBus(fixture.busID).Devices())
			response, err = callXboxActivation(fixture, created, context.Background())
			require.Error(t, err)
			require.Empty(t, response.JSON)
			require.Equal(t, 1, attached, "canceled receipt cannot activate again")
		})
	}
}

func TestXboxActivationConfiguredDeadlineCancelsNativeRequest(t *testing.T) {
	fixture, created := readyXboxActivationFixture(t, 63209)
	fixture.api.Config().ConnectionTimeout = 10 * time.Millisecond
	attachLocalhostClientWithResult = func(ctx context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.AutoAttachResult, error) {
		select {
		case <-ctx.Done():
			require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		case <-time.After(time.Second):
			t.Fatal("configured activation deadline was not propagated")
		}
		return api.AutoAttachResult{USBIPPort: 7}, nil
	}
	response, err := callXboxActivation(fixture, created, context.Background())
	require.ErrorContains(t, err, "activation canceled")
	require.Empty(t, response.JSON)
	require.Empty(t, fixture.server.GetBus(fixture.busID).Devices())
}

func TestXboxActivationRegistrationRemovalCancelsPendingAttachWithoutTouchingSuccessor(t *testing.T) {
	fixture, created := readyXboxActivationFixture(t, 63210)
	var successor xboxOneAuthorizedCreateResponse
	attachLocalhostClientWithResult = func(ctx context.Context, meta *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.AutoAttachResult, error) {
		alias, err := usbip.ExportBusID(*meta)
		require.NoError(t, err)
		require.Equal(t, created.USBIPBusID, alias)
		require.NoError(t, fixture.server.RemoveDeviceByID(fixture.busID, created.DevID))
		successor = fixture.create(t, 902)
		select {
		case <-ctx.Done():
			require.ErrorIs(t, ctx.Err(), context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("retired registration did not cancel native attach")
		}
		return api.AutoAttachResult{USBIPPort: 7}, nil
	}
	response, err := callXboxActivation(fixture, created, context.Background())
	require.ErrorContains(t, err, "activation canceled")
	require.Empty(t, response.JSON)
	require.Equal(t, created.DevID, successor.DevID)
	require.NotEqual(t, created.USBIPBusID, successor.USBIPBusID)
	require.Len(t, fixture.server.GetBus(fixture.busID).Devices(), 1)
	current := openXboxStartupConn(t, fixture, successor.DevID, startupPayload(successor.RemovalToken), true, false, true)
	expectStartupReady(t, current)
}

func TestXboxActivationCancellationKeepsReservationUntilNativeCompletion(t *testing.T) {
	fixture, created := readyXboxActivationFixture(t, 63211)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, observedCancellation, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpause := func() { releaseOnce.Do(func() { close(release) }) }
	defer unpause()
	var attached atomic.Int32
	attachLocalhostClientWithResult = func(ctx context.Context, _ *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.AutoAttachResult, error) {
		if attached.Add(1) != 1 {
			return api.AutoAttachResult{}, errors.New("duplicate native attach")
		}
		close(entered)
		<-ctx.Done()
		close(observedCancellation)
		<-release
		return api.AutoAttachResult{USBIPPort: 7}, nil
	}
	type completion struct {
		response *api.Response
		err      error
	}
	done := make(chan completion, 1)
	go func() { response, err := callXboxActivation(fixture, created, ctx); done <- completion{response, err} }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("attach was not submitted")
	}
	cancel()
	select {
	case <-observedCancellation:
	case <-time.After(time.Second):
		t.Fatal("attach did not observe cancellation")
	}
	response, err := callXboxActivation(fixture, created, context.Background())
	require.Error(t, err)
	require.Empty(t, response.JSON)
	require.Equal(t, int32(1), attached.Load())
	require.Len(t, fixture.server.GetBus(fixture.busID).Devices(), 1,
		"the registration remains reserved until the actual native result is known")
	unpause()
	select {
	case finished := <-done:
		require.ErrorContains(t, finished.err, "activation canceled")
		require.Empty(t, finished.response.JSON)
	case <-time.After(time.Second):
		t.Fatal("activation did not finish after native completion")
	}
	require.Empty(t, fixture.server.GetBus(fixture.busID).Devices())
}
