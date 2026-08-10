//go:build windows

package udecx

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	crSuccess                               = 0
	crBufferSmall                           = 0x1a
	cmGetDeviceInterfaceListPresent         = 0
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
	completionPortCloseKey          uintptr = ^uintptr(0)
	requiredCapabilities                    = CapabilityIsochronous | CapabilityDeviceLifecycle | CapabilityInputReports
)

var (
	interfaceGUID = windows.GUID{
		Data1: 0x32d03f48,
		Data2: 0x725b,
		Data3: 0x4baa,
		Data4: [8]byte{0x97, 0x0f, 0x7f, 0x5d, 0xe6, 0xc4, 0x46, 0x87},
	}
	cfgmgr32                         = windows.NewLazySystemDLL("cfgmgr32.dll")
	procCMGetDeviceInterfaceListSize = cfgmgr32.NewProc("CM_Get_Device_Interface_List_SizeW")
	procCMGetDeviceInterfaceList     = cfgmgr32.NewProc("CM_Get_Device_Interface_ListW")
)

type Client struct {
	mu             sync.RWMutex
	inflight       sync.WaitGroup
	handle         windows.Handle
	completionPort windows.Handle
	pumpDone       chan struct{}
	pumpErr        error
	requestPool    sync.Pool
	driverNonce    uint64
	capabilities   Capabilities
	limits         NegotiateResponse
}

type ioCompletion struct {
	transferred uint32
	err         error
}

// overlapped must remain the first field. Windows returns the exact pointer
// submitted to DeviceIoControl through the completion port, allowing the
// single completion pump to recover the owning request without a map or lock.
type ioRequest struct {
	overlapped windows.Overlapped
	done       chan ioCompletion
}

func Open(ctx context.Context) (*Client, error) {
	paths, err := discoverInterfacePaths()
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, errors.New("VIIPER native UDE interface is not present")
	}
	if len(paths) != 1 {
		return nil, fmt.Errorf("refusing ambiguous native UDE ownership: found %d controller interfaces", len(paths))
	}

	path, err := windows.UTF16PtrFromString(paths[0])
	if err != nil {
		return nil, fmt.Errorf("encode native UDE interface path: %w", err)
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED,
		0)
	if err != nil {
		return nil, fmt.Errorf("open native UDE controller: %w", err)
	}

	completionPort, err := windows.CreateIoCompletionPort(handle, 0, 0, 0)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("associate native UDE controller with I/O completion port: %w", err)
	}
	client := &Client{
		handle:         handle,
		completionPort: completionPort,
		pumpDone:       make(chan struct{}),
	}
	client.requestPool.New = func() any {
		return &ioRequest{done: make(chan ioCompletion, 1)}
	}
	go client.runCompletionPort(completionPort)
	if err = client.negotiate(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.handle == 0 || c.handle == windows.InvalidHandle {
		c.mu.Unlock()
		return nil
	}
	handle := c.handle
	completionPort := c.completionPort
	pumpDone := c.pumpDone
	c.handle = windows.InvalidHandle
	c.completionPort = windows.InvalidHandle
	c.mu.Unlock()

	_ = windows.CancelIoEx(handle, nil)
	c.inflight.Wait()
	if err := windows.PostQueuedCompletionStatus(
		completionPort, 0, completionPortCloseKey, nil); err != nil {
		// Closing the port is the documented escape hatch for a waiter when a
		// sentinel cannot be posted. The pump records the abandoned wait.
		_ = windows.CloseHandle(completionPort)
		<-pumpDone
		return errors.Join(windows.CloseHandle(handle), err)
	}
	<-pumpDone
	return errors.Join(windows.CloseHandle(handle), windows.CloseHandle(completionPort))
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
			c.pumpErr = fmt.Errorf("native UDE I/O completion pump stopped: %w", err)
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

func (c *Client) negotiate(ctx context.Context) error {
	var nonceBytes [8]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
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
		return fmt.Errorf("negotiate native UDE ABI: %w", err)
	}
	if written != NegotiateResponseSize {
		return fmt.Errorf("negotiate native UDE ABI: response bytes=%d want=%d", written, NegotiateResponseSize)
	}
	negotiated, err := ParseNegotiateResponse(response)
	if err != nil {
		return fmt.Errorf("validate native UDE negotiation: %w", err)
	}
	if err := validateNegotiation(negotiated, nonce); err != nil {
		return err
	}
	c.driverNonce = negotiated.DriverNonce
	c.capabilities = negotiated.Capabilities
	c.limits = negotiated
	return nil
}

func validateNegotiation(negotiated NegotiateResponse, nonce uint64) error {
	if negotiated.ClientNonce != nonce || negotiated.DriverNonce == 0 {
		return errors.New("validate native UDE negotiation: session nonce mismatch")
	}
	if negotiated.Capabilities&requiredCapabilities != requiredCapabilities {
		return fmt.Errorf("validate native UDE negotiation: required capabilities %#x, driver returned %#x",
			requiredCapabilities, negotiated.Capabilities)
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

func (c *Client) CreateDevice(ctx context.Context, device CreateDevice) error {
	limits := c.Limits()
	if uint32(len(device.DescriptorData)) > limits.MaxDescriptorBytes ||
		device.MaxPendingOperations > limits.MaxPendingOperations {
		return ErrLimitExceeded
	}
	request, err := device.MarshalBinary()
	if err != nil {
		return err
	}
	_, err = c.ioctl(ctx, ioctlCreateDevice, request, nil)
	return err
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
	if written < OperationSize || written > uint32(len(buffer)) {
		return Operation{}, ErrInvalidSize
	}
	return ParseOperation(buffer[:written])
}

func (c *Client) Complete(ctx context.Context, completion Completion) error {
	limits := c.Limits()
	if uint32(len(completion.Payload)) > limits.MaxTransferBytes ||
		uint32(len(completion.IsoPackets)) > limits.MaxIsoPackets ||
		completion.TransferLength > limits.MaxTransferBytes {
		return ErrLimitExceeded
	}
	request, err := completion.MarshalBinary()
	if err != nil {
		return err
	}
	// METHOD_IN_DIRECT keeps the fixed metadata in the system buffer and maps
	// the variable packet/payload tail read-only into the driver.
	_, err = c.ioctl(ctx, ioctlCompleteOperation, request[:CompletionSize], request[CompletionSize:])
	return err
}

func (c *Client) SubmitInputReport(ctx context.Context, report InputReport) error {
	var metadata [InputReportSize]byte
	if err := report.marshalMetadata(metadata[:]); err != nil {
		return err
	}
	_, err := c.ioctl(ctx, ioctlSubmitInputReport, metadata[:], report.Payload)
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

func (c *Client) beginIO() (windows.Handle, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.handle == 0 || c.handle == windows.InvalidHandle {
		return windows.InvalidHandle, windows.ERROR_INVALID_HANDLE
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
	err = windows.DeviceIoControl(
		handle, code,
		inputPointer, uint32(len(input)),
		outputPointer, uint32(len(output)),
		&immediate, &request.overlapped)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, err
	}

	select {
	case result := <-request.done:
		return result.transferred, result.err
	case <-ctx.Done():
		_ = windows.CancelIoEx(handle, &request.overlapped)
		select {
		case <-request.done:
			return 0, ctx.Err()
		case <-c.pumpDone:
			var transferred uint32
			_ = windows.GetOverlappedResult(handle, &request.overlapped, &transferred, true)
			return 0, errors.Join(ctx.Err(), c.completionPumpError())
		}
	case <-c.pumpDone:
		_ = windows.CancelIoEx(handle, &request.overlapped)
		var transferred uint32
		_ = windows.GetOverlappedResult(handle, &request.overlapped, &transferred, true)
		return 0, c.completionPumpError()
	}
}

func discoverInterfacePaths() ([]string, error) {
	for attempt := 0; attempt < 4; attempt++ {
		var required uint32
		ret, _, _ := procCMGetDeviceInterfaceListSize.Call(
			uintptr(unsafe.Pointer(&required)),
			uintptr(unsafe.Pointer(&interfaceGUID)),
			0,
			cmGetDeviceInterfaceListPresent)
		if uint32(ret) != crSuccess {
			return nil, fmt.Errorf("CM_Get_Device_Interface_List_SizeW returned CONFIGRET %#x", uint32(ret))
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
		return parseMultiSZ(buffer), nil
	}
	return nil, errors.New("native UDE interface list changed repeatedly during discovery")
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
