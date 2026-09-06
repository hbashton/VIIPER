package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/viipertypes"
	"github.com/stretchr/testify/require"
)

// This opt-in Go test executable is a peer for DS4WindowsTests, NOT a broker
// build. It never calls a native driver. Only the retained mode also starts
// an ephemeral loopback USB/IP listener with an in-process simulated host.
// Native attach is substituted; auth, routing, factory, stream and retirement
// are the production implementations. See docs/protocols/xbox-one-process-interop.md
// in DS4Windows for the pinned Desktop-only invocation.
func TestXboxDS4WindowsInteropPeer(t *testing.T) {
	if os.Getenv("DS4W_XBOX_INTEROP_PEER") != "1" {
		t.Skip("requires the explicitly enabled DS4Windows process-interop harness")
	}
	require.Equal(t, "^TestXboxDS4WindowsInteropPeer$", flag.Lookup("test.run").Value.String(),
		"the test peer requires its own process, not a shared test run")
	mode := os.Getenv("DS4W_XBOX_INTEROP_MODE")
	require.Contains(t, []string{"active", "cancel", "deadline", "retained", "retained-no-impulse",
		"retained-stop-reject", "retained-stop-ack-drop", "retained-stop-ack-timeout"}, mode)
	retained := strings.HasPrefix(mode, "retained")
	terminalFailure := strings.HasPrefix(mode, "retained-stop-")
	// Unlike the handler-only fixture, a real USB listener needs a positive
	// management deadline; its zero embedding default expires at accept.
	usbTimeout := 8 * time.Second
	if mode == "retained-stop-ack-timeout" {
		// The advertised removal budget is three USB lifecycle periods. Keep
		// that real deadline (six seconds) inside the test's ten-second wait;
		// the ordinary eight-second fixture advertises twenty-four seconds.
		usbTimeout = 2 * time.Second
	}
	fixture := newXboxRemovalFactoryFixture(t, 63212, usbTimeout)
	fixture.api.Config().Password = "synthetic-xbox-lifecycle-interop-not-a-deployment-key"
	fixture.api.Config().ConnectionTimeout = 8 * time.Second
	fixture.api.Config().AutoAttachLocalClient = true
	fixture.api.Config().AutoAttachWindowsNative = true
	if mode == "deadline" {
		fixture.api.Config().ConnectionTimeout = time.Second
	}
	var created, attached, canceled, removals, removed, removalConflicts atomic.Int32
	var busID atomic.Uint32
	var retainedHost atomic.Pointer[xboxInteropRetainedHost]
	var retainedMeta atomic.Pointer[usbip.ExportMeta]
	if retained {
		usbDone := make(chan error, 1)
		go func() { usbDone <- fixture.server.ListenAndServe() }()
		select {
		case <-fixture.server.Ready():
		case err := <-usbDone:
			t.Fatalf("isolated USB/IP listener did not start: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("isolated USB/IP listener readiness timed out")
		}
		t.Cleanup(func() {
			if host := retainedHost.Load(); host != nil {
				_ = host.conn.Close()
			}
			_ = fixture.server.Close()
			select {
			case <-usbDone:
			case <-time.After(2 * time.Second):
				t.Error("isolated USB/IP listener did not join")
			}
		})
	}
	var activationFinishedOnce sync.Once
	activationFinished := make(chan struct{})
	// Intentionally never restore native attach in this dedicated test process.
	// A late API handler during failed-test cleanup must still hit this stub,
	// never the installed driver. The exact -test.run guard above forbids sharing
	// this process with other tests which need the ordinary attach implementation.
	attachLocalhostClientWithResult = func(ctx context.Context, meta *usbip.ExportMeta, _ uint16, _ bool, _ *slog.Logger) (api.AutoAttachResult, error) {
		if attached.Add(1) != 1 {
			return api.AutoAttachResult{}, fmt.Errorf("unexpected duplicate attach in isolated test")
		}
		alias, err := usbip.ExportBusID(*meta)
		if err != nil || !usbip.ValidProductionXboxOneBusID(alias) {
			return api.AutoAttachResult{}, fmt.Errorf("test received no exact production export identity")
		}
		fmt.Println("DS4W_XBOX_INTEROP_ATTACH")
		if retained {
			metaCopy := *meta
			retainedMeta.Store(&metaCopy)
			host, err := newXboxInteropRetainedHost(fixture.server.Addr(), alias, meta)
			if err != nil {
				fmt.Printf("DS4W_XBOX_INTEROP_ERROR retained startup: %v\n", err)
				return api.AutoAttachResult{}, err
			}
			retainedHost.Store(host)
		} else if mode != "active" {
			<-ctx.Done()
			canceled.Add(1)
		}
		// Deliberately model success winning native cancellation. The handler
		// must not publish it as success after the exact registration retires.
		return api.AutoAttachResult{USBIPPort: 31070}, nil
	}
	fixture.api.Router().Register("bus/create", func(req *api.Request, res *api.Response, logger *slog.Logger) error {
		if err := BusCreate(fixture.server)(req, res, logger); err != nil {
			return err
		}
		var response viipertypes.BusCreateResponse
		if err := json.Unmarshal([]byte(res.JSON), &response); err != nil {
			return err
		}
		busID.Store(response.BusID)
		return nil
	})
	fixture.api.Router().Register("bus/{id}/add-authorized-xboxone", func(req *api.Request, res *api.Response, logger *slog.Logger) error {
		err := BusDeviceAddAuthorizedXboxOne(fixture.server, fixture.api)(req, res, logger)
		if err == nil {
			created.Add(1)
		}
		return err
	})
	fixture.api.Router().RegisterStream("bus/{busId}/{deviceid}/stream-authorized-xboxone", xboxone.ProductionStreamHandler)
	fixture.api.Router().Register("bus/{busId}/{devId}/activate-authorized-xboxone", func(req *api.Request, res *api.Response, logger *slog.Logger) error {
		defer activationFinishedOnce.Do(func() { close(activationFinished) })
		return BusDeviceActivateAuthorizedXboxOne(fixture.server, fixture.api)(req, res, logger)
	})
	fixture.api.Router().Register("bus/{busId}/{devId}/remove-authorized-xboxone", func(req *api.Request, res *api.Response, logger *slog.Logger) error {
		removals.Add(1)
		err := BusDeviceRemoveAuthorizedXboxOne(fixture.server)(req, res, logger)
		fmt.Printf("DS4W_XBOX_INTEROP_REMOVAL outcome=%s error=%v\n", res.JSON, err)
		if err == nil && res.JSON == `{"version":1,"removed":true}` {
			removed.Add(1)
		}
		if apiErr, ok := err.(viipertypes.APIError); ok && apiErr.Status == 409 {
			removalConflicts.Add(1)
		}
		return err
	})
	require.NoError(t, fixture.api.Start())
	t.Cleanup(fixture.api.Close)
	_, port, err := net.SplitHostPort(fixture.api.Addr())
	require.NoError(t, err)
	fmt.Println("DS4W_XBOX_INTEROP_READY " + port)
	command := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			command <- scanner.Text()
			if scanner.Text() == "DONE" {
				return
			}
		}
		command <- "EOF"
	}()
	if retained {
		select {
		case value := <-command:
			require.Equal(t, "INPUT", value)
		case <-time.After(10 * time.Second):
			t.Fatal("C# input was not submitted")
		}
		host := retainedHost.Load()
		require.NotNil(t, host)
		require.NoError(t, host.verifyInput(true))
		require.NoError(t, host.sendFeedback())
		fmt.Println("DS4W_XBOX_INTEROP_FEEDBACK_DONE")
		select {
		case value := <-command:
			require.Equal(t, "RELEASE", value)
		case <-time.After(10 * time.Second):
			t.Fatal("C# neutral input was not submitted")
		}
		require.NoError(t, host.submit(usbip.DirIn, 1, 64, [8]byte{}, nil))
		require.NoError(t, host.verifyInput(false))
		fmt.Println("DS4W_XBOX_INTEROP_RELEASED")
	}
	select {
	case value := <-command:
		require.Equal(t, "DONE", value)
	case <-time.After(25 * time.Second):
		t.Fatal("DS4Windows peer did not finish within the test bound")
	}
	select {
	case <-activationFinished:
	case <-time.After(2 * time.Second):
		t.Fatal("activation handler remained live after client completion")
	}
	require.Equal(t, int32(1), created.Load())
	require.Equal(t, int32(1), attached.Load())
	require.Equal(t, int32(1), removals.Load())
	if mode == "active" || retained {
		require.Zero(t, canceled.Load())
	} else {
		require.Equal(t, int32(1), canceled.Load())
	}
	if mode == "deadline" || terminalFailure {
		require.Zero(t, removed.Load(), "failed/deadline removal must not claim new safe retirement")
	} else {
		require.Equal(t, int32(1), removed.Load())
	}
	bus := fixture.server.GetBus(busID.Load())
	require.NotNil(t, bus)
	if terminalFailure {
		require.Equal(t, int32(1), removalConflicts.Load(), "failed Stop must not be reported as successful removal")
		registrations := bus.GetAllDeviceMetas()
		require.Len(t, registrations, 1, "uncertain neutral must retain the exact fenced registration")
		device, ok := registrations[0].Dev.(*xboxone.AuthorizedDormantRetainedUSBDevice)
		require.True(t, ok)
		require.False(t, device.ProductionBrokerConsumerReady())
		require.False(t, device.TryBeginProductionBrokerActivation())
		removedAgain, closeErr := fixture.server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registrations[0])
		require.False(t, removedAgain)
		require.Error(t, closeErr, "a retry cannot upgrade missing neutral proof to safe removal")
		meta := retainedMeta.Load()
		require.NotNil(t, meta)
		alias, aliasErr := usbip.ExportBusID(*meta)
		require.NoError(t, aliasErr)
		reimport, importErr := newXboxInteropRetainedHost(fixture.server.Addr(), alias, meta)
		if reimport != nil {
			_ = reimport.conn.Close()
		}
		require.Error(t, importErr, "the fenced exact alias must not admit a replacement USB import")
	} else {
		require.Zero(t, removalConflicts.Load())
		require.Empty(t, bus.Devices(), "the real production registration must be retired")
	}
	if host := retainedHost.Load(); host != nil {
		// Successful exact removal must close the retained import, not merely
		// erase a device listing while its USB connection remains alive.
		require.NoError(t, host.conn.SetReadDeadline(time.Now().Add(time.Second)))
		var extra [1]byte
		n, err := host.conn.Read(extra[:])
		require.Zero(t, n)
		require.ErrorIs(t, err, io.EOF)
	}
	fmt.Println("DS4W_XBOX_INTEROP_PASS " + mode)
}
