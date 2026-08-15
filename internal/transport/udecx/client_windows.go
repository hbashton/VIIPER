//go:build windows

package udecx

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	crSuccess                               = 0
	crBufferSmall                           = 0x1a
	cmGetDeviceInterfaceListPresent         = 0
	digcfPresent                            = 0x00000002
	digcfDeviceInterface                    = 0x00000010
	fileDeviceUnknown                       = 0x22
	methodBuffered                          = 0
	methodInDirect                          = 1
	methodOutDirect                         = 2
	fileReadData                            = 1
	fileWriteData                           = 2
	ioctlBase                               = 0x900
	ioctlNegotiate                          = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 0) << 2) | methodBuffered
	ioctlCreateDevice                       = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 1) << 2) | methodBuffered
	ioctlDestroyDevice                      = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 2) << 2) | methodBuffered
	ioctlDequeueOperation                   = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 3) << 2) | methodOutDirect
	ioctlCompleteOperation                  = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 4) << 2) | methodInDirect
	ioctlQueryStats                         = (fileDeviceUnknown << 16) | (fileReadData << 14) | ((ioctlBase + 5) << 2) | methodBuffered
	ioctlSubmitInputReport                  = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 6) << 2) | methodInDirect
	ioctlQueryLifecycleTrace                = (fileDeviceUnknown << 16) | (fileReadData << 14) | ((ioctlBase + 7) << 2) | methodBuffered
	completionPortCloseKey          uintptr = ^uintptr(0)
	fileSkipCompletionPortOnSuccess byte    = 0x1
	requiredCapabilities                    = AdvertisedCapabilities
	// The kernel rechecks asynchronous UdeCx owner cleanup every 100 ms. Match
	// that cadence for at most 1.9 seconds, rediscovering the interface before
	// every exclusive CreateFile rather than spinning on a stale symbolic link.
	nativeAcquisitionAttempts      = 20
	nativeAcquisitionRetryInterval = 100 * time.Millisecond
	nativeBrokerShareMode          = 0
	// Context expiry requests cancellation; it cannot safely bound completion.
	// Surface a stuck driver promptly without releasing memory still owned by
	// the Windows I/O manager.
	cancellationWatchdogInterval = 5 * time.Second
)

type spDeviceInterfaceData struct {
	CbSize             uint32
	InterfaceClassGUID windows.GUID
	Flags              uint32
	Reserved           uintptr
}

type spDeviceInfoData struct {
	CbSize    uint32
	ClassGUID windows.GUID
	DevInst   uint32
	Reserved  uintptr
}

type spDeviceInterfaceDetailData struct {
	CbSize     uint32
	DevicePath [1]uint16
}

// AcquisitionErrorKind identifies the only two controller-open failures that
// can resolve without repairing or reconfiguring the installed driver. Keep
// this set deliberately narrow: permission, ABI, ambiguity, and device faults
// must reach the caller immediately rather than being hidden by reconnect
// polling.
type AcquisitionErrorKind uint8

const (
	AcquisitionInterfaceUnavailable AcquisitionErrorKind = iota + 1
	AcquisitionOwnerCleanupInProgress
)

// AcquisitionError reports a transient native-controller acquisition state.
// Temporary always returns true; every other Open error is terminal.
type AcquisitionError struct {
	Kind     AcquisitionErrorKind
	Attempts int
	Err      error
}

func (e *AcquisitionError) Error() string {
	switch e.Kind {
	case AcquisitionInterfaceUnavailable:
		return fmt.Sprintf("VIIPER native UDE interface is temporarily unavailable after %d attempt(s): %v", e.Attempts, e.Err)
	case AcquisitionOwnerCleanupInProgress:
		return fmt.Sprintf("VIIPER native UDE controller ownership cleanup is still in progress after %d attempt(s): %v", e.Attempts, e.Err)
	default:
		return fmt.Sprintf("VIIPER native UDE controller acquisition failed after %d attempt(s): %v", e.Attempts, e.Err)
	}
}

func (e *AcquisitionError) Unwrap() error { return e.Err }

func (e *AcquisitionError) Temporary() bool {
	return e != nil && (e.Kind == AcquisitionInterfaceUnavailable ||
		e.Kind == AcquisitionOwnerCleanupInProgress)
}

type nativeAcquisitionPolicy struct {
	attempts int
	interval time.Duration
}

type nativeAcquisitionOps struct {
	discover func(context.Context) ([]string, error)
	open     func(context.Context, string) (windows.Handle, error)
	close    func(windows.Handle) error
	wait     func(context.Context, time.Duration) error
}

var (
	interfaceGUID = windows.GUID{
		Data1: 0x32d03f48,
		Data2: 0x725b,
		Data3: 0x4baa,
		Data4: [8]byte{0x97, 0x0f, 0x7f, 0x5d, 0xe6, 0xc4, 0x46, 0x87},
	}
	cfgmgr32                             = windows.NewLazySystemDLL("cfgmgr32.dll")
	procCMGetDeviceInterfaceListSize     = cfgmgr32.NewProc("CM_Get_Device_Interface_List_SizeW")
	procCMGetDeviceInterfaceList         = cfgmgr32.NewProc("CM_Get_Device_Interface_ListW")
	setupapi                             = windows.NewLazySystemDLL("setupapi.dll")
	procSetupDiGetClassDevsW             = setupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces      = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiGetDeviceInstanceIdW      = setupapi.NewProc("SetupDiGetDeviceInstanceIdW")
	procSetupDiDestroyDeviceInfoList     = setupapi.NewProc("SetupDiDestroyDeviceInfoList")
	kernel32                             = windows.NewLazySystemDLL("kernel32.dll")
	procSetFileCompletionModes           = kernel32.NewProc("SetFileCompletionNotificationModes")
)

type Client struct {
	mu             sync.RWMutex
	inflight       sync.WaitGroup
	handle         windows.Handle
	completionPort windows.Handle
	pumpDone       chan struct{}
	pumpErr        error
	closeDone      chan struct{}
	closeErr       error
	requestPool    sync.Pool
	completionPool sync.Pool
	slowCancels    atomic.Uint64
	// Windows suppresses IOCP packets only for operations that return success
	// inline. Pending operations still use the shared completion pump. This
	// removes a scheduler/channel round trip from direct input without changing
	// cancellation or lifecycle I/O.
	skipCompletionPortOnSuccess bool
	// driverNonce is the nonzero negotiated tag for this exact exclusive file
	// session. The Client and its Host are one-shot, so it cannot be inherited
	// by a successor handle or reused by a later worker/publication graph.
	driverNonce          uint64
	buildIdentity        [BuildIdentitySize]byte
	controllerInstanceID string
	capabilities         Capabilities
	limits               NegotiateResponse
	// pendingObserver is a package-private synchronization seam for the
	// Windows IOCP stress harness. Production clients leave it nil. It runs
	// only after the overlapped issuer has returned ERROR_IO_PENDING, so tests can
	// trigger cancellation and close without scheduler sleeps.
	pendingObserver func(*ioRequest)
	// overlappedIssuer lets the Windows-only stress harness substitute another
	// real overlapped kernel request for DeviceIoControl. Production clients
	// leave it nil and always issue the native UDE IOCTL below.
	overlappedIssuer func(windows.Handle, *windows.Overlapped) (uint32, error)
	// These seams let the Windows IOCP harness deterministically model a driver
	// that accepts cancellation but delays completion. Production clients use
	// CancelIoEx and a real timer and leave the observer nil.
	cancelIssuer             func(windows.Handle, *windows.Overlapped) error
	cancellationWatchdog     func() (<-chan time.Time, func())
	slowCancellationObserver func(code uint32, elapsed time.Duration, count uint64)
	// Deterministic seams for the committed-create rollback tests. Production
	// leaves both nil and uses the exact driver plug-out plus owner-file close.
	destroyForCreateRollback func(context.Context, DeviceIdentity) error
	closeForCreateRollback   func() error
}

type ioCompletion struct {
	transferred uint32
	err         error
}

// CancellationTelemetry reports client-side cancellation acknowledgements
// that exceeded the watchdog interval. It is deliberately outside the ABI
// 1.8 Stats message, so observing a sick driver does not alter the wire format.
type CancellationTelemetry struct {
	SlowAcknowledgements uint64
}

// overlapped must remain the first field. Windows returns the exact pointer
// submitted to DeviceIoControl through the completion port, allowing the
// single completion pump to recover the owning request without a map or lock.
type ioRequest struct {
	overlapped windows.Overlapped
	done       chan ioCompletion
}

func Open(ctx context.Context) (*Client, error) {
	var selectedInterfacePath string
	handle, err := acquireNativeController(ctx, nativeAcquisitionOps{
		discover: discoverNativeInterfacePaths,
		open: func(openCtx context.Context, interfacePath string) (windows.Handle, error) {
			handle, openErr := openNativeController(openCtx, interfacePath)
			if openErr == nil && isUsableNativeHandle(handle) {
				selectedInterfacePath = interfacePath
			}
			return handle, openErr
		},
		close: windows.CloseHandle,
		wait:  waitForNativeAcquisition,
	}, nativeAcquisitionPolicy{
		attempts: nativeAcquisitionAttempts,
		interval: nativeAcquisitionRetryInterval,
	})
	if err != nil {
		return nil, err
	}
	controllerInstanceID, err := controllerInstanceIDForInterfacePath(selectedInterfacePath)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("resolve native UDE controller identity: %w", err)
	}

	completionPort, err := windows.CreateIoCompletionPort(handle, 0, 0, 0)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("associate native UDE controller with I/O completion port: %w", err)
	}
	client := &Client{
		handle:                      handle,
		completionPort:              completionPort,
		pumpDone:                    make(chan struct{}),
		skipCompletionPortOnSuccess: enableSkipCompletionPortOnSuccess(handle),
		controllerInstanceID:        controllerInstanceID,
	}
	client.requestPool.New = func() any {
		return &ioRequest{done: make(chan ioCompletion, 1)}
	}
	client.completionPool.New = func() any {
		// Control/state completions stay inside this initial slab. Larger media
		// buffers grow once and are then recycled by capacity.
		return make([]byte, 0, 4096)
	}
	go client.runCompletionPort(completionPort)
	if err = client.negotiate(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func acquireNativeController(
	ctx context.Context,
	ops nativeAcquisitionOps,
	policy nativeAcquisitionPolicy,
) (windows.Handle, error) {
	if err := ctx.Err(); err != nil {
		return windows.InvalidHandle, err
	}
	if policy.attempts <= 0 {
		return windows.InvalidHandle, errors.New("native UDE acquisition policy has no attempts")
	}
	if policy.interval <= 0 {
		return windows.InvalidHandle, errors.New("native UDE acquisition retry interval must be positive")
	}

	var lastTransient *AcquisitionError
	for attempt := 1; attempt <= policy.attempts; attempt++ {
		paths, err := ops.discover(ctx)
		if err != nil {
			return windows.InvalidHandle, fmt.Errorf("discover native UDE controller: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return windows.InvalidHandle, err
		}

		if len(paths) > 1 {
			return windows.InvalidHandle, fmt.Errorf(
				"refusing ambiguous native UDE ownership: found %d controller interfaces", len(paths))
		}
		if len(paths) == 0 {
			lastTransient = &AcquisitionError{
				Kind:     AcquisitionInterfaceUnavailable,
				Attempts: attempt,
				Err:      windows.ERROR_FILE_NOT_FOUND,
			}
		} else {
			handle, openErr := ops.open(ctx, paths[0])
			if openErr == nil && isUsableNativeHandle(handle) {
				if err := ctx.Err(); err != nil {
					if closeErr := ops.close(handle); closeErr != nil {
						return windows.InvalidHandle, errors.Join(err,
							fmt.Errorf("close canceled native UDE controller handle: %w", closeErr))
					}
					return windows.InvalidHandle, err
				}
				return handle, nil
			}
			if isUsableNativeHandle(handle) {
				if closeErr := ops.close(handle); closeErr != nil {
					return windows.InvalidHandle, errors.Join(openErr,
						fmt.Errorf("close failed native UDE controller handle: %w", closeErr))
				}
			}
			if openErr == nil {
				openErr = windows.ERROR_INVALID_HANDLE
			}
			lastTransient = classifyNativeAcquisitionError(openErr, attempt)
			if lastTransient == nil {
				return windows.InvalidHandle, fmt.Errorf("open native UDE controller: %w", openErr)
			}
		}

		if attempt == policy.attempts {
			return windows.InvalidHandle, lastTransient
		}
		if err := ops.wait(ctx, policy.interval); err != nil {
			return windows.InvalidHandle, err
		}
	}

	panic("unreachable native UDE acquisition state")
}

func classifyNativeAcquisitionError(err error, attempt int) *AcquisitionError {
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
		return &AcquisitionError{
			Kind:     AcquisitionInterfaceUnavailable,
			Attempts: attempt,
			Err:      err,
		}
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return &AcquisitionError{
			Kind:     AcquisitionOwnerCleanupInProgress,
			Attempts: attempt,
			Err:      err,
		}
	default:
		return nil
	}
}

func isUsableNativeHandle(handle windows.Handle) bool {
	return handle != 0 && handle != windows.InvalidHandle
}

func discoverNativeInterfacePaths(ctx context.Context) ([]string, error) {
	return discoverInterfacePaths(ctx)
}

func openNativeController(ctx context.Context, interfacePath string) (windows.Handle, error) {
	if err := ctx.Err(); err != nil {
		return windows.InvalidHandle, err
	}
	path, err := windows.UTF16PtrFromString(interfacePath)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("encode native UDE interface path: %w", err)
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		nativeBrokerShareMode, // One broker owns the driver session; never weaken exclusive sharing.
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED,
		0)
	if err != nil {
		return handle, err
	}
	return handle, nil
}

func waitForNativeAcquisition(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func enableSkipCompletionPortOnSuccess(handle windows.Handle) bool {
	result, _, _ := procSetFileCompletionModes.Call(
		uintptr(handle), uintptr(fileSkipCompletionPortOnSuccess))
	return result != 0
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.handle == 0 || c.handle == windows.InvalidHandle {
		closeDone := c.closeDone
		closeErr := c.closeErr
		c.mu.Unlock()
		if closeDone == nil {
			return closeErr
		}
		<-closeDone
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.closeErr
	}
	handle := c.handle
	completionPort := c.completionPort
	pumpDone := c.pumpDone
	closeDone := make(chan struct{})
	c.closeDone = closeDone
	c.handle = windows.InvalidHandle
	c.completionPort = windows.InvalidHandle
	c.mu.Unlock()

	_ = windows.CancelIoEx(handle, nil)
	c.inflight.Wait()
	var closeErr error
	if err := windows.PostQueuedCompletionStatus(
		completionPort, 0, completionPortCloseKey, nil); err != nil {
		// Closing the port is the documented escape hatch for a waiter when a
		// sentinel cannot be posted. The pump records the abandoned wait.
		_ = windows.CloseHandle(completionPort)
		<-pumpDone
		closeErr = errors.Join(windows.CloseHandle(handle), err)
	} else {
		<-pumpDone
		closeErr = errors.Join(windows.CloseHandle(handle), windows.CloseHandle(completionPort))
	}

	c.mu.Lock()
	c.closeErr = closeErr
	close(closeDone)
	c.mu.Unlock()
	return closeErr
}

func (c *Client) runCompletionPort(completionPort windows.Handle) {
	defer close(c.pumpDone)
	for {
		var transferred uint32
		var key uintptr
		var overlapped *windows.Overlapped
		err := windows.GetQueuedCompletionStatus(
			completionPort, &transferred, &key, &overlapped, windows.INFINITE)
		if overlapped == nil {
			if key == completionPortCloseKey {
				return
			}
			c.mu.Lock()
			if err == nil {
				c.pumpErr = errors.New("native UDE I/O completion pump stopped on an unexpected packet")
			} else {
				c.pumpErr = fmt.Errorf("native UDE I/O completion pump stopped: %w", err)
			}
			c.mu.Unlock()
			return
		}
		request := (*ioRequest)(unsafe.Pointer(overlapped))
		request.done <- ioCompletion{transferred: transferred, err: err}
	}
}

func (c *Client) completionPumpError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.pumpErr != nil {
		return c.pumpErr
	}
	return windows.ERROR_INVALID_HANDLE
}

func completionAfterCancel(result ioCompletion, contextErr error) (uint32, error) {
	// CancelIoEx is advisory: Microsoft explicitly permits the operation to
	// complete normally when cancellation loses the race. Preserve that kernel
	// outcome so create/destroy state cannot diverge across the ABI boundary.
	if result.err == nil {
		return result.transferred, nil
	}
	if errors.Is(result.err, windows.ERROR_OPERATION_ABORTED) {
		return 0, contextErr
	}
	return result.transferred, errors.Join(contextErr, result.err)
}

func completionAfterPumpStop(handle windows.Handle, request *ioRequest) ioCompletion {
	// The pump closes pumpDone only after its last channel send. Drain that
	// terminal packet first: if both channels were ready, select may have chosen
	// pumpDone and leaving request.done populated would poison pooled reuse.
	select {
	case result := <-request.done:
		return result
	default:
	}

	_ = windows.CancelIoEx(handle, &request.overlapped)
	var transferred uint32
	err := windows.GetOverlappedResult(handle, &request.overlapped, &transferred, true)
	return ioCompletion{transferred: transferred, err: err}
}

func (c *Client) cancelOverlapped(handle windows.Handle, request *ioRequest) error {
	if c.cancelIssuer != nil {
		return c.cancelIssuer(handle, &request.overlapped)
	}
	return windows.CancelIoEx(handle, &request.overlapped)
}

func (c *Client) startCancellationWatchdog() (<-chan time.Time, func()) {
	if c.cancellationWatchdog != nil {
		return c.cancellationWatchdog()
	}
	timer := time.NewTimer(cancellationWatchdogInterval)
	return timer.C, func() { timer.Stop() }
}

func (c *Client) recordSlowCancellation(code uint32, elapsed time.Duration) {
	count := c.slowCancels.Add(1)
	if c.slowCancellationObserver != nil {
		c.slowCancellationObserver(code, elapsed, count)
		return
	}
	slog.Warn(
		"native UDE driver has not acknowledged overlapped I/O cancellation",
		"ioctl", fmt.Sprintf("%#x", code),
		"elapsed", elapsed.Round(time.Millisecond),
		"slow_cancellations", count,
	)
}

func (c *Client) CancellationTelemetry() CancellationTelemetry {
	return CancellationTelemetry{SlowAcknowledgements: c.slowCancels.Load()}
}

func (c *Client) Capabilities() Capabilities {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.capabilities
}

func (c *Client) Limits() NegotiateResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.limits
}

// BuildIdentity is the identity returned by the currently loaded kernel
// image and accepted during this client's negotiation. It is not inferred
// from an on-disk driver path or copied from broker build metadata.
func (c *Client) BuildIdentity() [BuildIdentitySize]byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.buildIdentity
}

func (c *Client) ControllerInstanceID() string {
	return c.controllerInstanceID
}

// ControllerSessionID is the nonzero kernel-authored nonce for this exact
// exclusive controller file session. It is stable across API and stream
// reconnects through the same broker, and changes whenever that controller
// session is recreated.
func (c *Client) ControllerSessionID() uint64 {
	return c.driverNonce
}

func (c *Client) negotiate(ctx context.Context) error {
	expectedBuildIdentity, err := ExpectedBuildIdentity()
	if err != nil {
		return fmt.Errorf("prepare native UDE negotiation: %w", err)
	}
	var nonceBytes [8]byte
	if _, err = rand.Read(nonceBytes[:]); err != nil {
		return fmt.Errorf("create native UDE session nonce: %w", err)
	}
	nonce := binary.LittleEndian.Uint64(nonceBytes[:])
	if nonce == 0 {
		nonce = 1
	}
	request, err := (NegotiateRequest{
		ClientNonce:           nonce,
		RequestedCapabilities: requiredCapabilities,
	}).MarshalBinary()
	if err != nil {
		return err
	}
	response := make([]byte, NegotiateResponseSize)
	written, err := c.ioctl(ctx, ioctlNegotiate, request, response)
	if err != nil {
		return normalizeNegotiationError(err)
	}
	if written != NegotiateResponseSize {
		return fmt.Errorf("negotiate native UDE ABI: response bytes=%d want=%d", written, NegotiateResponseSize)
	}
	negotiated, err := ParseNegotiateResponse(response)
	if err != nil {
		return fmt.Errorf("validate native UDE negotiation: %w", err)
	}
	if err := validateNegotiation(negotiated, nonce, expectedBuildIdentity); err != nil {
		return err
	}
	c.driverNonce = negotiated.DriverNonce
	c.capabilities = negotiated.Capabilities
	c.buildIdentity = negotiated.BuildIdentity
	c.limits = negotiated
	return nil
}

func normalizeNegotiationError(err error) error {
	// ABI 1.8 was the first driver that reported ERROR_REVISION_MISMATCH. Older
	// native previews reject this service's otherwise internally generated,
	// fixed negotiation request as ERROR_INVALID_PARAMETER. A future fixed
	// request-size change can surface as either length error before the driver
	// reaches its version check. None of these can be caused by user data, so
	// they all mean that the service and installed package must be repaired as
	// one version-locked unit.
	if errors.Is(err, windows.ERROR_REVISION_MISMATCH) ||
		errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) ||
		errors.Is(err, windows.ERROR_BAD_LENGTH) {
		return fmt.Errorf(
			"%w: service expects ABI %d.%d; install the exact native UDE driver packaged with this VIIPER build: %v",
			ErrIncompatibleABI, ABIMajor, ABIMinor, err)
	}
	return fmt.Errorf("negotiate native UDE ABI: %w", err)
}

func validateNegotiation(negotiated NegotiateResponse, nonce uint64, expectedBuildIdentity [BuildIdentitySize]byte) error {
	if negotiated.ClientNonce != nonce || negotiated.DriverNonce == 0 {
		return errors.New("validate native UDE negotiation: session nonce mismatch")
	}
	if negotiated.Capabilities != requiredCapabilities {
		return fmt.Errorf("validate native UDE negotiation: exact capabilities %#x required, driver returned %#x",
			requiredCapabilities, negotiated.Capabilities)
	}
	if subtle.ConstantTimeCompare(negotiated.BuildIdentity[:], expectedBuildIdentity[:]) != 1 {
		return fmt.Errorf(
			"%w: loaded kernel build identity=%s expected=%s; restart or repair the exact signed native package",
			ErrIncompatibleABI, BuildIdentityHex(negotiated.BuildIdentity), BuildIdentityHex(expectedBuildIdentity),
		)
	}
	if negotiated.MaxDevices == 0 || negotiated.MaxDescriptorBytes == 0 ||
		negotiated.MaxTransferBytes == 0 || negotiated.MaxIsoPackets == 0 ||
		negotiated.MaxPendingOperations == 0 {
		return errors.New("validate native UDE negotiation: driver returned a zero limit")
	}
	if negotiated.MaxDevices > MaxDevices || negotiated.MaxDescriptorBytes > MaxDescriptorBytes ||
		negotiated.MaxTransferBytes > MaxTransferBytes || negotiated.MaxIsoPackets > MaxIsoPackets ||
		negotiated.MaxPendingOperations > MaxPendingOperations {
		return errors.New("validate native UDE negotiation: driver limits exceed this client's ABI bounds")
	}
	return nil
}

func (c *Client) CreateDevice(ctx context.Context, device CreateDevice) (DeviceRegistration, error) {
	limits := c.Limits()
	if uint32(len(device.DescriptorData)) > limits.MaxDescriptorBytes ||
		device.MaxPendingOperations > limits.MaxPendingOperations {
		return DeviceRegistration{}, ErrLimitExceeded
	}
	request, err := device.MarshalBinary()
	if err != nil {
		return DeviceRegistration{}, err
	}
	response := make([]byte, CreateDeviceResultSize)
	written, err := c.ioctl(ctx, ioctlCreateDevice, request, response)
	if err != nil {
		return DeviceRegistration{}, err
	}
	if written != CreateDeviceResultSize {
		return DeviceRegistration{}, c.rollbackCommittedCreate(device,
			fmt.Errorf("native UDE create receipt: %w", ErrInvalidSize))
	}
	result, err := ParseCreateDeviceResult(response)
	if err != nil {
		return DeviceRegistration{}, c.rollbackCommittedCreate(device,
			fmt.Errorf("parse native UDE create receipt: %w", err))
	}
	if result.DeviceID != device.DeviceID || result.Generation != device.Generation ||
		result.Speed != device.Speed {
		return DeviceRegistration{}, c.rollbackCommittedCreate(device,
			fmt.Errorf("%w: native UDE create receipt does not match request", ErrInvalidRange))
	}
	if c.controllerInstanceID == "" {
		return DeviceRegistration{}, c.rollbackCommittedCreate(device,
			errors.New("native UDE controller instance identity is unavailable"))
	}
	controllerSessionID := c.ControllerSessionID()
	if controllerSessionID == 0 {
		return DeviceRegistration{}, c.rollbackCommittedCreate(device,
			errors.New("native UDE controller session identity is unavailable"))
	}
	return DeviceRegistration{
		DeviceIdentity: DeviceIdentity{DeviceID: result.DeviceID, Generation: result.Generation},
		Speed:          result.Speed, USB20PortNumber: result.USB20PortNumber,
		USB30PortNumber:      result.USB30PortNumber,
		ControllerSessionID:  controllerSessionID,
		ControllerInstanceID: c.controllerInstanceID,
	}, nil
}

func (c *Client) rollbackCommittedCreate(device CreateDevice, receiptErr error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), terminalCleanupTimeout)
	defer cancel()
	identity := DeviceIdentity{DeviceID: device.DeviceID, Generation: device.Generation}
	var cleanupErr error
	if c.destroyForCreateRollback != nil {
		cleanupErr = c.destroyForCreateRollback(cleanupCtx, identity)
	} else {
		cleanupErr = c.DestroyDevice(cleanupCtx, identity)
	}
	if cleanupErr != nil {
		// A malformed successful receipt is already a terminal session fault. If
		// its exact plug-out is rejected, close the exclusive file immediately;
		// the kernel's owner-cleanup join is the final authority that prevents an
		// unrouteable child from surviving this failed registration.
		var closeErr error
		if c.closeForCreateRollback != nil {
			closeErr = c.closeForCreateRollback()
		} else {
			closeErr = c.Close()
		}
		rollbackErr := fmt.Errorf(
			"rollback native UDE device after invalid create receipt: %w", cleanupErr)
		if closeErr != nil {
			return errors.Join(receiptErr, rollbackErr,
				fmt.Errorf("close native UDE owner session after uncertain create rollback: %w", closeErr))
		}
		return errors.Join(receiptErr, rollbackErr)
	}
	return receiptErr
}

func (c *Client) DestroyDevice(ctx context.Context, identity DeviceIdentity) error {
	request, err := identity.MarshalBinary()
	if err != nil {
		return err
	}
	_, err = c.ioctl(ctx, ioctlDestroyDevice, request, nil)
	return err
}

func (c *Client) Dequeue(ctx context.Context, buffer []byte) (Operation, error) {
	if len(buffer) < OperationSize {
		return Operation{}, ErrShortMessage
	}
	written, err := c.ioctl(ctx, ioctlDequeueOperation, nil, buffer)
	if err != nil {
		return Operation{}, err
	}
	return parseDequeuedOperation(buffer, written)
}

func (c *Client) Complete(ctx context.Context, completion Completion) error {
	limits := c.Limits()
	if uint32(len(completion.Payload)) > limits.MaxTransferBytes ||
		uint32(len(completion.IsoPackets)) > limits.MaxIsoPackets ||
		completion.TransferLength > limits.MaxTransferBytes {
		return ErrLimitExceeded
	}
	_, _, total, err := completion.wireLayout()
	if err != nil {
		return err
	}
	request := c.acquireCompletionBuffer(total)
	defer c.releaseCompletionBuffer(request)
	if err = completion.marshalBinaryInto(request); err != nil {
		return err
	}
	// METHOD_IN_DIRECT keeps the fixed metadata in the system buffer and maps
	// the variable packet/payload tail read-only into the driver.
	_, err = c.ioctl(ctx, ioctlCompleteOperation, request[:CompletionSize], request[CompletionSize:])
	return err
}

func (c *Client) acquireCompletionBuffer(size int) []byte {
	var buffer []byte
	if pooled := c.completionPool.Get(); pooled != nil {
		buffer = pooled.([]byte)
	}
	if cap(buffer) < size {
		return make([]byte, size)
	}
	return buffer[:size]
}

func (c *Client) releaseCompletionBuffer(buffer []byte) {
	// A negotiated completion cannot exceed the protocol's bounded maximum.
	// Retaining the slab avoids high-frequency ISO completion churn while
	// keeping worst-case pool entries bounded by the ABI.
	c.completionPool.Put(buffer[:0])
}

func (c *Client) SubmitInputReport(ctx context.Context, report InputReport) error {
	var metadata [InputReportSize]byte
	if err := report.marshalMetadata(metadata[:]); err != nil {
		return err
	}
	_, err := c.ioctl(ctx, ioctlSubmitInputReport, metadata[:], report.Payload)
	if errors.Is(err, windows.ERROR_BUSY) {
		return ErrInputQueueFull
	}
	return err
}

func (c *Client) QueryStats(ctx context.Context) (Stats, error) {
	buffer := make([]byte, StatsSize)
	written, err := c.ioctl(ctx, ioctlQueryStats, nil, buffer)
	if err != nil {
		return Stats{}, err
	}
	if written != StatsSize {
		return Stats{}, ErrInvalidSize
	}
	return ParseStats(buffer)
}

func (c *Client) QueryLifecycleTrace(ctx context.Context) (LifecycleTrace, error) {
	buffer := make([]byte, LifecycleTraceSize)
	written, err := c.ioctl(ctx, ioctlQueryLifecycleTrace, nil, buffer)
	if err != nil {
		return LifecycleTrace{}, err
	}
	if written != LifecycleTraceSize {
		return LifecycleTrace{}, ErrInvalidSize
	}
	return ParseLifecycleTrace(buffer)
}

func (c *Client) beginIO() (windows.Handle, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.handle == 0 || c.handle == windows.InvalidHandle {
		return windows.InvalidHandle, windows.ERROR_INVALID_HANDLE
	}
	if c.pumpErr != nil {
		return windows.InvalidHandle, c.pumpErr
	}
	c.inflight.Add(1)
	return c.handle, nil
}

func (c *Client) ioctl(ctx context.Context, code uint32, input, output []byte) (uint32, error) {
	handle, err := c.beginIO()
	if err != nil {
		return 0, err
	}
	defer c.inflight.Done()

	request := c.requestPool.Get().(*ioRequest)
	request.overlapped = windows.Overlapped{}
	select {
	case <-request.done:
		panic("native UDE I/O request returned to pool with an unread completion")
	default:
	}
	defer func() {
		runtime.KeepAlive(input)
		runtime.KeepAlive(output)
		runtime.KeepAlive(request)
		c.requestPool.Put(request)
	}()
	var inputPointer *byte
	var outputPointer *byte
	if len(input) != 0 {
		inputPointer = &input[0]
	}
	if len(output) != 0 {
		outputPointer = &output[0]
	}
	var immediate uint32
	if c.overlappedIssuer != nil {
		immediate, err = c.overlappedIssuer(handle, &request.overlapped)
	} else {
		err = windows.DeviceIoControl(
			handle, code,
			inputPointer, uint32(len(input)),
			outputPointer, uint32(len(output)),
			&immediate, &request.overlapped)
	}
	if err == nil && c.skipCompletionPortOnSuccess {
		// FILE_SKIP_COMPLETION_PORT_ON_SUCCESS guarantees that no completion
		// packet exists for this exact immediate-success operation. Returning
		// inline preserves the direct report-submission path and avoids waking the
		// completion pump merely to hand the same result back to this goroutine.
		return immediate, nil
	}
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, err
	}
	if errors.Is(err, windows.ERROR_IO_PENDING) && c.pendingObserver != nil {
		c.pendingObserver(request)
	}

	select {
	case result := <-request.done:
		return result.transferred, result.err
	case <-ctx.Done():
		contextErr := ctx.Err()
		cancelStarted := time.Now()
		_ = c.cancelOverlapped(handle, request)
		watchdog, stopWatchdog := c.startCancellationWatchdog()
		defer stopWatchdog()

		// CancelIoEx only marks the operation for cancellation and explicitly
		// forbids freeing or reusing OVERLAPPED until the final completion:
		// https://learn.microsoft.com/windows/win32/api/ioapiset/nf-ioapiset-cancelioex
		// DeviceIoControl also still owns input/output buffers. A hard return here
		// would therefore permit both use-after-return and pooled OVERLAPPED reuse.
		// Lifecycle mutations may also complete successfully after cancellation,
		// so wait for and preserve the authoritative kernel outcome. The watchdog
		// makes a broken cancellation path observable without violating lifetime.
		for {
			select {
			case result := <-request.done:
				return completionAfterCancel(result, contextErr)
			case <-c.pumpDone:
				result := completionAfterPumpStop(handle, request)
				return completionAfterCancel(result, errors.Join(contextErr, c.completionPumpError()))
			case <-watchdog:
				c.recordSlowCancellation(code, time.Since(cancelStarted))
				watchdog = nil
			}
		}
	case <-c.pumpDone:
		result := completionAfterPumpStop(handle, request)
		return completionAfterCancel(result, c.completionPumpError())
	}
}

func discoverInterfacePaths(ctx context.Context) ([]string, error) {
	for attempt := 0; attempt < 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var required uint32
		ret, _, _ := procCMGetDeviceInterfaceListSize.Call(
			uintptr(unsafe.Pointer(&required)),
			uintptr(unsafe.Pointer(&interfaceGUID)),
			0,
			cmGetDeviceInterfaceListPresent)
		if uint32(ret) != crSuccess {
			return nil, fmt.Errorf("CM_Get_Device_Interface_List_SizeW returned CONFIGRET %#x", uint32(ret))
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if required <= 1 {
			return nil, nil
		}
		buffer := make([]uint16, required)
		ret, _, _ = procCMGetDeviceInterfaceList.Call(
			uintptr(unsafe.Pointer(&interfaceGUID)),
			0,
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(required),
			cmGetDeviceInterfaceListPresent)
		if uint32(ret) == crBufferSmall {
			continue
		}
		if uint32(ret) != crSuccess {
			return nil, fmt.Errorf("CM_Get_Device_Interface_ListW returned CONFIGRET %#x", uint32(ret))
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return parseMultiSZ(buffer), nil
	}
	return nil, errors.New("native UDE interface list changed repeatedly during discovery")
}

func controllerInstanceIDForInterfacePath(interfacePath string) (string, error) {
	if strings.TrimSpace(interfacePath) == "" {
		return "", errors.New("native UDE interface path is empty")
	}
	setValue, _, setErr := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&interfaceGUID)), 0, 0,
		uintptr(digcfPresent|digcfDeviceInterface))
	set := windows.Handle(setValue)
	if set == windows.InvalidHandle {
		if setErr != nil && !errors.Is(setErr, windows.ERROR_SUCCESS) {
			return "", fmt.Errorf("SetupDiGetClassDevsW: %w", setErr)
		}
		return "", errors.New("SetupDiGetClassDevsW returned an invalid handle")
	}
	defer procSetupDiDestroyDeviceInfoList.Call(uintptr(set))

	for index := uint32(0); ; index++ {
		interfaceData := spDeviceInterfaceData{CbSize: uint32(unsafe.Sizeof(spDeviceInterfaceData{}))}
		ok, _, enumErr := procSetupDiEnumDeviceInterfaces.Call(
			uintptr(set), 0, uintptr(unsafe.Pointer(&interfaceGUID)), uintptr(index),
			uintptr(unsafe.Pointer(&interfaceData)))
		if ok == 0 {
			if errors.Is(enumErr, windows.ERROR_NO_MORE_ITEMS) {
				break
			}
			return "", fmt.Errorf("SetupDiEnumDeviceInterfaces(%d): %w", index, enumErr)
		}

		var required uint32
		_, _, sizeErr := procSetupDiGetDeviceInterfaceDetailW.Call(
			uintptr(set), uintptr(unsafe.Pointer(&interfaceData)), 0, 0,
			uintptr(unsafe.Pointer(&required)), 0)
		if required < uint32(unsafe.Sizeof(spDeviceInterfaceDetailData{})) ||
			!errors.Is(sizeErr, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", fmt.Errorf("SetupDiGetDeviceInterfaceDetailW size query: %w", sizeErr)
		}
		detailBytes := make([]byte, required)
		detail := (*spDeviceInterfaceDetailData)(unsafe.Pointer(&detailBytes[0]))
		detail.CbSize = uint32(unsafe.Sizeof(spDeviceInterfaceDetailData{}))
		deviceInfo := spDeviceInfoData{CbSize: uint32(unsafe.Sizeof(spDeviceInfoData{}))}
		ok, _, detailErr := procSetupDiGetDeviceInterfaceDetailW.Call(
			uintptr(set), uintptr(unsafe.Pointer(&interfaceData)),
			uintptr(unsafe.Pointer(detail)), uintptr(required), 0,
			uintptr(unsafe.Pointer(&deviceInfo)))
		if ok == 0 {
			return "", fmt.Errorf("SetupDiGetDeviceInterfaceDetailW: %w", detailErr)
		}
		candidate := windows.UTF16PtrToString(&detail.DevicePath[0])
		if !strings.EqualFold(candidate, interfacePath) {
			continue
		}

		var instanceChars uint32
		_, _, instanceSizeErr := procSetupDiGetDeviceInstanceIdW.Call(
			uintptr(set), uintptr(unsafe.Pointer(&deviceInfo)), 0, 0,
			uintptr(unsafe.Pointer(&instanceChars)))
		if instanceChars < 2 || !errors.Is(instanceSizeErr, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", fmt.Errorf("SetupDiGetDeviceInstanceIdW size query: %w", instanceSizeErr)
		}
		instanceBuffer := make([]uint16, instanceChars)
		ok, _, instanceErr := procSetupDiGetDeviceInstanceIdW.Call(
			uintptr(set), uintptr(unsafe.Pointer(&deviceInfo)),
			uintptr(unsafe.Pointer(&instanceBuffer[0])), uintptr(instanceChars), 0)
		if ok == 0 {
			return "", fmt.Errorf("SetupDiGetDeviceInstanceIdW: %w", instanceErr)
		}
		instanceID := windows.UTF16ToString(instanceBuffer)
		if !IsCanonicalControllerInstanceID(instanceID) {
			return "", errors.New("native UDE controller returned an invalid instance identity")
		}
		return instanceID, nil
	}
	return "", errors.New("opened native UDE interface was not present in the verified SetupAPI set")
}

func parseMultiSZ(raw []uint16) []string {
	result := make([]string, 0, 1)
	start := 0
	for i, value := range raw {
		if value != 0 {
			continue
		}
		if i == start {
			break
		}
		result = append(result, string(utf16.Decode(raw[start:i])))
		start = i + 1
	}
	return result
}
