package udecx

import (
	"strings"
	"testing"
)

func TestNativeLifecycleTraceUsesBoundedPerProcessorRecorder(t *testing.T) {
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")
	controller := nativeContractSource(t, "native", "udecx", "driver", "Controller.c")
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	trace := nativeContractSource(t, "native", "udecx", "driver", "Trace.c")
	ioctl := nativeContractSource(t, "native", "udecx", "driver", "Ioctl.c")

	for _, fragment := range []string{
		"#define VIIPER_UDE_LIFECYCLE_TRACE_MAX_SHARDS 64",
		"typedef struct VIIPER_UDE_LIFECYCLE_TRACE_SHARD",
		"DECLSPEC_ALIGN(SYSTEM_CACHE_ALIGNMENT_SIZE) volatile LONG64 WriteSequence;",
		"volatile LONG64 SlotStates[VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY];",
		"VIIPER_UDE_LIFECYCLE_TRACE_RECORD Records[VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY];",
		"WDFMEMORY LifecycleTraceStorage;",
		"VIIPER_UDE_LIFECYCLE_TRACE_SHARD *LifecycleTraceShards;",
		"ULONG LifecycleTraceShardCount;",
	} {
		if !strings.Contains(header, fragment) {
			t.Fatalf("native lifecycle recorder is missing %q", fragment)
		}
	}
	if strings.Contains(header,
		"LifecycleTrace[VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY]") {
		t.Fatal("controller context retained the contended global lifecycle record array")
	}

	initialize := normalizedContract(nativeCFunction(t, trace,
		"ViiperInitializeLifecycleTrace"))
	requireContractOrder(t, initialize,
		"KeQueryMaximumProcessorCountEx(ALL_PROCESSOR_GROUPS)",
		"VIIPER_UDE_LIFECYCLE_TRACE_MAX_SHARDS",
		"WdfMemoryCreate(",
		"NonPagedPoolNx",
		"RtlZeroMemory(rawStorage, storageSize);",
		"controllerContext->LifecycleTraceShards =",
		"controllerContext->LifecycleTraceShardCount = shardCount;")
	if !strings.Contains(controller, "status = ViiperInitializeLifecycleTrace(device);") {
		t.Fatal("controller publishes emulation without constructing the nonpaged recorder")
	}

	hot := normalizedContract(nativeCFunction(t, trace, "ViiperTraceLifecycle"))
	requireContractOrder(t, hot,
		"KeGetCurrentProcessorNumberEx(&processorNumber);",
		"KeGetProcessorIndexFromNumber(&processorNumber);",
		"shardIndex = processorIndex % controllerContext->LifecycleTraceShardCount;",
		"InterlockedIncrement64(&shard->WriteSequence);",
		"slotState = &shard->SlotStates[slotIndex];",
		"claimedSlotState = (LONG64)((localSequence << 1) | 1ULL);",
		"VIIPER_UDE_LIFECYCLE_TRACE_STATUS_DROPPED_RECORD",
		"InterlockedCompareExchange64( slotState, claimedSlotState, observedSlotState)",
		"InterlockedIncrement64( &controllerContext->LifecycleTraceSequence);",
		"record = &shard->Records[",
		"InterlockedExchange64((volatile LONG64 *)&record->PublishedSequence, 0);",
		"KeMemoryBarrier();",
		"InterlockedExchange64( (volatile LONG64 *)&record->PublishedSequence, (LONG64)sequence);",
		"InterlockedExchange64(slotState, (LONG64)(localSequence << 1));")
	for _, forbidden := range []string{
		"WdfMemoryCreate", "WdfSpinLockAcquire", "WdfWaitLockAcquire",
		"ExAcquirePushLock", "KeWaitForSingleObject",
	} {
		if strings.Contains(hot, forbidden) {
			t.Fatalf("lifecycle recorder hot path contains %q", forbidden)
		}
	}

	query := normalizedContract(nativeCFunction(t, ioctl,
		"ViiperHandleQueryLifecycleTrace"))
	requireContractOrder(t, query,
		"latestSequence = (ULONGLONG)ViiperReadCounter( &context->LifecycleTraceSequence);",
		"shardIndex < context->LifecycleTraceShardCount",
		"recordIndex < VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY",
		"slotStateBefore = InterlockedCompareExchange64(",
		"publishedBefore < firstSequence",
		"RtlCopyMemory(&candidate, source, sizeof(candidate));",
		"slotStateAfter = InterlockedCompareExchange64(",
		"slotStateAfter != slotStateBefore",
		"publishedAfter != publishedBefore",
		"output->Records[insertIndex] = candidate;",
		"++output->RecordCount;",
		"output->StatusFlags = (VIIPER_UDE_UINT32)InterlockedCompareExchange(",
		"WdfRequestSetInformation(Request, sizeof(*output));")

	for _, name := range []string{
		"ViiperWaitForEndpointQuiescence",
		"ViiperWaitForEndpointPurgeQuiescence",
	} {
		wait := normalizedContract(nativeCFunction(t, device, name))
		requireContractOrder(t, wait,
			"watchdogWait.QuadPart = -(LONGLONG)VIIPER_UDE_RUNDOWN_WATCHDOG_INTERVAL_100NS;",
			"KeWaitForSingleObject(",
			"&watchdogWait);",
			"if (quiescent)",
			"return;",
			"VIIPER_UDE_TRACE_ENDPOINT_QUIESCENCE_WATCHDOG",
			"STATUS_IO_TIMEOUT",
			"KeDelayExecutionThread(")
		if strings.Contains(wait, "UdecxUsbEndpointPurgeComplete") {
			t.Fatalf("%s abandons rundown and completes UdeCx from its watchdog path", name)
		}
	}
}

func TestLifecycleRecorderSlotClaimRejectsPreemptedStaleWriter(t *testing.T) {
	const capacity = LifecycleTraceCapacity
	type slot struct {
		state    uint64
		sequence uint64
	}
	var slots [capacity]slot
	claim := func(local uint64) bool {
		index := (local - 1) % capacity
		observed := slots[index].state
		if observed&1 != 0 || observed>>1 >= local {
			return false
		}
		slots[index].state = local<<1 | 1
		return true
	}
	publish := func(local, sequence uint64) {
		index := (local - 1) % capacity
		slots[index].sequence = sequence
		slots[index].state = local << 1
	}

	// Writer 1 is preempted after reserving its local sequence but before its
	// atomic slot claim. A complete ring of successors reaches the same slot.
	for local := uint64(2); local <= capacity+1; local++ {
		if !claim(local) {
			t.Fatalf("successor local sequence %d could not claim its slot", local)
		}
		publish(local, local)
	}
	if claim(1) {
		t.Fatal("preempted writer reclaimed a slot already published by a newer wrap")
	}
	if got := slots[0].sequence; got != capacity+1 {
		t.Fatalf("slot 0 sequence=%d want newest sequence %d", got, capacity+1)
	}

	// A writer which already owns the slot cannot be overwritten either. The
	// colliding successor is dropped and made sticky by the production path.
	slots[0] = slot{state: 1<<1 | 1}
	if claim(capacity + 1) {
		t.Fatal("colliding successor overwrote an active slot writer")
	}
	publish(1, 1)
	if !claim(capacity + 1) {
		t.Fatal("settled old slot did not admit its newer wrap")
	}
}

func TestPerProcessorLifecycleRecorderRetainsGlobalPublicWindow(t *testing.T) {
	const (
		shardCount = 7
		capacity   = LifecycleTraceCapacity
		writes     = 10000
	)
	type record struct{ sequence uint64 }
	shards := make([][capacity]record, shardCount)
	local := make([]uint64, shardCount)
	for sequence := uint64(1); sequence <= writes; sequence++ {
		// Exercise uneven load and processor-to-shard collisions rather than a
		// round-robin distribution.
		processor := int((sequence*sequence + sequence*17 + 3) % 97)
		shard := processor % shardCount
		local[shard]++
		shards[shard][(local[shard]-1)%capacity] = record{sequence: sequence}
	}
	first := uint64(writes-capacity) + 1
	seen := make(map[uint64]struct{}, capacity)
	for shard := range shards {
		for _, record := range shards[shard] {
			if record.sequence >= first && record.sequence <= writes {
				seen[record.sequence] = struct{}{}
			}
		}
	}
	if len(seen) != capacity {
		t.Fatalf("retained records=%d want complete global suffix=%d", len(seen), capacity)
	}
	for sequence := first; sequence <= writes; sequence++ {
		if _, ok := seen[sequence]; !ok {
			t.Fatalf("global retained suffix is missing sequence %d", sequence)
		}
	}
}
