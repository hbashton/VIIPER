package udecx

import (
	"strings"
	"testing"
)

func nativeCFunction(t *testing.T, source, name string) string {
	t.Helper()
	start := strings.Index(source, "\n"+name+"(")
	if start < 0 {
		t.Fatalf("native function %s is missing", name)
	}
	start++
	openOffset := strings.IndexByte(source[start:], '{')
	if openOffset < 0 {
		t.Fatalf("native function %s has no body", name)
	}
	open := start + openOffset
	depth := 0
	for index := open; index < len(source); index++ {
		switch source[index] {
		case '/':
			if index+1 >= len(source) {
				continue
			}
			switch source[index+1] {
			case '/':
				newline := strings.IndexByte(source[index+2:], '\n')
				if newline < 0 {
					t.Fatalf("native function %s has an unterminated line comment", name)
				}
				index += newline + 2
			case '*':
				closeComment := strings.Index(source[index+2:], "*/")
				if closeComment < 0 {
					t.Fatalf("native function %s has an unterminated block comment", name)
				}
				index += closeComment + 3
			}
		case '\'', '"':
			quote := source[index]
			for index++; index < len(source); index++ {
				if source[index] == '\\' {
					index++
					continue
				}
				if source[index] == quote {
					break
				}
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start : index+1]
			}
		}
	}
	t.Fatalf("native function %s has an unterminated body", name)
	return ""
}

func normalizedContract(source string) string {
	return strings.Join(strings.Fields(source), " ")
}

func requireContractOrder(t *testing.T, source string, fragments ...string) {
	t.Helper()
	cursor := 0
	for _, fragment := range fragments {
		offset := strings.Index(source[cursor:], fragment)
		if offset < 0 {
			t.Fatalf("native contract lost ordered fragment %q in:\n%s", fragment, source)
		}
		cursor += offset + len(fragment)
	}
}

func TestKernelOwnerCleanupJoinsFiniteMutationRundown(t *testing.T) {
	controller := nativeContractSource(t, "native", "udecx", "driver", "Controller.c")
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")
	all := controller + device + header
	for _, obsolete := range []string{"OwnerCleanupTimer", "ViiperEvtOwnerCleanupRetry"} {
		if strings.Contains(all, obsolete) {
			t.Fatalf("owner cleanup still depends on unbounded retry state %q", obsolete)
		}
	}
	if !strings.Contains(header, "KEVENT OwnerAdmissionsDrained;") {
		t.Fatal("controller lost the finite owner-admission rundown event")
	}
	if !strings.Contains(controller,
		"KeInitializeEvent(&context->OwnerAdmissionsDrained, NotificationEvent, TRUE);") {
		t.Fatal("owner-admission rundown must start signaled")
	}

	begin := normalizedContract(nativeCFunction(t, device, "ViiperBeginOwnerAdmission"))
	requireContractOrder(t, begin,
		"WdfWaitLockAcquire(controllerContext->OwnerLock, NULL);",
		"InterlockedIncrement(&controllerContext->ActiveOwnerAdmissions) == 1",
		"KeClearEvent(&controllerContext->OwnerAdmissionsDrained);",
		"WdfWaitLockRelease(controllerContext->OwnerLock);")
	end := normalizedContract(nativeCFunction(t, device, "ViiperEndOwnerAdmission"))
	requireContractOrder(t, end,
		"InterlockedDecrement(&controllerContext->ActiveOwnerAdmissions);",
		"if (remaining == 0)",
		"KeSetEvent(&controllerContext->OwnerAdmissionsDrained, IO_NO_INCREMENT, FALSE);",
		"WdfWaitLockRelease(controllerContext->OwnerLock);",
		"WdfObjectDereference(OwnerFile);")

	finish := normalizedContract(nativeCFunction(t, controller, "ViiperFinishOwnerCleanup"))
	requireContractOrder(t, finish,
		"WdfWaitLockRelease(context->OwnerLock);",
		"KeWaitForSingleObject( &context->OwnerAdmissionsDrained",
		"ViiperDestroyOwnedDevices(Device, OwnerFile)",
		"context->OwnerFile = WDF_NO_HANDLE;",
		"WdfObjectDereference(OwnerFile);")

	for _, name := range []string{"ViiperCreateVirtualDevice", "ViiperDestroyVirtualDevice"} {
		mutation := normalizedContract(nativeCFunction(t, device, name))
		requireContractOrder(t, mutation,
			"ViiperBeginOwnerAdmission(controller, Request, &ownerFile)",
			"ViiperEndOwnerAdmission(controller, ownerFile);")
	}
}

func TestKernelDelayedCleanupCannotBlockOrRevokeReusedSlot(t *testing.T) {
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")
	if strings.Contains(device+header, "RemovingSlots") {
		t.Fatal("logical slot reuse still depends on asynchronous child cleanup")
	}

	claim := normalizedContract(nativeCFunction(t, device, "ViiperClaimDeviceSlot"))
	requireContractOrder(t, claim,
		"if (current == WDF_NO_HANDLE)",
		"freeSlot = index;",
		"ControllerContext->Devices[freeSlot] = Device;")
	release := normalizedContract(nativeCFunction(t, device, "ViiperReleaseDeviceSlot"))
	requireContractOrder(t, release,
		"if (ControllerContext->Devices[Slot] == Device)",
		"ControllerContext->Devices[Slot] = WDF_NO_HANDLE;")

	remove := normalizedContract(nativeCFunction(t, device, "ViiperBeginRemoveDevice"))
	requireContractOrder(t, remove,
		"InterlockedExchange(&deviceContext->Purging, TRUE);",
		"ControllerContext->Devices[index] = WDF_NO_HANDLE;",
		"ViiperRetireActiveDevice(ControllerContext, deviceContext);",
		"*Device = current;")
	cleanup := normalizedContract(nativeCFunction(t, device, "ViiperEvtVirtualDeviceCleanup"))
	requireContractOrder(t, cleanup,
		"InterlockedExchange(&deviceContext->OwnerReferenced, 0)",
		"ViiperReleaseDeviceSlot(controllerContext, device, deviceContext->Slot);",
		"ViiperRetireActiveDevice(controllerContext, deviceContext);",
		"WdfObjectDereference(ownerFile);")

	destroyOwned := nativeCFunction(t, device, "ViiperDestroyOwnedDevices")
	for _, forbidden := range []string{"EvtVirtualDeviceCleanup", "ActiveDevices", "CleanupRetries"} {
		if strings.Contains(destroyOwned, forbidden) {
			t.Fatalf("logical owner release still waits on physical cleanup state %q", forbidden)
		}
	}
}

func TestKernelStaleChildCannotNotifySuccessorOwner(t *testing.T) {
	broker := nativeContractSource(t, "native", "udecx", "driver", "Broker.c")
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")

	ownerGate := normalizedContract(nativeCFunction(
		t, broker, "ViiperLifecycleOwnerSessionActiveLocked"))
	requireContractOrder(t, ownerGate,
		"InterlockedCompareExchange(&DeviceContext->Purging, 0, 0) != 0",
		"InterlockedCompareExchange(&DeviceContext->OwnerReferenced, 0, 0) == 0",
		"ownerFile = DeviceContext->OwnerFile;",
		"if (ownerFile == WDF_NO_HANDLE)",
		"fileContext = ViiperGetFileContext(ownerFile);",
		"InterlockedCompareExchange(&fileContext->BrokerOwner, 0, 0) != 0",
		"InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) != 0",
		"InterlockedCompareExchange(&fileContext->Closing, 0, 0) == 0")
	if strings.Contains(ownerGate, "WdfWaitLockAcquire(") {
		t.Fatalf("lifecycle owner gate reverses cleanup lock order: %s", ownerGate)
	}

	insert := normalizedContract(nativeCFunction(t, broker, "ViiperQueueLifecycleEventLocked"))
	requireContractOrder(t, insert,
		"if (!ViiperLifecycleOwnerSessionActiveLocked(DeviceContext))",
		"event = &ControllerContext->Notifications[ControllerContext->NotificationTail];",
		"InterlockedIncrement64( &DeviceContext->EndpointSequences[event->EndpointAddress])",
		"InterlockedIncrement64( &DeviceContext->DeviceSequence)")

	for _, name := range []string{
		"ViiperQueueEndpointLifecycleEvent",
		"ViiperQueueDeviceLifecycleEvent",
		"ViiperQueueInterfaceLifecycleEvent",
		"ViiperQueueAcknowledgedLifecycleEvent",
	} {
		producer := normalizedContract(nativeCFunction(t, broker, name))
		requireContractOrder(t, producer,
			"WdfSpinLockAcquire(controllerContext->BrokerLock);",
			"ViiperLifecycleOwnerSessionActiveLocked(deviceContext)",
			"ViiperQueueLifecycleEventLocked(",
			"WdfSpinLockRelease(controllerContext->BrokerLock);")
	}

	remove := normalizedContract(nativeCFunction(t, device, "ViiperBeginRemoveDevice"))
	requireContractOrder(t, remove,
		"WdfSpinLockAcquire(ControllerContext->BrokerLock);",
		"InterlockedExchange(&deviceContext->Purging, TRUE);",
		"WdfSpinLockRelease(ControllerContext->BrokerLock);",
		"ControllerContext->Devices[index] = WDF_NO_HANDLE;")

	cleanup := normalizedContract(nativeCFunction(t, device, "ViiperEvtVirtualDeviceCleanup"))
	requireContractOrder(t, cleanup,
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"InterlockedExchange(&deviceContext->Purging, TRUE);",
		"InterlockedExchange(&deviceContext->OwnerReferenced, 0)",
		"ownerFile = deviceContext->OwnerFile;",
		"deviceContext->OwnerFile = WDF_NO_HANDLE;",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"ViiperReleaseDeviceSlot(controllerContext, device, deviceContext->Slot);",
		"if (ownerFile != WDF_NO_HANDLE)",
		"WdfObjectDereference(ownerFile);")
}

func TestKernelNeverUsesConsumedUDEDeviceHandle(t *testing.T) {
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")

	destroy := normalizedContract(nativeCFunction(t, device, "ViiperDestroyVirtualDevice"))
	requireContractOrder(t, destroy,
		"ViiperBeginRemoveDevice(",
		"UdecxUsbDevicePlugOutAndDelete(device);",
		"ViiperEndOwnerAdmission(controller, ownerFile);")
	if suffix := destroy[strings.Index(destroy, "UdecxUsbDevicePlugOutAndDelete(device);")+len("UdecxUsbDevicePlugOutAndDelete(device);"):]; strings.Contains(suffix, "ViiperGetDeviceContext(device)") ||
		strings.Contains(suffix, "WdfObjectDelete(device)") {
		t.Fatalf("destroy path uses consumed UDE handle after PlugOutAndDelete: %s", suffix)
	}

	destroyOwned := normalizedContract(nativeCFunction(t, device, "ViiperDestroyOwnedDevices"))
	requireContractOrder(t, destroyOwned,
		"deviceContext = ViiperGetDeviceContext(device);",
		"plugged = deviceContext->Plugged;",
		"ViiperAbortDeviceManagementOperations(Controller, device, STATUS_FILE_CLOSED);",
		"if (plugged)",
		"UdecxUsbDevicePlugOutAndDelete(device)",
		"return FALSE;")
	assertNoConsumedHandleUse(t, destroyOwned, "UdecxUsbDevicePlugOutAndDelete(device)", "} else {")
	shutdown := normalizedContract(nativeCFunction(t, device, "ViiperBeginControllerShutdown"))
	requireContractOrder(t, shutdown,
		"VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(devices[index]);",
		"if (deviceContext->Plugged)",
		"UdecxUsbDevicePlugOutAndDelete(devices[index]);")
	assertNoConsumedHandleUse(t, shutdown,
		"UdecxUsbDevicePlugOutAndDelete(devices[index]);", "} else {")
}

func assertNoConsumedHandleUse(t *testing.T, source, call, branchEnd string) {
	t.Helper()
	callAt := strings.Index(source, call)
	if callAt < 0 {
		t.Fatalf("native contract lost consuming call %q", call)
	}
	afterCall := source[callAt+len(call):]
	endAt := strings.Index(afterCall, branchEnd)
	if endAt < 0 {
		t.Fatalf("native contract lost branch end %q after %q", branchEnd, call)
	}
	for _, forbidden := range []string{
		"ViiperGetDeviceContext", "WdfObjectDelete", "WdfObjectReference",
		"WdfObjectDereference",
	} {
		if strings.Contains(afterCall[:endAt], forbidden) {
			t.Fatalf("consumed UDE handle branch calls %s after %s: %s",
				forbidden, call, afterCall[:endAt])
		}
	}
}
