//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// A private namespace prevents an unprivileged process from pre-creating a
	// public Global mutex and making CreateMutex open an attacker-owned object.
	// The alias and complete boundary (name plus Administrators SID) identify one
	// namespace shared by the package and broker-service transactions.
	nativeMutexNamespaceAlias = "VIIPER_NATIVE_INSTALL_NAMESPACE_V1"
	nativeMutexBoundaryName   = "VIIPER_NATIVE_INSTALL_ADMIN_BOUNDARY_V1"
	nativeMutexObjectSDDL     = "O:BAG:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)"

	nativeMutexNamespaceRaceRetries = 16
)

var (
	nativeMutexKernel32              = windows.NewLazySystemDLL("kernel32.dll")
	nativeCreateBoundaryDescriptorW  = nativeMutexKernel32.NewProc("CreateBoundaryDescriptorW")
	nativeAddSIDToBoundaryDescriptor = nativeMutexKernel32.NewProc("AddSIDToBoundaryDescriptor")
	nativeDeleteBoundaryDescriptor   = nativeMutexKernel32.NewProc("DeleteBoundaryDescriptor")
	nativeCreatePrivateNamespaceW    = nativeMutexKernel32.NewProc("CreatePrivateNamespaceW")
	nativeOpenPrivateNamespaceW      = nativeMutexKernel32.NewProc("OpenPrivateNamespaceW")
	nativeClosePrivateNamespace      = nativeMutexKernel32.NewProc("ClosePrivateNamespace")
)

type nativeMutexNamespace struct {
	boundary  windows.Handle
	namespace windows.Handle
}

// close releases the namespace before deleting the boundary descriptor. Every
// caller keeps this scope alive until after its mutex handle is closed, as the
// private-namespace contract requires for new opens and named-object lookup.
func (scope *nativeMutexNamespace) close() {
	if scope == nil {
		return
	}
	if scope.namespace != 0 {
		nativeClosePrivateNamespace.Call(uintptr(scope.namespace), 0) //nolint:errcheck
		scope.namespace = 0
	}
	if scope.boundary != 0 {
		nativeDeleteBoundaryDescriptor.Call(uintptr(scope.boundary)) //nolint:errcheck
		scope.boundary = 0
	}
}

func nativeMutexCallError(err error) error {
	if err == nil || errors.Is(err, windows.ERROR_SUCCESS) {
		return windows.ERROR_GEN_FAILURE
	}
	return err
}

func createNativeMutexBoundary() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(nativeMutexBoundaryName)
	if err != nil {
		return 0, err
	}
	result, _, callErr := nativeCreateBoundaryDescriptorW.Call(
		uintptr(unsafe.Pointer(name)), 0,
	)
	runtime.KeepAlive(name)
	if result == 0 {
		return 0, nativeMutexCallError(callErr)
	}
	boundary := windows.Handle(result)
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		nativeDeleteBoundaryDescriptor.Call(uintptr(boundary)) //nolint:errcheck
		return 0, err
	}
	result, _, callErr = nativeAddSIDToBoundaryDescriptor.Call(
		uintptr(unsafe.Pointer(&boundary)), uintptr(unsafe.Pointer(administrators)),
	)
	runtime.KeepAlive(administrators)
	if result == 0 {
		nativeDeleteBoundaryDescriptor.Call(uintptr(boundary)) //nolint:errcheck
		return 0, nativeMutexCallError(callErr)
	}
	return boundary, nil
}

// createOrOpenNativeMutexNamespace follows the documented private-namespace
// rendezvous: create with a protected DACL, or open only the namespace with the
// exact alias and Administrators boundary. A creator can disappear between
// ERROR_ALREADY_EXISTS and OpenPrivateNamespace, so retry only that absence
// race; all access and boundary failures remain fail-closed.
func createOrOpenNativeMutexNamespace() (*nativeMutexNamespace, error) {
	boundary, err := createNativeMutexBoundary()
	if err != nil {
		return nil, fmt.Errorf("create native install mutex boundary: %w", err)
	}
	scope := &nativeMutexNamespace{boundary: boundary}
	alias, err := windows.UTF16PtrFromString(nativeMutexNamespaceAlias)
	if err != nil {
		scope.close()
		return nil, err
	}
	descriptor, err := windows.SecurityDescriptorFromString(nativeMutexObjectSDDL)
	if err != nil {
		scope.close()
		return nil, fmt.Errorf("create native install namespace security descriptor: %w", err)
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}

	var lastErr error
	for attempt := 0; attempt < nativeMutexNamespaceRaceRetries; attempt++ {
		result, _, createErr := nativeCreatePrivateNamespaceW.Call(
			uintptr(unsafe.Pointer(&attributes)), uintptr(boundary),
			uintptr(unsafe.Pointer(alias)),
		)
		runtime.KeepAlive(alias)
		runtime.KeepAlive(descriptor)
		if result != 0 {
			scope.namespace = windows.Handle(result)
			return scope, nil
		}
		createErr = nativeMutexCallError(createErr)
		if !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
			scope.close()
			return nil, fmt.Errorf("create native install mutex namespace: %w", createErr)
		}

		result, _, openErr := nativeOpenPrivateNamespaceW.Call(
			uintptr(boundary), uintptr(unsafe.Pointer(alias)),
		)
		runtime.KeepAlive(alias)
		if result != 0 {
			scope.namespace = windows.Handle(result)
			return scope, nil
		}
		openErr = nativeMutexCallError(openErr)
		lastErr = openErr
		if !errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) {
			scope.close()
			return nil, fmt.Errorf("open native install mutex namespace: %w", openErr)
		}
		runtime.Gosched()
	}
	scope.close()
	return nil, fmt.Errorf("create or open native install mutex namespace after creator race: %w", lastErr)
}

func nativePrivateMutexName(objectName string) (*uint16, error) {
	if objectName == "" || strings.ContainsAny(objectName, `\\/`) {
		return nil, fmt.Errorf("invalid native private mutex object name %q", objectName)
	}
	return windows.UTF16PtrFromString(nativeMutexNamespaceAlias + `\` + objectName)
}

func nativeMutexSecurityAttributes() (*windows.SecurityAttributes, error) {
	descriptor, err := windows.SecurityDescriptorFromString(nativeMutexObjectSDDL)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}, nil
}

// createNamedNativeMutex normalizes the Win32 CreateMutex contract. A named
// mutex that already exists is a successful open: Windows returns its valid
// handle and ERROR_ALREADY_EXISTS, and WaitForSingleObject decides ownership.
func createNamedNativeMutex(
	attributes *windows.SecurityAttributes,
	name *uint16,
) (windows.Handle, error) {
	handle, err := windows.CreateMutex(attributes, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return 0, err
	}
	if handle == 0 {
		if err != nil {
			return 0, err
		}
		return 0, windows.ERROR_INVALID_HANDLE
	}
	return handle, nil
}

func nativeMutexWaitMilliseconds(timeout time.Duration) uint32 {
	if timeout <= 0 {
		return 0
	}
	milliseconds := timeout / time.Millisecond
	if milliseconds >= time.Duration(windows.INFINITE) {
		return windows.INFINITE - 1
	}
	return uint32(milliseconds)
}

// acquireNativeNamedMutex preserves Win32's thread-affine mutex ownership by
// pinning the goroutine through the returned release closure. Mutex ownership
// and its handle, the namespace, the boundary, and the thread pin are released
// in that order.
func acquireNativeNamedMutex(
	objectName string,
	timeout time.Duration,
	busyMessage string,
) (func(), error) {
	name, err := nativePrivateMutexName(objectName)
	if err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	scope, err := createOrOpenNativeMutexNamespace()
	if err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	attributes, err := nativeMutexSecurityAttributes()
	if err != nil {
		scope.close()
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("create native mutex security descriptor: %w", err)
	}
	handle, err := createNamedNativeMutex(attributes, name)
	runtime.KeepAlive(attributes.SecurityDescriptor)
	if err != nil {
		scope.close()
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("create protected native mutex: %w", err)
	}
	status, err := windows.WaitForSingleObject(handle, nativeMutexWaitMilliseconds(timeout))
	if err != nil || (status != windows.WAIT_OBJECT_0 && status != windows.WAIT_ABANDONED) {
		windows.CloseHandle(handle) //nolint:errcheck
		scope.close()
		runtime.UnlockOSThread()
		if err != nil {
			return nil, fmt.Errorf("wait for protected native mutex: %w", err)
		}
		return nil, errors.New(busyMessage)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			windows.ReleaseMutex(handle) //nolint:errcheck
			windows.CloseHandle(handle)  //nolint:errcheck
			scope.close()
			runtime.UnlockOSThread()
		})
	}, nil
}

// nativeNamedMutexHeldByAnotherOwner opens only the exact object inside the
// Administrators namespace. WAIT_TIMEOUT proves another thread/process owns
// it; an absent, abandoned, or immediately acquirable mutex proves no live
// owner. This is the cross-process nested package-commit probe.
func nativeNamedMutexHeldByAnotherOwner(objectName string) (bool, error) {
	name, err := nativePrivateMutexName(objectName)
	if err != nil {
		return false, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	scope, err := createOrOpenNativeMutexNamespace()
	if err != nil {
		return false, err
	}
	defer scope.close()
	handle, err := windows.OpenMutex(
		windows.SYNCHRONIZE|windows.MUTEX_MODIFY_STATE, false, name,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return false, nil
		}
		return false, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	status, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, err
	}
	switch status {
	case uint32(windows.WAIT_TIMEOUT):
		return true, nil
	case uint32(windows.WAIT_OBJECT_0), uint32(windows.WAIT_ABANDONED):
		windows.ReleaseMutex(handle) //nolint:errcheck
		return false, nil
	default:
		return false, fmt.Errorf("unexpected protected native mutex wait status: 0x%08x", status)
	}
}
