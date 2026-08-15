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

func TestKernelDelayedCleanupReservesPhysicalPortAndCannotRevokeSuccessor(t *testing.T) {
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")
	for _, required := range []string{
		"ULONGLONG PortReservationEpochs[VIIPER_UDE_MAX_DEVICES];",
		"BOOLEAN PortReserved[VIIPER_UDE_MAX_DEVICES];",
		"volatile LONG ReservedPorts;",
		"ULONGLONG PortReservation;",
	} {
		if !strings.Contains(header, required) {
			t.Fatalf("physical port reservation contract missing %q", required)
		}
	}

	claim := normalizedContract(nativeCFunction(t, device, "ViiperClaimDeviceSlot"))
	requireContractOrder(t, claim,
		"if (current == WDF_NO_HANDLE)",
		"if (!ControllerContext->PortReserved[index] && freeSlot == VIIPER_UDE_MAX_DEVICES)",
		"freeSlot = index;",
		"ControllerContext->PortReserved[freeSlot] = TRUE;",
		"InterlockedIncrement(&ControllerContext->ReservedPorts);",
		"ControllerContext->Devices[freeSlot] = Device;",
		"*PortReservation = reservation;")
	release := normalizedContract(nativeCFunction(t, device, "ViiperReleaseDeviceSlot"))
	requireContractOrder(t, release,
		"ControllerContext->PortReservationEpochs[Slot] == PortReservation",
		"if (ControllerContext->Devices[Slot] == Device)",
		"ControllerContext->Devices[Slot] = WDF_NO_HANDLE;",
		"ControllerContext->PortReserved[Slot] = FALSE;",
		"InterlockedDecrement(&ControllerContext->ReservedPorts);",
		"NT_ASSERT(remaining >= 0);")

	controller := nativeContractSource(t, "native", "udecx", "driver", "Controller.c")
	controllerCleanup := normalizedContract(nativeCFunction(
		t, controller, "ViiperEvtControllerCleanup"))
	requireContractOrder(t, controllerCleanup,
		"context->ActiveDevices",
		"context->ReservedPorts",
		"context->InputDeviceCount == 0",
		"for (index = 0; index < VIIPER_UDE_MAX_DEVICES; ++index)",
		"NT_ASSERT(!context->PortReserved[index]);")

	remove := normalizedContract(nativeCFunction(t, device, "ViiperBeginRemoveDevice"))
	requireContractOrder(t, remove,
		"InterlockedExchange(&deviceContext->Purging, TRUE);",
		"ControllerContext->Devices[index] = WDF_NO_HANDLE;",
		"ViiperRetireActiveDevice(ControllerContext, deviceContext);",
		"*Device = current;")
	if strings.Contains(remove, "PortReserved") {
		t.Fatal("logical removal releases the physical port before framework cleanup")
	}
	cleanup := normalizedContract(nativeCFunction(t, device, "ViiperEvtVirtualDeviceCleanup"))
	requireContractOrder(t, cleanup,
		"InterlockedExchange(&deviceContext->OwnerReferenced, 0)",
		"ViiperReleaseDeviceSlot( controllerContext, device, deviceContext->Slot, deviceContext->PortReservation);",
		"ViiperRetireActiveDevice(controllerContext, deviceContext);",
		"WdfObjectDereference(ownerFile);")

	destroyOwned := nativeCFunction(t, device, "ViiperDestroyOwnedDevices")
	for _, forbidden := range []string{"EvtVirtualDeviceCleanup", "ActiveDevices", "CleanupRetries"} {
		if strings.Contains(destroyOwned, forbidden) {
			t.Fatalf("logical owner release still waits on physical cleanup state %q", forbidden)
		}
	}
}

func TestKernelPlugInPublishesCleanupAccountingBeforeUdeCxExposure(t *testing.T) {
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")

	claim := normalizedContract(nativeCFunction(t, device, "ViiperClaimDeviceSlot"))
	requireContractOrder(t, claim,
		"ViiperAcquireDeviceLockExclusive(ControllerContext);",
		"deviceContext->Slot = freeSlot;",
		"deviceContext->PortReservation = reservation;",
		"deviceContext->Plugged = TRUE;",
		"ControllerContext->PortReserved[freeSlot] = TRUE;",
		"ControllerContext->Devices[freeSlot] = Device;",
		"InterlockedIncrement(&ControllerContext->ActiveDevices);",
		"InterlockedExchange(&deviceContext->ActiveCounted, 1);",
		"ViiperReleaseDeviceLockExclusive(ControllerContext);")

	create := normalizedContract(nativeCFunction(t, device, "ViiperCreateVirtualDevice"))
	requireContractOrder(t, create,
		"deviceId = input->DeviceId;",
		"ViiperClaimDeviceSlot( controllerContext, device, deviceId, &slot, &portReservation);",
		"status = UdecxUsbDevicePlugIn(device, &plugOptions);",
		"if (!NT_SUCCESS(status))",
		"ViiperReleaseDeviceSlot(controllerContext, device, slot, portReservation);",
		"ViiperRetireActiveDevice(controllerContext, deviceContext);",
		"WdfObjectDelete(device);",
		"goto ExitAdmission;")

	plugIn := "status = UdecxUsbDevicePlugIn(device, &plugOptions);"
	postPlugIn := create[strings.Index(create, plugIn)+len(plugIn):]
	for _, forbidden := range []string{
		"deviceContext->Plugged",
		"deviceContext->ActiveCounted",
		"controllerContext->ActiveDevices",
	} {
		if strings.Contains(postPlugIn, forbidden) {
			t.Fatalf("PlugIn publication still mutates %q after UdeCx exposure: %s",
				forbidden, postPlugIn)
		}
	}
	failureBoundary := strings.Index(postPlugIn, "if (!NT_SUCCESS(status))")
	if failureBoundary < 0 {
		t.Fatal("PlugIn failure rollback is missing")
	}
	if strings.Contains(postPlugIn[:failureBoundary], "deviceContext->") {
		t.Fatalf("PlugIn return tracing accesses context after UdeCx exposure: %s",
			postPlugIn[:failureBoundary])
	}
}

type modeledPortDevice struct {
	slot   int
	token  uint64
	active bool
}

type modeledPortController struct {
	devices       [4]*modeledPortDevice
	epochs        [4]uint64
	reserved      [4]bool
	active        int
	reservedPorts int
}

func (controller *modeledPortController) claim(device *modeledPortDevice) bool {
	for slot := range controller.devices {
		if controller.devices[slot] != nil || controller.reserved[slot] {
			continue
		}
		controller.epochs[slot]++
		if controller.epochs[slot] == 0 {
			controller.epochs[slot]++
		}
		device.slot = slot
		device.token = controller.epochs[slot]
		device.active = true
		controller.reserved[slot] = true
		controller.reservedPorts++
		controller.devices[slot] = device
		controller.active++
		return true
	}
	return false
}

func (controller *modeledPortController) retire(device *modeledPortDevice) {
	if !device.active {
		return
	}
	device.active = false
	controller.active--
}

func (controller *modeledPortController) logicalRemove(device *modeledPortDevice) {
	if device.slot >= 0 && device.slot < len(controller.devices) &&
		controller.devices[device.slot] == device {
		controller.devices[device.slot] = nil
		controller.retire(device)
	}
}

func (controller *modeledPortController) release(
	device *modeledPortDevice,
	slot int,
	token uint64,
) {
	if slot < 0 || slot >= len(controller.devices) || token == 0 ||
		!controller.reserved[slot] || controller.epochs[slot] != token {
		return
	}
	if controller.devices[slot] == device {
		controller.devices[slot] = nil
	}
	controller.reserved[slot] = false
	controller.reservedPorts--
}

func (controller *modeledPortController) cleanup(device *modeledPortDevice) {
	controller.release(device, device.slot, device.token)
	controller.retire(device)
}

func TestPortReservationAndActiveAccountingModel(t *testing.T) {
	t.Run("normal remove holds the exact port until cleanup", func(t *testing.T) {
		controller := new(modeledPortController)
		first := &modeledPortDevice{slot: -1}
		second := &modeledPortDevice{slot: -1}
		if !controller.claim(first) {
			t.Fatal("first claim failed")
		}
		controller.logicalRemove(first)
		if controller.active != 0 || !controller.reserved[first.slot] ||
			controller.reservedPorts != 1 {
			t.Fatalf("logical remove lost teardown state: active=%d reserved=%v ports=%d",
				controller.active, controller.reserved[first.slot], controller.reservedPorts)
		}
		if !controller.claim(second) || second.slot == first.slot {
			t.Fatalf("successor reused reserved port: first=%d second=%d",
				first.slot, second.slot)
		}
		controller.cleanup(first)
		controller.cleanup(first)
		if controller.active != 1 || controller.reservedPorts != 1 ||
			controller.reserved[first.slot] ||
			controller.devices[second.slot] != second {
			t.Fatalf("exact cleanup disturbed successor: active=%d ports=%d first_reserved=%v",
				controller.active, controller.reservedPorts, controller.reserved[first.slot])
		}
	})

	t.Run("failed PlugIn rollback cannot revoke its successor", func(t *testing.T) {
		controller := new(modeledPortController)
		failed := &modeledPortDevice{slot: -1}
		successor := &modeledPortDevice{slot: -1}
		if !controller.claim(failed) {
			t.Fatal("failed-device claim failed")
		}
		failedSlot, failedToken := failed.slot, failed.token
		controller.release(failed, failedSlot, failedToken)
		controller.retire(failed)
		if controller.reservedPorts != 0 {
			t.Fatalf("failed PlugIn release leaked %d reserved ports", controller.reservedPorts)
		}
		if !controller.claim(successor) || successor.slot != failedSlot ||
			successor.token == failedToken {
			t.Fatalf("successor identity did not advance: failed=(%d,%d) successor=(%d,%d)",
				failedSlot, failedToken, successor.slot, successor.token)
		}
		controller.cleanup(failed)
		if controller.active != 1 || controller.reservedPorts != 1 ||
			!controller.reserved[successor.slot] ||
			controller.devices[successor.slot] != successor {
			t.Fatal("late failed-device cleanup revoked the successor")
		}
	})

	t.Run("controller shutdown retires logic before exact physical cleanup", func(t *testing.T) {
		controller := new(modeledPortController)
		devices := []*modeledPortDevice{{slot: -1}, {slot: -1}, {slot: -1}}
		for _, device := range devices {
			if !controller.claim(device) {
				t.Fatal("shutdown fixture claim failed")
			}
		}
		for _, device := range devices {
			controller.logicalRemove(device)
		}
		if controller.active != 0 || controller.reservedPorts != len(devices) {
			t.Fatalf("logical shutdown state active=%d reserved=%d",
				controller.active, controller.reservedPorts)
		}
		for _, device := range devices {
			if !controller.reserved[device.slot] {
				t.Fatalf("shutdown released port %d before cleanup", device.slot)
			}
			controller.cleanup(device)
		}
		if controller.active != 0 || controller.reservedPorts != 0 {
			t.Fatalf("terminal cleanup state active=%d reserved=%d",
				controller.active, controller.reservedPorts)
		}
	})

	t.Run("unexpected and invalid cleanup is idempotent", func(t *testing.T) {
		controller := new(modeledPortController)
		device := &modeledPortDevice{slot: -1}
		if !controller.claim(device) {
			t.Fatal("unexpected-cleanup fixture claim failed")
		}
		controller.release(device, -1, device.token)
		controller.release(device, device.slot, 0)
		controller.release(device, device.slot, device.token+1)
		if controller.active != 1 || controller.reservedPorts != 1 ||
			!controller.reserved[device.slot] {
			t.Fatal("invalid token mutated live reservation")
		}
		controller.cleanup(device)
		controller.cleanup(device)
		if controller.active != 0 || controller.reservedPorts != 0 ||
			controller.reserved[device.slot] ||
			controller.devices[device.slot] != nil {
			t.Fatalf("unexpected cleanup was not idempotent: active=%d", controller.active)
		}
	})
}

func TestControllerRestartPreservesOutstandingPortReservationEpochs(t *testing.T) {
	controller := nativeContractSource(t, "native", "udecx", "driver", "Controller.c")
	deviceAdd := normalizedContract(nativeCFunction(t, controller, "ViiperEvtDeviceAdd"))
	if !strings.Contains(deviceAdd, "RtlZeroMemory(context, sizeof(*context));") {
		t.Fatal("controller context is not initialized exactly at device creation")
	}
	selfManagedInit := nativeCFunction(t, controller, "ViiperEvtDeviceSelfManagedIoInit")
	for _, forbidden := range []string{"RtlZeroMemory", "PortReservationEpochs", "PortReserved"} {
		if strings.Contains(selfManagedInit, forbidden) {
			t.Fatalf("same-object restart resets outstanding reservation state %q: %s",
				forbidden, selfManagedInit)
		}
	}
}

func TestDispatchLevelPowerAndResetCallbacksDeferPassiveInvalidation(t *testing.T) {
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")

	for _, required := range []string{
		"WDFWORKITEM D0ExitWorkItem;",
		"volatile LONG D0ExitPending;",
		"EVT_WDF_WORKITEM ViiperEvtUsbDeviceD0ExitWorkItem;",
	} {
		if !strings.Contains(header, required) {
			t.Fatalf("device power deferral contract missing %q", required)
		}
	}

	create := normalizedContract(nativeCFunction(t, device, "ViiperCreateVirtualDevice"))
	requireContractOrder(t, create,
		"WDF_WORKITEM_CONFIG_INIT(&workItemConfig, ViiperEvtUsbDeviceD0ExitWorkItem);",
		"workItemConfig.AutomaticSerialization = WdfFalse;",
		"attributes.ParentObject = device;",
		"WdfWorkItemCreate( &workItemConfig, &attributes, &deviceContext->D0ExitWorkItem);",
		"ViiperClaimDeviceSlot(",
		"UdecxUsbDevicePlugIn(device, &plugOptions);")

	d0Exit := normalizedContract(nativeCFunction(t, device, "ViiperEvtUsbDeviceD0Exit"))
	requireContractOrder(t, d0Exit,
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"InterlockedExchange(&deviceContext->InD0, FALSE);",
		"controllerContext->ShuttingDown",
		"deviceContext->Purging",
		"status = STATUS_SUCCESS;",
		"&deviceContext->D0ExitPending, TRUE, FALSE",
		"status = STATUS_DEVICE_BUSY;",
		"WdfWorkItemEnqueue(deviceContext->D0ExitWorkItem);",
		"status = STATUS_PENDING;",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"return status;")
	assertNoDispatchLevelWaits(t, "D0 exit", d0Exit)

	reset := normalizedContract(nativeCFunction(t, device, "ViiperEvtEndpointReset"))
	requireContractOrder(t, reset,
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"&endpointContext->Resetting, TRUE, FALSE",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"ViiperPurgeEndpointOperations(Endpoint, STATUS_DEVICE_NOT_READY);",
		"endpointContext->ResetRequest = Request;",
		"WdfWorkItemEnqueue(endpointContext->ResetWorkItem);")
	assertNoDispatchLevelWaits(t, "endpoint reset", reset)

	d0Work := normalizedContract(nativeCFunction(
		t, device, "ViiperEvtUsbDeviceD0ExitWorkItem"))
	requireContractOrder(t, d0Work,
		"NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);",
		"ViiperInvalidateDeviceInputReports(device);",
		"ViiperQueueDeviceLifecycleEvent( device, ViiperUdeOperationDeviceD0Exit);",
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"InterlockedExchange(&deviceContext->D0ExitPending, FALSE);",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"UdecxUsbDeviceLinkPowerExitComplete(device, STATUS_SUCCESS);")
	completion := "UdecxUsbDeviceLinkPowerExitComplete(device, STATUS_SUCCESS);"
	afterCompletion := d0Work[strings.Index(d0Work, completion)+len(completion):]
	for _, forbidden := range []string{
		"deviceContext", "controllerContext", "ViiperGet", "Wdf", "VIIPER_TRACE",
	} {
		if strings.Contains(afterCompletion, forbidden) {
			t.Fatalf("D0-exit work item accesses the device after UdeCx completion via %q: %s",
				forbidden, afterCompletion)
		}
	}

	flush := normalizedContract(nativeCFunction(t, device, "ViiperFlushD0ExitWorkItem"))
	requireContractOrder(t, flush,
		"NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);",
		"WdfWorkItemFlush(deviceContext->D0ExitWorkItem);",
		"deviceContext->D0ExitPending")
	if strings.Contains(flush, "if (InterlockedCompareExchange") {
		t.Fatal("teardown conditionally skips the D0-exit flush after the pending flag clears")
	}

	destroy := normalizedContract(nativeCFunction(t, device, "ViiperDestroyVirtualDevice"))
	requireContractOrder(t, destroy,
		"ViiperBeginRemoveDevice(",
		"ViiperFlushD0ExitWorkItem(device);",
		"ViiperAbortDeviceManagementOperations(controller, device, STATUS_DEVICE_REMOVED);",
		"UdecxUsbDevicePlugOutAndDelete(device);")
	destroyOwned := normalizedContract(nativeCFunction(t, device, "ViiperDestroyOwnedDevices"))
	requireContractOrder(t, destroyOwned,
		"ViiperBeginRemoveDevice(",
		"ViiperFlushD0ExitWorkItem(device);",
		"ViiperAbortDeviceManagementOperations(Controller, device, STATUS_FILE_CLOSED);",
		"UdecxUsbDevicePlugOutAndDelete(device)")
	controllerShutdown := normalizedContract(nativeCFunction(t, device, "ViiperBeginControllerShutdown"))
	requireContractOrder(t, controllerShutdown,
		"devices[deviceCount++] = device;",
		"ViiperReleaseDeviceLockExclusive(controllerContext);",
		"ViiperFlushD0ExitWorkItem(devices[index]);",
		"UdecxUsbDevicePlugOutAndDelete(devices[index]);")

	resetWork := normalizedContract(nativeCFunction(
		t, device, "ViiperEvtEndpointResetWorkItem"))
	requireContractOrder(t, resetWork,
		"NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);",
		"ViiperQuiesceResetByIdentity(",
		"if (!resetCurrent)",
		"ViiperInvalidateEndpointInputReport(endpoint);",
		"ViiperQueueAcknowledgedEndpointLifecycleEvent(")

	d0Entry := normalizedContract(nativeCFunction(t, device, "ViiperEvtUsbDeviceD0Entry"))
	requireContractOrder(t, d0Entry,
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"deviceContext->Purging",
		"deviceContext->D0ExitPending",
		"status = STATUS_DEVICE_BUSY;",
		"InterlockedExchange(&deviceContext->InD0, TRUE);",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"ViiperQueueDeviceLifecycleEvent(Device, ViiperUdeOperationDeviceD0Entry);")
	assertNoDispatchLevelWaits(t, "D0 entry", d0Entry)

	activate := normalizedContract(nativeCFunction(t, device, "ViiperActivateEndpoint"))
	requireContractOrder(t, activate,
		"deviceContext->Purging",
		"deviceContext->InD0",
		"deviceContext->D0ExitPending",
		"InterlockedExchange(&endpointContext->Purging, FALSE);")
}

func assertNoDispatchLevelWaits(t *testing.T, name, body string) {
	t.Helper()
	for _, forbidden := range []string{
		"ViiperInvalidateEndpointInputReport",
		"ViiperInvalidateDeviceInputReports",
		"ViiperAcquireDeviceLock",
		"WdfWaitLockAcquire",
		"KeWaitForSingleObject",
		"KeDelayExecutionThread",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("%s performs passive-only work through %q: %s", name, forbidden, body)
		}
	}
}

type modeledDevicePower struct {
	inD0            bool
	exitPending     bool
	cacheValid      bool
	workEnqueues    int
	exitCompletions int
}

func (device *modeledDevicePower) beginExit() bool {
	if device.exitPending {
		return false
	}
	device.exitPending = true
	device.inD0 = false
	device.workEnqueues++
	return true
}

func (device *modeledDevicePower) completeExitWork() bool {
	if !device.exitPending {
		return false
	}
	device.cacheValid = false
	device.exitPending = false
	device.exitCompletions++
	return true
}

func (device *modeledDevicePower) enterD0() bool {
	if device.exitPending {
		return false
	}
	device.inD0 = true
	return true
}

func (device *modeledDevicePower) canAdmitInputOrStart() bool {
	return device.inD0 && !device.exitPending
}

func TestAsyncD0ExitStateModelRejectsReentrancyAndStaleInput(t *testing.T) {
	device := &modeledDevicePower{inD0: true, cacheValid: true}
	if !device.canAdmitInputOrStart() || !device.beginExit() {
		t.Fatal("initial D0 exit was not admitted")
	}
	if device.beginExit() || device.enterD0() || device.canAdmitInputOrStart() {
		t.Fatal("pending D0 exit admitted a duplicate exit, D0 entry, input, or START")
	}
	if device.workEnqueues != 1 || device.exitCompletions != 0 || !device.cacheValid {
		t.Fatalf("DISPATCH phase mutated passive cache state: %+v", *device)
	}
	if !device.completeExitWork() || device.cacheValid || device.exitPending ||
		device.exitCompletions != 1 {
		t.Fatalf("passive completion did not clear cache and close one transition: %+v", *device)
	}
	if device.completeExitWork() || !device.enterD0() || !device.canAdmitInputOrStart() {
		t.Fatal("completion duplicated or D0 entry failed to reopen admission")
	}
	if device.workEnqueues != 1 || device.exitCompletions != 1 {
		t.Fatalf("one D0-exit callback did not map to one async completion: %+v", *device)
	}
}

func TestAsyncD0ExitTeardownFlushesPastPendingFlagBoundary(t *testing.T) {
	type powerTeardown struct {
		inD0            bool
		purging         bool
		shuttingDown    bool
		exitPending     bool
		workQueued      bool
		workRunning     bool
		completionCalls int
		flushCalls      int
		handleConsumed  bool
	}
	beginExit := func(state *powerTeardown) string {
		state.inD0 = false
		if state.shuttingDown || state.purging {
			return "success"
		}
		if state.exitPending {
			return "busy"
		}
		state.exitPending = true
		state.workQueued = true
		return "pending"
	}
	startWorker := func(state *powerTeardown) bool {
		if !state.workQueued || state.workRunning {
			return false
		}
		state.workQueued = false
		state.workRunning = true
		return true
	}
	completePowerTransition := func(state *powerTeardown) bool {
		if !state.workRunning || !state.exitPending {
			return false
		}
		// The real worker clears this immediately before the UdeCx completion.
		// It is still executing and using the device until it returns.
		state.exitPending = false
		state.completionCalls++
		return true
	}
	returnWorker := func(state *powerTeardown) bool {
		if !state.workRunning || state.exitPending {
			return false
		}
		state.workRunning = false
		return true
	}
	flush := func(state *powerTeardown) {
		state.flushCalls++
		if state.workQueued {
			if !startWorker(state) || !completePowerTransition(state) {
				t.Fatal("flush could not run a queued D0-exit worker")
			}
		}
		if state.workRunning && !returnWorker(state) {
			t.Fatal("flush returned before the active D0-exit worker")
		}
	}
	consume := func(state *powerTeardown) bool {
		if state.workQueued || state.workRunning || state.exitPending {
			return false
		}
		state.handleConsumed = true
		return true
	}

	// Power exit wins admission, then removal closes the gate. Teardown must
	// drain the already-queued callback before consuming the UdeCx handle.
	queued := powerTeardown{inD0: true}
	if status := beginExit(&queued); status != "pending" {
		t.Fatalf("D0 exit did not win its BrokerLock boundary: %s", status)
	}
	queued.purging = true
	flush(&queued)
	if !consume(&queued) || queued.completionCalls != 1 || queued.flushCalls != 1 {
		t.Fatalf("teardown consumed before its queued power completion returned: %+v", queued)
	}

	// The worker can clear D0ExitPending just before its completion call. A
	// conditional flag check would miss this still-running callback; an
	// unconditional work-item flush joins it.
	running := powerTeardown{inD0: true}
	if beginExit(&running) != "pending" || !startWorker(&running) ||
		!completePowerTransition(&running) || running.exitPending || !running.workRunning {
		t.Fatalf("model did not reach the cleared-flag/running-worker boundary: %+v", running)
	}
	running.purging = true
	flush(&running)
	if !consume(&running) || running.workRunning || running.completionCalls != 1 {
		t.Fatalf("unconditional flush failed to join the post-flag callback tail: %+v", running)
	}

	// Removal can win first. A later D0-exit callback closes InD0 but completes
	// synchronously and must not enqueue work against the soon-consumed handle.
	teardownFirst := powerTeardown{inD0: true, purging: true}
	if status := beginExit(&teardownFirst); status != "success" ||
		teardownFirst.inD0 || teardownFirst.workQueued || teardownFirst.exitPending {
		t.Fatalf("teardown-owned D0 exit did not finish synchronously: %+v status=%s",
			teardownFirst, status)
	}
	flush(&teardownFirst)
	if !consume(&teardownFirst) || teardownFirst.completionCalls != 0 {
		t.Fatalf("teardown-first path scheduled an unowned async completion: %+v", teardownFirst)
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
		"ViiperReleaseDeviceSlot( controllerContext, device, deviceContext->Slot, deviceContext->PortReservation);",
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
		"ViiperFlushD0ExitWorkItem(device);",
		"ViiperAbortDeviceManagementOperations(Controller, device, STATUS_FILE_CLOSED);",
		"if (plugged)",
		"UdecxUsbDevicePlugOutAndDelete(device)",
		"return FALSE;")
	assertNoConsumedHandleUse(t, destroyOwned, "UdecxUsbDevicePlugOutAndDelete(device)", "} else {")
	shutdown := normalizedContract(nativeCFunction(t, device, "ViiperBeginControllerShutdown"))
	requireContractOrder(t, shutdown,
		"VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(devices[index]);",
		"BOOLEAN plugged = deviceContext->Plugged;",
		"ULONGLONG deviceId = deviceContext->DeviceId;",
		"ULONG generation = deviceContext->Generation;",
		"ViiperFlushD0ExitWorkItem(devices[index]);",
		"if (plugged)",
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
