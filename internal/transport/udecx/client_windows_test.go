//go:build windows

package udecx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestNegotiationABIMismatchExplainsPackageRepair(t *testing.T) {
	for _, transportErr := range []error{
		windows.ERROR_REVISION_MISMATCH,
		windows.ERROR_INVALID_PARAMETER, // Native preview before ABI 1.8.
		windows.ERROR_INSUFFICIENT_BUFFER,
		windows.ERROR_BAD_LENGTH,
	} {
		err := normalizeNegotiationError(transportErr)
		if !errors.Is(err, ErrIncompatibleABI) {
			t.Errorf("negotiation error for %v = %v, want ErrIncompatibleABI", transportErr, err)
		}
		for _, phrase := range []string{fmt.Sprintf("ABI %d.%d", ABIMajor, ABIMinor), "exact native UDE driver", "VIIPER build"} {
			if !strings.Contains(err.Error(), phrase) {
				t.Errorf("negotiation error %q does not contain %q", err, phrase)
			}
		}
	}
}

func TestCompletionAfterCancelPreservesKernelOutcome(t *testing.T) {
	t.Parallel()

	transferred, err := completionAfterCancel(ioCompletion{transferred: 547}, context.Canceled)
	if err != nil || transferred != 547 {
		t.Fatalf("normal completion after cancellation = (%d, %v), want (547, nil)", transferred, err)
	}

	transferred, err = completionAfterCancel(
		ioCompletion{err: windows.ERROR_OPERATION_ABORTED}, context.DeadlineExceeded)
	if transferred != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled completion = (%d, %v), want deadline exceeded", transferred, err)
	}

	transportErr := windows.ERROR_INVALID_DATA
	transferred, err = completionAfterCancel(
		ioCompletion{transferred: 17, err: transportErr}, context.Canceled)
	if transferred != 17 || !errors.Is(err, context.Canceled) || !errors.Is(err, transportErr) {
		t.Fatalf("failed completion = (%d, %v), want joined context and transport errors", transferred, err)
	}
}

func validTestNegotiation() NegotiateResponse {
	identity, err := DeriveBuildIdentity(strings.Repeat("a", 40), DriverPackageVersion,
		ABIMajor, ABIMinor, AdvertisedCapabilities)
	if err != nil {
		panic(err)
	}
	return NegotiateResponse{
		ClientNonce:          7,
		DriverNonce:          8,
		Capabilities:         requiredCapabilities,
		MaxDevices:           MaxDevices,
		MaxDescriptorBytes:   MaxDescriptorBytes,
		MaxTransferBytes:     MaxTransferBytes,
		MaxIsoPackets:        MaxIsoPackets,
		MaxPendingOperations: MaxPendingOperations,
		BuildIdentity:        identity,
	}
}

func TestNegotiationRejectsMissingCapabilitiesAndImpossibleLimits(t *testing.T) {
	valid := validTestNegotiation()
	if err := validateNegotiation(valid, valid.ClientNonce, valid.BuildIdentity); err != nil {
		t.Fatal(err)
	}

	missingCapability := valid
	missingCapability.Capabilities &^= CapabilityIsochronous
	if err := validateNegotiation(missingCapability, valid.ClientNonce, valid.BuildIdentity); err == nil {
		t.Fatal("negotiation accepted a driver without isochronous support")
	}

	extraCapability := valid
	extraCapability.Capabilities |= CapabilityStreams
	if err := validateNegotiation(extraCapability, valid.ClientNonce, valid.BuildIdentity); err == nil {
		t.Fatal("negotiation accepted capabilities outside the identity-bound exact mask")
	}

	oversized := valid
	oversized.MaxTransferBytes++
	if err := validateNegotiation(oversized, valid.ClientNonce, valid.BuildIdentity); err == nil {
		t.Fatal("negotiation accepted a driver limit outside the client ABI")
	}
}

func TestNegotiationNoncesFenceExactFileSession(t *testing.T) {
	valid := validTestNegotiation()
	if err := validateNegotiation(valid, valid.ClientNonce+1, valid.BuildIdentity); err == nil {
		t.Fatal("negotiation accepted a response from a different client-nonce session")
	}
	zeroDriverNonce := valid
	zeroDriverNonce.DriverNonce = 0
	if err := validateNegotiation(
		zeroDriverNonce, valid.ClientNonce, valid.BuildIdentity,
	); err == nil {
		t.Fatal("negotiation accepted a session without a kernel nonce tag")
	}
}

func TestNegotiationRejectsStaleLoadedKernelDespiteMatchingOnDiskPackageContract(t *testing.T) {
	// acceptedPackageIdentity represents the exact source-bound identity from
	// the already validated signed on-disk package and protected manifest. The
	// negotiate response is deliberately from an older image still loaded by
	// Windows, while ABI, capabilities, nonce, and limits all remain identical.
	response := validTestNegotiation()
	acceptedPackageIdentity := response.BuildIdentity
	response.BuildIdentity[0] ^= 0xff

	if err := validateNegotiation(
		response, response.ClientNonce, acceptedPackageIdentity,
	); !errors.Is(err, ErrIncompatibleABI) {
		t.Fatalf("same-ABI/capability stale loaded kernel error=%v want ErrIncompatibleABI", err)
	}
}

func TestClientRejectsRequestsOutsideNegotiatedLimitsBeforeKernelIO(t *testing.T) {
	client := &Client{limits: validTestNegotiation()}
	client.limits.MaxDescriptorBytes = 1
	if err := client.CreateDevice(context.Background(), CreateDevice{
		DescriptorData: []byte{1, 2},
	}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("CreateDevice error=%v want ErrLimitExceeded", err)
	}

	client.limits = validTestNegotiation()
	client.limits.MaxTransferBytes = 1
	if err := client.Complete(context.Background(), Completion{
		Payload: []byte{1, 2},
	}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("Complete error=%v want ErrLimitExceeded", err)
	}
}

func TestCompletionPoolReusesBoundedMediaBuffer(t *testing.T) {
	client := &Client{}
	first := client.acquireCompletionBuffer(2048)
	if len(first) != 2048 || cap(first) < 2048 {
		t.Fatalf("first buffer len=%d cap=%d", len(first), cap(first))
	}
	first[0] = 0x5a
	client.releaseCompletionBuffer(first)
	second := client.acquireCompletionBuffer(1024)
	if len(second) != 1024 || cap(second) < 2048 {
		t.Fatalf("reused buffer len=%d cap=%d", len(second), cap(second))
	}
	client.releaseCompletionBuffer(second)
}

func TestIOCTLCodesMatchPackedHeader(t *testing.T) {
	wants := map[string]struct{ got, want uint32 }{
		"negotiate": {ioctlNegotiate, 0x22e400},
		"create":    {ioctlCreateDevice, 0x22e404},
		"destroy":   {ioctlDestroyDevice, 0x22e408},
		"dequeue":   {ioctlDequeueOperation, 0x22e40e},
		"complete":  {ioctlCompleteOperation, 0x22e411},
		"stats":     {ioctlQueryStats, 0x226414},
		"input":     {ioctlSubmitInputReport, 0x22e419},
		"trace":     {ioctlQueryLifecycleTrace, 0x22641c},
	}
	for name, pair := range wants {
		if pair.got != pair.want {
			t.Errorf("%s IOCTL=%#x want=%#x", name, pair.got, pair.want)
		}
	}
}

func TestParseMultiSZ(t *testing.T) {
	raw := []uint16{'a', 'b', 0, 'c', 0, 0, 'x'}
	got := parseMultiSZ(raw)
	if len(got) != 2 || got[0] != "ab" || got[1] != "c" {
		t.Fatalf("parseMultiSZ=%q", got)
	}
}

func TestCompletionPortRoutesExactOverlappedRequest(t *testing.T) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{completionPort: port, pumpDone: make(chan struct{})}
	go client.runCompletionPort(port)

	request := &ioRequest{done: make(chan ioCompletion, 1)}
	if err := windows.PostQueuedCompletionStatus(port, 547, 0, &request.overlapped); err != nil {
		t.Fatal(err)
	}
	select {
	case completion := <-request.done:
		if completion.err != nil || completion.transferred != 547 {
			t.Fatalf("completion=%+v want 547 successful bytes", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("completion pump did not route the exact OVERLAPPED request")
	}

	if err := windows.PostQueuedCompletionStatus(port, 0, completionPortCloseKey, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.pumpDone:
	case <-time.After(time.Second):
		t.Fatal("completion pump did not stop on its sentinel")
	}
	if err := windows.CloseHandle(port); err != nil {
		t.Fatal(err)
	}
}

func TestEnableSkipCompletionPortOnSuccessRejectsInvalidHandle(t *testing.T) {
	if enableSkipCompletionPortOnSuccess(windows.InvalidHandle) {
		t.Fatal("SetFileCompletionNotificationModes accepted an invalid handle")
	}
}
