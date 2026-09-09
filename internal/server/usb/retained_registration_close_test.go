package usb

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/device/xboxone"
	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

func TestRetainedExactRegistrationRemovalJoinsActualProductionStop(t *testing.T) {
	requireWindowsCanonicalFeedbackClock(t)
	for _, outcome := range []string{"accepted", "rejected", "wrong correlation", "socket lost"} {
		t.Run(outcome, func(t *testing.T) {
			const authorityID, deviceID = uint64(0x9581), uint64(0x0000fffb01020304)
			device := newProductionRetirementIntegrationDevice(t, authorityID, deviceID)
			brokerServer, brokerClient := net.Pipe()
			require.NoError(t, brokerClient.SetDeadline(time.Now().Add(5*time.Second)))
			brokerDone := make(chan error, 1)
			var usbDevice rootusb.Device = device
			go func() {
				brokerDone <- xboxone.ProductionStreamHandler(retainedXboxRetirementAuthenticatedConn{brokerServer}, &usbDevice, nil)
			}()
			t.Cleanup(func() {
				_ = brokerClient.Close()
				_ = brokerServer.Close()
				select {
				case <-brokerDone:
				case <-time.After(time.Second):
					t.Error("broker did not join")
				}
			})
			writeRetirementBrokerFrame(t, brokerClient, 0x01, 0, nil)
			kind, _, _ := readRetirementBrokerFrame(t, brokerClient)
			require.Equal(t, byte(0x81), kind)
			bus := virtualbus.New(958)
			t.Cleanup(func() { _ = bus.Close() })
			server := New(ServerConfig{ConnectionTimeout: time.Second, BusCleanupTimeout: time.Hour,
				RetainedImportAuthorityID: authorityID}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			require.NoError(t, server.AddBus(bus))
			t.Cleanup(func() { _ = server.Close() })
			registration, err := server.AddRetainedDeviceRegistration(bus.BusID(), authorityID, device)
			require.NoError(t, err)
			usbServer, usbClient := netPipeWithDeadline(t, 958)
			usbDone := make(chan error, 1)
			go func() { usbDone <- server.handleConn(usbServer) }()
			writeRetainedImportRequest(t, usbClient, "958-1")
			readSuccessfulRetainedImport(t, usbClient)
			writeRetainedSubmit(t, usbClient, 1, usbip.DirOut, 0, 0, [8]byte{0, 9, 1}, nil)
			_, status, _, _ := readRetainedSubmitResponse(t, usbClient)
			require.Zero(t, status)
			writeRetainedSubmit(t, usbClient, 2, usbip.DirIn, 1, 64, [8]byte{}, nil)
			_, status, _, _ = readRetainedSubmitResponse(t, usbClient)
			require.Zero(t, status)
			start := []byte{0x05, 0x20, 0x02, 0x01, byte(xboxone.SetDeviceStateStart)}
			writeRetainedSubmit(t, usbClient, 3, usbip.DirOut, 1, uint32(len(start)), [8]byte{}, start)
			_, status, _, _ = readRetainedSubmitResponseForDirection(t, usbClient, usbip.DirOut)
			require.Zero(t, status)
			for _, sequence := range []uint32{4, 5} {
				writeRetainedSubmit(t, usbClient, sequence, usbip.DirIn, 1, 64, [8]byte{}, nil)
				_, status, _, _ = readRetainedSubmitResponse(t, usbClient)
				require.Zero(t, status)
			}
			server.retainedImports.mu.Lock()
			session := server.retainedImports.active[deviceID]
			server.retainedImports.mu.Unlock()
			require.NotNil(t, session)
			type closeResult struct {
				removed bool
				err     error
				first   bool
			}
			closeDone := make(chan closeResult, 2)
			go func() {
				removed, err := server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
				closeDone <- closeResult{removed, err, true}
			}()
			kind, correlation, payload := readRetirementBrokerFrame(t, brokerClient)
			require.Equal(t, byte(0x83), kind)
			var feedback controllerfeedback.Frame
			require.NoError(t, feedback.UnmarshalFrom(payload))
			require.True(t, feedback.IsStop())
			require.Len(t, bus.Devices(), 1, "exact Add must remain registered until neutral ACK")
			require.False(t, session.released.Load())
			select {
			case result := <-closeDone:
				t.Fatalf("removal returned before Stop ACK: %+v", result)
			default:
			}
			// A racing new USB stream cannot pass the exact-Add close fence.
			_, registered := server.registerRetainedDeviceStream(func() {}, registration)
			require.False(t, registered)
			go func() {
				removed, err := server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
				closeDone <- closeResult{removed, err, false}
			}()
			switch outcome {
			case "accepted":
				writeRetirementBrokerFrame(t, brokerClient, 0x03, correlation, []byte{1})
			case "rejected":
				writeRetirementBrokerFrame(t, brokerClient, 0x03, correlation, []byte{0})
			case "wrong correlation":
				writeRetirementBrokerFrame(t, brokerClient, 0x03, correlation+1, []byte{1})
			case "socket lost":
				_ = brokerClient.Close()
			}
			for range 2 {
				select {
				case result := <-closeDone:
					if outcome == "accepted" {
						require.NoError(t, result.err)
						if result.first {
							require.True(t, result.removed, "first exact closer must prove retirement")
						}
					} else {
						require.Error(t, result.err)
						require.False(t, result.removed)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("exact management removal did not join")
				}
			}
			select {
			case <-usbDone:
			case <-time.After(time.Second):
				t.Fatal("USB stream did not join")
			}
			if outcome == "accepted" {
				require.Empty(t, bus.Devices())
				require.True(t, session.released.Load())
				require.False(t, session.quarantined.Load())
			} else {
				require.Len(t, bus.Devices(), 1)
				require.False(t, session.released.Load())
				require.True(t, session.quarantined.Load())
				removed, err := server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
				require.Error(t, err)
				require.False(t, removed)
				removed, err = server.RemoveDeviceRegistrationIfPresent(registration)
				require.NoError(t, err)
				require.True(t, removed)
				server.serverLifecycleMu.Lock()
				closeEntries := len(server.retainedRegistrationClosures)
				server.serverLifecycleMu.Unlock()
				require.Zero(t, closeEntries)
				server.retainedImports.mu.Lock()
				quarantinedSession := server.retainedImports.active[deviceID]
				server.retainedImports.mu.Unlock()
				require.Same(t, session, quarantinedSession, "bookkeeping cleanup must not erase owner quarantine")
				require.True(t, session.quarantined.Load())
				require.False(t, session.released.Load())
			}
		})
	}
}

func TestRetainedExactRegistrationCloseRetainsFinishedFailure(t *testing.T) {
	server, registration := newExactCloseTrackedRegistration(t)
	streamID, ok := server.registerRetainedDeviceStream(func() {}, registration)
	require.True(t, ok)
	failure := errors.New("unacknowledged neutral")
	server.finishRetainedStream(streamID, failure)
	removed, err := server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
	require.ErrorIs(t, err, failure)
	require.False(t, removed)
	require.Len(t, registration.Bus.Devices(), 1)
}

func TestRetainedExactRegistrationCompletedFailedCloseIsForgottenAfterCancellation(t *testing.T) {
	for _, cancelBeforeCompletion := range []bool{false, true} {
		t.Run(fmt.Sprint("cancelBeforeCompletion=", cancelBeforeCompletion), func(t *testing.T) {
			server, registration := newExactCloseTrackedRegistration(t)
			canceled := make(chan struct{})
			streamID, ok := server.registerRetainedDeviceStream(func() { close(canceled) }, registration)
			require.True(t, ok)
			failure := errors.New("unacknowledged neutral")
			closeDone := make(chan error, 1)
			go func() {
				_, err := server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
				closeDone <- err
			}()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("exact closer did not cancel its stream")
			}
			if cancelBeforeCompletion {
				removed, err := server.RemoveDeviceRegistrationIfPresent(registration)
				require.NoError(t, err)
				require.True(t, removed)
				server.serverLifecycleMu.Lock()
				count := len(server.retainedRegistrationClosures)
				server.serverLifecycleMu.Unlock()
				require.Equal(t, 1, count, "in-flight closer/stream still owns its bookkeeping")
			}
			server.finishRetainedStream(streamID, failure)
			select {
			case err := <-closeDone:
				require.ErrorIs(t, err, failure)
			case <-time.After(time.Second):
				t.Fatal("exact closer did not join its failed stream")
			}
			if !cancelBeforeCompletion {
				removed, err := server.RemoveDeviceRegistrationIfPresent(registration)
				require.NoError(t, err)
				require.True(t, removed)
			}
			server.serverLifecycleMu.Lock()
			count := len(server.retainedRegistrationClosures)
			server.serverLifecycleMu.Unlock()
			require.Zero(t, count, "a completed close owns no quarantine proof after exact cancellation")
		})
	}
}

func TestRetainedExactRegistrationCompletedFailedCloseKeepsActiveStreamUntilJoin(t *testing.T) {
	server, registration := newExactCloseTrackedRegistration(t)
	server.config.ConnectionTimeout = time.Millisecond
	streamID, ok := server.registerRetainedDeviceStream(func() {}, registration)
	require.True(t, ok)
	removed, err := server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
	require.ErrorIs(t, err, errRetainedImportCloseTimedOut)
	require.False(t, removed)
	removed, err = server.RemoveDeviceRegistrationIfPresent(registration)
	require.NoError(t, err)
	require.True(t, removed)
	server.serverLifecycleMu.Lock()
	count := len(server.retainedRegistrationClosures)
	server.serverLifecycleMu.Unlock()
	require.Equal(t, 1, count, "completed closer must not discard its still-active stream")
	server.finishRetainedStream(streamID, nil)
	server.serverLifecycleMu.Lock()
	count = len(server.retainedRegistrationClosures)
	server.serverLifecycleMu.Unlock()
	require.Zero(t, count)
}

func TestRetainedExactRegistrationCloseDoesNotPoisonOwnerOnDuplicateImport(t *testing.T) {
	server, registration := newExactCloseTrackedRegistration(t)
	streamID, ok := server.registerRetainedDeviceStream(func() {}, registration)
	require.True(t, ok)
	server.finishRetainedStream(streamID, fmt.Errorf("reserve retained import: %w", errRetainedImportBusy))
	removed, err := server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
	require.NoError(t, err)
	require.True(t, removed)
	failure := errors.New("real uncertainty")
	require.ErrorIs(t, retainedRegistrationCloseFailure(errors.Join(errRetainedImportBusy, failure)), failure)
}

func TestRetainedExactRegistrationClosureDoesNotAccumulateAfterOrdinaryRemoval(t *testing.T) {
	for _, finishFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("finishFirst=", finishFirst), func(t *testing.T) {
			server, registration := newExactCloseTrackedRegistration(t)
			streamID, ok := server.registerRetainedDeviceStream(func() {}, registration)
			require.True(t, ok)
			if finishFirst {
				server.finishRetainedStream(streamID, nil)
			}
			removed, err := server.RemoveDeviceRegistrationIfPresent(registration)
			require.NoError(t, err)
			require.True(t, removed)
			if !finishFirst {
				server.finishRetainedStream(streamID, nil)
			}
			server.serverLifecycleMu.Lock()
			count := len(server.retainedRegistrationClosures)
			server.serverLifecycleMu.Unlock()
			require.Zero(t, count)
		})
	}
}

func TestRetainedExactRegistrationCloseTimeoutNeverReopensAdmission(t *testing.T) {
	server, registration := newExactCloseTrackedRegistration(t)
	server.config.ConnectionTimeout = time.Millisecond
	canceled := make(chan struct{})
	streamID, ok := server.registerRetainedDeviceStream(func() { close(canceled) }, registration)
	require.True(t, ok)
	removed, err := server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
	require.ErrorIs(t, err, errRetainedImportCloseTimedOut)
	require.False(t, removed)
	select {
	case <-canceled:
	default:
		t.Fatal("exact stream was not canceled")
	}
	_, ok = server.registerRetainedDeviceStream(func() {}, registration)
	require.False(t, ok)
	server.finishRetainedStream(streamID, nil)
	removed, err = server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
	require.ErrorIs(t, err, errRetainedImportCloseTimedOut)
	require.False(t, removed)
	require.Len(t, registration.Bus.Devices(), 1)
}

// The tracker-only regression uses a production retained descriptor, but no
// import, controller, local executor, or network effect.
func newExactCloseTrackedRegistration(t *testing.T) (*Server, virtualbus.DeviceMeta) {
	t.Helper()
	const authorityID, deviceID = uint64(0x9591), uint64(0x0000fffb01020304)
	device := newProductionRetirementIntegrationDevice(t, authorityID, deviceID)
	server := New(ServerConfig{RetainedImportAuthorityID: authorityID, BusCleanupTimeout: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	bus := virtualbus.New(959)
	require.NoError(t, server.AddBus(bus))
	t.Cleanup(func() { _ = server.RemoveBus(bus.BusID()); _ = server.Close() })
	registration, err := server.AddRetainedDeviceRegistration(bus.BusID(), authorityID, device)
	require.NoError(t, err)
	return server, registration
}
