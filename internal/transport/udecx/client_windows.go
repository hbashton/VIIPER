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
	crSuccess                       = 0
	crBufferSmall                   = 0x1a
	cmGetDeviceInterfaceListPresent = 0
	fileDeviceUnknown               = 0x22
	methodBuffered                  = 0
	methodInDirect                  = 1
	methodOutDirect                 = 2
	fileReadData                    = 1
	fileWriteData                   = 2
	ioctlBase                       = 0x900
	ioctlNegotiate                  = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 0) << 2) | methodBuffered
	ioctlCreateDevice               = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 1) << 2) | methodBuffered
	ioctlDestroyDevice              = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 2) << 2) | methodBuffered
	ioctlDequeueOperation           = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 3) << 2) | methodOutDirect
	ioctlCompleteOperation          = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | ((ioctlBase + 4) << 2) | methodInDirect
	ioctlQueryStats                 = (fileDeviceUnknown << 16) | (fileReadData << 14) | ((ioctlBase + 5) << 2) | methodBuffered
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
	mu           sync.RWMutex
	handle       windows.Handle
	driverNonce  uint64
	capabilities Capabilities
	limits       NegotiateResponse
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

	client := &Client{handle: handle}
	if err = client.negotiate(ctx); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return client, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handle == 0 || c.handle == windows.InvalidHandle {
		return nil
	}
	handle := c.handle
	c.handle = windows.InvalidHandle
	_ = windows.CancelIoEx(handle, nil)
	return windows.CloseHandle(handle)
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
		RequestedCapabilities: CapabilityIsochronous | CapabilityDeviceLifecycle,
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
	if negotiated.ClientNonce != nonce || negotiated.DriverNonce == 0 {
		return errors.New("validate native UDE negotiation: session nonce mismatch")
	}
	if negotiated.MaxDevices == 0 || negotiated.MaxDescriptorBytes == 0 ||
		negotiated.MaxTransferBytes == 0 || negotiated.MaxIsoPackets == 0 ||
		negotiated.MaxPendingOperations == 0 {
		return errors.New("validate native UDE negotiation: driver returned a zero limit")
	}
	c.driverNonce = negotiated.DriverNonce
	c.capabilities = negotiated.Capabilities
	c.limits = negotiated
	return nil
}

func (c *Client) ioctl(ctx context.Context, code uint32, input, output []byte) (uint32, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	handle := c.handle
	if handle == 0 || handle == windows.InvalidHandle {
		return 0, windows.ERROR_INVALID_HANDLE
	}

	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(event)
	overlapped := windows.Overlapped{HEvent: event}
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
		&immediate, &overlapped)
	if err == nil {
		var transferred uint32
		err = windows.GetOverlappedResult(handle, &overlapped, &transferred, false)
		runtime.KeepAlive(input)
		runtime.KeepAlive(output)
		return transferred, err
	}
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, err
	}

	done := make(chan struct{})
	var transferred uint32
	var resultErr error
	go func() {
		resultErr = windows.GetOverlappedResult(handle, &overlapped, &transferred, true)
		close(done)
	}()
	select {
	case <-ctx.Done():
		_ = windows.CancelIoEx(handle, &overlapped)
		<-done
		return 0, ctx.Err()
	case <-done:
		runtime.KeepAlive(input)
		runtime.KeepAlive(output)
		return transferred, resultErr
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
