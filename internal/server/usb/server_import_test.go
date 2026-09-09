package usb

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

func TestParseUSBIPBusIDRejectsUnterminatedAndEmptyFields(t *testing.T) {
	unterminated := bytes.Repeat([]byte{'a'}, busIDSize)
	if _, err := parseUSBIPBusID(unterminated); err == nil ||
		!strings.Contains(err.Error(), "not NUL-terminated") {
		t.Fatalf("unterminated error = %v", err)
	}
	empty := make([]byte, busIDSize)
	if _, err := parseUSBIPBusID(empty); err == nil ||
		!strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty error = %v", err)
	}
	valid := make([]byte, busIDSize)
	copy(valid, "17-4")
	if got, err := parseUSBIPBusID(valid); err != nil || got != "17-4" {
		t.Fatalf("valid busid = %q, %v", got, err)
	}
}

func TestParseUSBIPBusAddressRequiresCanonicalNonzeroComponents(t *testing.T) {
	bus, device, err := parseUSBIPBusAddress("17-4")
	require.NoError(t, err)
	require.Equal(t, uint32(17), bus)
	require.Equal(t, uint32(4), device)
	for _, value := range []string{
		"", "17", "17-", "-4", "0-4", "17-0", "017-4", "17-04",
		"17-4-extra", "4294967296-1", "1-4294967296",
	} {
		_, _, err := parseUSBIPBusAddress(value)
		require.Error(t, err, "accepted non-canonical busid %q", value)
	}
}

func TestReadImportSelectionRetainsExactBusContextForSharedDevicePointer(
	t *testing.T,
) {
	device := &schedulerTestDevice{desc: testCompositeDescriptor()}
	busOne := virtualbus.New(906)
	busTwo := virtualbus.New(907)
	defer busOne.Close() //nolint:errcheck
	defer busTwo.Close() //nolint:errcheck
	contextOne, err := busOne.Add(device)
	require.NoError(t, err)
	contextTwo, err := busTwo.Add(device)
	require.NoError(t, err)
	server := New(ServerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(busOne))
	require.NoError(t, server.AddBus(busTwo))

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close() //nolint:errcheck
	defer clientConn.Close() //nolint:errcheck
	selected := make(chan importSelection, 1)
	failed := make(chan error, 1)
	go func() {
		selection, readErr := server.readImportSelection(serverConn)
		if readErr != nil {
			failed <- readErr
			return
		}
		selected <- selection
	}()
	var request [busIDSize]byte
	copy(request[:], "907-1")
	_, err = clientConn.Write(request[:])
	require.NoError(t, err)
	var selection importSelection
	select {
	case selection = <-selected:
	case err = <-failed:
		t.Fatalf("readImportSelection: %v", err)
	case <-time.After(time.Second):
		t.Fatal("readImportSelection timed out")
	}
	require.Same(t, busTwo, selection.bus)
	require.Same(t, device, selection.dev)
	require.Equal(t, uint32(907), selection.meta.BusID)
	require.Equal(t, uint32(1), selection.meta.DevID)
	require.Equal(t, contextTwo, selection.deviceContext)
	require.NoError(t, busTwo.RemoveDeviceByID("1"))
	select {
	case <-contextTwo.Done():
	case <-time.After(time.Second):
		t.Fatal("selected bus context was not cancelled")
	}
	select {
	case <-contextOne.Done():
		t.Fatal("removing selected bus cancelled the foreign bus context")
	default:
	}
}

func TestHandleImportRejectsUnterminatedBusIDWithoutPanic(t *testing.T) {
	server := New(ServerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close() //nolint:errcheck
	defer clientConn.Close() //nolint:errcheck
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(time.Second)))
	result := make(chan error, 1)
	go func() {
		_, err := server.handleImport(serverConn)
		result <- err
	}()

	_, err := clientConn.Write(bytes.Repeat([]byte{0x5a}, busIDSize))
	require.NoError(t, err)
	var reply [8]byte
	require.NoError(t, usbip.ReadExactly(clientConn, reply[:]))
	require.Equal(t, uint16(usbip.OpRepImport),
		binary.BigEndian.Uint16(reply[2:4]))
	require.NotZero(t, binary.BigEndian.Uint32(reply[4:8]))
	require.ErrorContains(t, <-result, "not NUL-terminated")
}

func TestHandleImportEnforcesExclusiveTokenizedDeviceLease(t *testing.T) {
	device := &schedulerTestDevice{desc: testCompositeDescriptor()}
	bus := virtualbus.New(905)
	defer bus.Close() //nolint:errcheck
	_, err := bus.Add(device)
	require.NoError(t, err)
	server := New(ServerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))

	first := performTestImport(t, server, "905-1")
	require.NoError(t, first.err)
	require.Zero(t, first.status)
	require.NotNil(t, first.release)

	busy := performTestImport(t, server, "905-1")
	require.ErrorContains(t, busy.err, "already imported")
	require.NotZero(t, busy.status)

	first.release()
	successor := performTestImport(t, server, "905-1")
	require.NoError(t, successor.err)
	require.Zero(t, successor.status)

	// A duplicated/late cleanup from the retired owner cannot erase the
	// successor's token.
	first.release()
	stillBusy := performTestImport(t, server, "905-1")
	require.ErrorContains(t, stillBusy.err, "already imported")
	require.NotZero(t, stillBusy.status)
	successor.release()
}

func TestConcurrentDeviceImportClaimsHaveOneOwner(t *testing.T) {
	server := New(ServerConfig{}, nil, nil)
	device := &schedulerTestDevice{desc: testCompositeDescriptor()}
	const contenders = 32
	start := make(chan struct{})
	releaseWinner := make(chan struct{})
	attempted := make(chan struct{}, contenders)
	var successes atomic.Uint32
	var group sync.WaitGroup
	group.Add(contenders)
	for range contenders {
		go func() {
			defer group.Done()
			<-start
			release, ok := server.claimDeviceImport(device)
			if ok {
				successes.Add(1)
				attempted <- struct{}{}
				<-releaseWinner
				release()
				return
			}
			attempted <- struct{}{}
		}()
	}
	close(start)
	for range contenders {
		<-attempted
	}
	require.Equal(t, uint32(1), successes.Load())
	close(releaseWinner)
	group.Wait()
	release, ok := server.claimDeviceImport(device)
	require.True(t, ok, "winner cleanup did not release import lease")
	release()
}

type testImportResult struct {
	status  uint32
	release func()
	err     error
}

func performTestImport(t *testing.T, server *Server, busID string) testImportResult {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(time.Second)))
	result := make(chan testImportResult, 1)
	go func() {
		release, err := server.handleImport(serverConn)
		result <- testImportResult{release: release, err: err}
		_ = serverConn.Close()
	}()
	var request [busIDSize]byte
	copy(request[:], busID)
	_, err := clientConn.Write(request[:])
	require.NoError(t, err)
	var header [8]byte
	require.NoError(t, usbip.ReadExactly(clientConn, header[:]))
	status := binary.BigEndian.Uint32(header[4:8])
	if status == 0 {
		var exported [312]byte
		require.NoError(t, usbip.ReadExactly(clientConn, exported[:]))
	}
	completed := <-result
	completed.status = status
	require.NoError(t, clientConn.Close())
	return completed
}

func FuzzParseUSBIPBusIDNeverPanics(f *testing.F) {
	f.Add([]byte("1-1\x00"))
	f.Add(bytes.Repeat([]byte{0xff}, busIDSize))
	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := parseUSBIPBusID(data)
		if err != nil {
			return
		}
		if len(data) != busIDSize || parsed == "" ||
			bytes.IndexByte(data, 0) != len(parsed) {
			t.Fatalf("accepted malformed busid data=% x parsed=%q", data, parsed)
		}
	})
}
