package usb_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/internal/transport/udecx"
)

type nativeLiveTeardownDeviceKey struct {
	deviceID     uint64
	generation   uint32
	deviceObject uint64
}

type nativeLiveTeardownEndpointKey struct {
	nativeLiveTeardownDeviceKey
	endpointObject  uint64
	endpointAddress uint8
}

type nativeLivePurgeProgress struct {
	key                 nativeLiveTeardownEndpointKey
	beginSequence       uint64
	quiescentSequence   uint64
	drainEndSequence    uint64
	completeEndSequence uint64
}

type nativeLiveEndpointHistory struct {
	beginSequences  []uint64
	cycles          []*nativeLivePurgeProgress
	building        *nativeLivePurgeProgress
	prefixFragments int
}

type nativeLiveTeardownAudit struct {
	complete   bool
	purgeCount int
	diagnostic string
}

func nativeLiveTraceEventName(event uint16) string {
	switch event {
	case udecx.TraceEndpointPurgeBegin:
		return "endpoint-purge-begin"
	case udecx.TraceEndpointDriverQuiescent:
		return "endpoint-driver-quiescent"
	case udecx.TraceEndpointDrainEnd:
		return "endpoint-drain-end"
	case udecx.TraceEndpointPurgeCompleteEnd:
		return "endpoint-purge-complete-end"
	case udecx.TraceEndpointCleanupEnd:
		return "endpoint-cleanup-end"
	case udecx.TraceDeviceCleanupEnd:
		return "device-cleanup-end"
	case udecx.TraceEndpointQuiescenceWatchdog:
		return "endpoint-quiescence-watchdog"
	case udecx.TraceCompletionRundownWatchdog:
		return "completion-rundown-watchdog"
	case udecx.TraceControllerRundownWatchdog:
		return "controller-rundown-watchdog"
	case udecx.TraceOwnerRundownWatchdog:
		return "owner-rundown-watchdog"
	default:
		return fmt.Sprintf("event-%d", event)
	}
}

func nativeLiveTeardownDevice(record udecx.LifecycleTraceRecord) nativeLiveTeardownDeviceKey {
	return nativeLiveTeardownDeviceKey{
		deviceID:     record.DeviceID,
		generation:   record.Generation,
		deviceObject: record.DeviceObject,
	}
}

func nativeLiveTeardownEndpoint(record udecx.LifecycleTraceRecord) nativeLiveTeardownEndpointKey {
	return nativeLiveTeardownEndpointKey{
		nativeLiveTeardownDeviceKey: nativeLiveTeardownDevice(record),
		endpointObject:              record.EndpointObject,
		endpointAddress:             record.EndpointAddress,
	}
}

func nativeLiveEndpointDiagnostic(
	prefix string,
	progress *nativeLivePurgeProgress,
	last udecx.LifecycleTraceRecord,
) string {
	return fmt.Sprintf(
		"%s: device=%#x generation=%d device-object=%#x endpoint=%#02x endpoint-object=%#x purge-sequence=%d last-sequence=%d last-event=%s line=%d queue-state=%#x active=%d",
		prefix,
		progress.key.deviceID,
		progress.key.generation,
		progress.key.deviceObject,
		progress.key.endpointAddress,
		progress.key.endpointObject,
		progress.beginSequence,
		last.PublishedSequence,
		nativeLiveTraceEventName(last.Event),
		last.Line,
		last.QueueState,
		last.ActiveOperations,
	)
}

func auditNativeLiveTeardown(
	trace udecx.LifecycleTrace,
	stats udecx.Stats,
) (nativeLiveTeardownAudit, error) {
	if trace.StatusFlags&udecx.LifecycleTraceStatusWatchdogFired != 0 {
		return nativeLiveTeardownAudit{}, fmt.Errorf(
			"lifecycle watchdog status is sticky even if its record rolled out: flags=%#x latest-sequence=%d",
			trace.StatusFlags, trace.LatestSequence)
	}
	if trace.StatusFlags&udecx.LifecycleTraceStatusDroppedRecord != 0 {
		return nativeLiveTeardownAudit{}, fmt.Errorf(
			"lifecycle recorder dropped a contended record: flags=%#x latest-sequence=%d",
			trace.StatusFlags, trace.LatestSequence)
	}
	if trace.LatestSequence == 0 {
		return nativeLiveTeardownAudit{diagnostic: "no lifecycle records are published yet"}, nil
	}
	wantRecords := trace.LatestSequence
	if wantRecords > udecx.LifecycleTraceCapacity {
		wantRecords = udecx.LifecycleTraceCapacity
	}
	if uint64(len(trace.Records)) != wantRecords {
		return nativeLiveTeardownAudit{diagnostic: fmt.Sprintf(
			"lifecycle suffix snapshot is incomplete: latest-sequence=%d records=%d want=%d",
			trace.LatestSequence, len(trace.Records), wantRecords)}, nil
	}
	firstSequence := trace.LatestSequence - uint64(len(trace.Records)) + 1
	for index, record := range trace.Records {
		expected := firstSequence + uint64(index)
		if record.PublishedSequence != expected {
			return nativeLiveTeardownAudit{diagnostic: fmt.Sprintf(
				"lifecycle snapshot has a sequence gap: index=%d sequence=%d want=%d latest=%d",
				index, record.PublishedSequence, expected, trace.LatestSequence)}, nil
		}
	}
	truncation := ""
	if firstSequence > 1 {
		truncation = fmt.Sprintf(
			"retained lifecycle suffix starts at sequence %d (latest=%d capacity=%d); %d prefix records are unavailable",
			firstSequence, trace.LatestSequence, udecx.LifecycleTraceCapacity, firstSequence-1)
	}
	withTruncation := func(diagnostic string) string {
		if truncation == "" {
			return diagnostic
		}
		return truncation + "; " + diagnostic
	}

	histories := make(map[nativeLiveTeardownEndpointKey]*nativeLiveEndpointHistory)
	lastEndpoint := make(map[nativeLiveTeardownEndpointKey]udecx.LifecycleTraceRecord)
	lastDevice := make(map[nativeLiveTeardownDeviceKey]udecx.LifecycleTraceRecord)
	endpointCleanup := make(map[nativeLiveTeardownEndpointKey]uint64)
	deviceCleanup := make(map[nativeLiveTeardownDeviceKey]uint64)
	historyFor := func(key nativeLiveTeardownEndpointKey) *nativeLiveEndpointHistory {
		history := histories[key]
		if history == nil {
			history = &nativeLiveEndpointHistory{}
			histories[key] = history
		}
		return history
	}

	for _, record := range trace.Records {
		if record.Event >= udecx.TraceEndpointQuiescenceWatchdog &&
			record.Event <= udecx.TraceOwnerRundownWatchdog {
			return nativeLiveTeardownAudit{}, fmt.Errorf(
				"lifecycle watchdog %s fired at sequence %d: device=%#x generation=%d endpoint=%#02x object=%#x line=%d queue-state=%#x active=%d pending=%d",
				nativeLiveTraceEventName(record.Event),
				record.PublishedSequence, record.DeviceID, record.Generation,
				record.EndpointAddress, record.EndpointObject, record.Line,
				record.QueueState, record.ActiveOperations, record.PendingOperations)
		}
		deviceKey := nativeLiveTeardownDevice(record)
		lastDevice[deviceKey] = record
		if record.EndpointObject != 0 {
			lastEndpoint[nativeLiveTeardownEndpoint(record)] = record
		}

		switch record.Event {
		case udecx.TraceEndpointPurgeBegin:
			key := nativeLiveTeardownEndpoint(record)
			historyFor(key).beginSequences = append(
				historyFor(key).beginSequences, record.PublishedSequence)
		case udecx.TraceEndpointDriverQuiescent:
			key := nativeLiveTeardownEndpoint(record)
			history := historyFor(key)
			if history.building != nil {
				return nativeLiveTeardownAudit{}, fmt.Errorf(
					"driver-quiescent sequence %d overlaps an unfinished retained cycle for device=%#x generation=%d endpoint=%#02x object=%#x",
					record.PublishedSequence, key.deviceID, key.generation,
					key.endpointAddress, key.endpointObject)
			}
			if record.Status != 0 || record.ActiveOperations != 0 {
				return nativeLiveTeardownAudit{}, fmt.Errorf(
					"driver-quiescent sequence %d is not terminal: status=%#x active=%d",
					record.PublishedSequence, uint32(record.Status), record.ActiveOperations)
			}
			history.building = &nativeLivePurgeProgress{
				key:               key,
				quiescentSequence: record.PublishedSequence,
			}
		case udecx.TraceEndpointDrainEnd:
			key := nativeLiveTeardownEndpoint(record)
			history := historyFor(key)
			if history.building == nil {
				if firstSequence > 1 && len(history.cycles) == 0 {
					history.prefixFragments++
					continue
				}
				return nativeLiveTeardownAudit{}, fmt.Errorf(
					"drain-end sequence %d has no quiescent purge for device=%#x generation=%d endpoint=%#02x object=%#x",
					record.PublishedSequence, key.deviceID, key.generation,
					key.endpointAddress, key.endpointObject)
			}
			history.building.drainEndSequence = record.PublishedSequence
		case udecx.TraceEndpointPurgeCompleteEnd:
			key := nativeLiveTeardownEndpoint(record)
			history := historyFor(key)
			if history.building == nil || history.building.drainEndSequence == 0 {
				if firstSequence > 1 && len(history.cycles) == 0 {
					history.prefixFragments++
					history.building = nil
					continue
				}
				return nativeLiveTeardownAudit{}, fmt.Errorf(
					"purge-complete sequence %d has no drained purge for device=%#x generation=%d endpoint=%#02x object=%#x",
					record.PublishedSequence, key.deviceID, key.generation,
					key.endpointAddress, key.endpointObject)
			}
			history.building.completeEndSequence = record.PublishedSequence
			history.cycles = append(history.cycles, history.building)
			history.building = nil
		case udecx.TraceEndpointCleanupEnd:
			endpointCleanup[nativeLiveTeardownEndpoint(record)] = record.PublishedSequence
		case udecx.TraceDeviceCleanupEnd:
			deviceCleanup[deviceKey] = record.PublishedSequence
		}
	}

	progresses := make([]*nativeLivePurgeProgress, 0)
	for key, history := range histories {
		if history.building != nil {
			last := lastEndpoint[key]
			phase := "drain-end"
			if history.building.drainEndSequence != 0 {
				phase = "purge-complete-end"
			}
			return nativeLiveTeardownAudit{diagnostic: withTruncation(fmt.Sprintf(
				"retained endpoint cycle has not reached %s: device=%#x generation=%d endpoint=%#02x object=%#x quiescent-sequence=%d last-sequence=%d last-event=%s line=%d",
				phase, key.deviceID, key.generation, key.endpointAddress, key.endpointObject,
				history.building.quiescentSequence, last.PublishedSequence,
				nativeLiveTraceEventName(last.Event), last.Line))}, nil
		}
		beginCount := len(history.beginSequences)
		cycleCount := len(history.cycles)
		pairCount := beginCount
		if cycleCount < pairCount {
			pairCount = cycleCount
		}
		if beginCount > cycleCount {
			return nativeLiveTeardownAudit{purgeCount: len(progresses), diagnostic: withTruncation(fmt.Sprintf(
				"%d retained purge begin(s) for device=%#x generation=%d endpoint=%#02x object=%#x have only %d complete cycles; at least one has not reached driver-quiescent; begin-sequences=%v",
				beginCount, key.deviceID, key.generation, key.endpointAddress,
				key.endpointObject, cycleCount, history.beginSequences))}, nil
		}
		for index := 0; index < pairCount; index++ {
			beginSequence := history.beginSequences[beginCount-pairCount+index]
			progress := history.cycles[cycleCount-pairCount+index]
			if beginSequence >= progress.quiescentSequence {
				return nativeLiveTeardownAudit{purgeCount: len(progresses), diagnostic: withTruncation(fmt.Sprintf(
					"retained purge begin sequence %d has no later complete cycle for device=%#x generation=%d endpoint=%#02x object=%#x",
					beginSequence, key.deviceID, key.generation, key.endpointAddress,
					key.endpointObject))}, nil
			}
			progress.beginSequence = beginSequence
			progresses = append(progresses, progress)
		}
	}

	if len(progresses) == 0 && firstSequence == 1 {
		last := trace.Records[len(trace.Records)-1]
		return nativeLiveTeardownAudit{diagnostic: fmt.Sprintf(
			"no endpoint purge was observed; latest sequence=%d event=%s line=%d",
			last.PublishedSequence, nativeLiveTraceEventName(last.Event), last.Line)}, nil
	}

	for _, progress := range progresses {
		last := lastEndpoint[progress.key]
		if sequence := endpointCleanup[progress.key]; sequence <= progress.completeEndSequence {
			return nativeLiveTeardownAudit{purgeCount: len(progresses), diagnostic: withTruncation(
				nativeLiveEndpointDiagnostic("purged endpoint has not reached endpoint-cleanup-end", progress, last))}, nil
		}
		if sequence := deviceCleanup[progress.key.nativeLiveTeardownDeviceKey]; sequence <= progress.completeEndSequence {
			last = lastDevice[progress.key.nativeLiveTeardownDeviceKey]
			return nativeLiveTeardownAudit{purgeCount: len(progresses), diagnostic: withTruncation(
				nativeLiveEndpointDiagnostic("purged device has not reached device-cleanup-end", progress, last))}, nil
		}
	}

	if stats.ActiveDevices != 0 || stats.PendingOperations != 0 || stats.ReservedPorts != 0 {
		return nativeLiveTeardownAudit{purgeCount: len(progresses), diagnostic: withTruncation(fmt.Sprintf(
			"kernel teardown counters are not clean: ActiveDevices=%d PendingOperations=%d ReservedPorts=%d",
			stats.ActiveDevices, stats.PendingOperations, stats.ReservedPorts))}, nil
	}

	summary := truncation
	if summary != "" {
		prefixFragments := 0
		for _, history := range histories {
			prefixFragments += history.prefixFragments
		}
		summary += fmt.Sprintf(
			"; audited %d retained purge begin(s), ignored %d prefix-only phase fragment(s); zeroed kernel counters prove whole-run cleanup",
			len(progresses), prefixFragments)
	}
	return nativeLiveTeardownAudit{complete: true, purgeCount: len(progresses), diagnostic: summary}, nil
}

func nativeLiveTrace(events ...uint16) udecx.LifecycleTrace {
	const (
		deviceID       = uint64(0x5649495000000001)
		deviceObject   = uint64(0xffff800000001000)
		endpointObject = uint64(0xffff800000002000)
	)
	records := make([]udecx.LifecycleTraceRecord, 0, len(events))
	for index, event := range events {
		record := udecx.LifecycleTraceRecord{
			PublishedSequence: uint64(index + 1),
			DeviceID:          deviceID,
			DeviceObject:      deviceObject,
			Generation:        1,
			Event:             event,
			Line:              uint32(100 + index),
		}
		if (event >= udecx.TraceEndpointPurgeBegin && event <= udecx.TraceEndpointCleanupEnd) ||
			event == udecx.TraceEndpointQuiescenceWatchdog {
			record.EndpointObject = endpointObject
			record.EndpointAddress = 0x81
		}
		if event == udecx.TraceEndpointPurgeBegin || event == udecx.TraceEndpointDriverQuiescent {
			record.QueueState = 0x0f
		}
		records = append(records, record)
	}
	return udecx.LifecycleTrace{LatestSequence: uint64(len(records)), Records: records}
}

func nativeLiveRolledTrace(events ...uint16) udecx.LifecycleTrace {
	latestSequence := uint64(udecx.LifecycleTraceCapacity + 10)
	firstSequence := latestSequence - udecx.LifecycleTraceCapacity + 1
	prefixRecords := udecx.LifecycleTraceCapacity - len(events)
	records := make([]udecx.LifecycleTraceRecord, 0, udecx.LifecycleTraceCapacity)
	for index := 0; index < prefixRecords; index++ {
		records = append(records, udecx.LifecycleTraceRecord{
			PublishedSequence: firstSequence + uint64(index),
			Event:             udecx.TraceControllerShutdownBegin,
		})
	}
	for _, record := range nativeLiveTrace(events...).Records {
		record.PublishedSequence = firstSequence + uint64(len(records))
		records = append(records, record)
	}
	return udecx.LifecycleTrace{LatestSequence: latestSequence, Records: records}
}

func TestNativeLiveTeardownAuditAcceptsReadyQueuePurge(t *testing.T) {
	trace := nativeLiveTrace(
		udecx.TraceEndpointPurgeBegin,
		udecx.TraceEndpointDriverQuiescent,
		udecx.TraceEndpointDrainEnd,
		udecx.TraceEndpointPurgeCompleteEnd,
		udecx.TraceEndpointCleanupEnd,
		udecx.TraceDeviceCleanupEnd,
	)
	audit, err := auditNativeLiveTeardown(trace, udecx.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	if !audit.complete || audit.purgeCount != 1 {
		t.Fatalf("ready 0x0f purge did not pass teardown audit: %+v", audit)
	}
}

func TestNativeLiveTeardownAuditRejectsAnyQuiescenceWatchdog(t *testing.T) {
	trace := nativeLiveTrace(
		udecx.TraceEndpointPurgeBegin,
		udecx.TraceEndpointQuiescenceWatchdog,
	)
	trace.Records[1].Status = -1
	trace.Records[1].ActiveOperations = 1
	trace.Records[1].QueueState = 0x0f
	_, err := auditNativeLiveTeardown(trace, udecx.Stats{})
	if err == nil || !strings.Contains(err.Error(), "endpoint-quiescence-watchdog") ||
		!strings.Contains(err.Error(), "active=1") {
		t.Fatalf("watchdog audit error=%v want explicit active rundown snapshot", err)
	}
}

func TestNativeLiveTeardownAuditRejectsStickyWatchdogAfterRecordRollover(t *testing.T) {
	trace := nativeLiveTrace(udecx.TraceCreateBegin)
	trace.StatusFlags = udecx.LifecycleTraceStatusWatchdogFired
	_, err := auditNativeLiveTeardown(trace, udecx.Stats{})
	if err == nil || !strings.Contains(err.Error(), "watchdog status is sticky") {
		t.Fatalf("sticky watchdog audit error=%v want permanent release failure", err)
	}
}

func TestNativeLiveTeardownAuditRejectsRecorderContentionDrop(t *testing.T) {
	trace := nativeLiveTrace(udecx.TraceCreateBegin)
	trace.StatusFlags = udecx.LifecycleTraceStatusDroppedRecord
	_, err := auditNativeLiveTeardown(trace, udecx.Stats{})
	if err == nil || !strings.Contains(err.Error(), "dropped a contended record") {
		t.Fatalf("recorder drop audit error=%v want fail-closed release result", err)
	}
}

func TestNativeLiveTeardownAuditTracksRepeatedPurgesFIFO(t *testing.T) {
	trace := nativeLiveTrace(
		udecx.TraceEndpointPurgeBegin,
		udecx.TraceEndpointPurgeBegin,
		udecx.TraceEndpointDriverQuiescent,
		udecx.TraceEndpointDrainEnd,
		udecx.TraceEndpointPurgeCompleteEnd,
		udecx.TraceEndpointDriverQuiescent,
		udecx.TraceEndpointDrainEnd,
		udecx.TraceEndpointPurgeCompleteEnd,
		udecx.TraceEndpointCleanupEnd,
		udecx.TraceDeviceCleanupEnd,
	)
	audit, err := auditNativeLiveTeardown(trace, udecx.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	if !audit.complete || audit.purgeCount != 2 {
		t.Fatalf("repeated purges were not correlated one-for-one: %+v", audit)
	}
}

func TestNativeLiveTeardownAuditReportsEveryIncompletePhase(t *testing.T) {
	tests := []struct {
		name       string
		events     []uint16
		diagnostic string
	}{
		{name: "quiescent", events: []uint16{udecx.TraceEndpointPurgeBegin}, diagnostic: "driver-quiescent"},
		{name: "drain", events: []uint16{
			udecx.TraceEndpointPurgeBegin, udecx.TraceEndpointDriverQuiescent,
		}, diagnostic: "drain-end"},
		{name: "complete", events: []uint16{
			udecx.TraceEndpointPurgeBegin, udecx.TraceEndpointDriverQuiescent,
			udecx.TraceEndpointDrainEnd,
		}, diagnostic: "purge-complete-end"},
		{name: "endpoint cleanup", events: []uint16{
			udecx.TraceEndpointPurgeBegin, udecx.TraceEndpointDriverQuiescent,
			udecx.TraceEndpointDrainEnd, udecx.TraceEndpointPurgeCompleteEnd,
		}, diagnostic: "endpoint-cleanup-end"},
		{name: "device cleanup", events: []uint16{
			udecx.TraceEndpointPurgeBegin, udecx.TraceEndpointDriverQuiescent,
			udecx.TraceEndpointDrainEnd, udecx.TraceEndpointPurgeCompleteEnd,
			udecx.TraceEndpointCleanupEnd,
		}, diagnostic: "device-cleanup-end"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			audit, err := auditNativeLiveTeardown(nativeLiveTrace(test.events...), udecx.Stats{})
			if err != nil {
				t.Fatal(err)
			}
			if audit.complete || !strings.Contains(audit.diagnostic, test.diagnostic) {
				t.Fatalf("incomplete phase diagnostic=%q want %q", audit.diagnostic, test.diagnostic)
			}
		})
	}
}

func TestNativeLiveTeardownAuditToleratesRolloverAndFailsReservations(t *testing.T) {
	trace := nativeLiveTrace(
		udecx.TraceEndpointPurgeBegin,
		udecx.TraceEndpointDriverQuiescent,
		udecx.TraceEndpointDrainEnd,
		udecx.TraceEndpointPurgeCompleteEnd,
		udecx.TraceEndpointCleanupEnd,
		udecx.TraceDeviceCleanupEnd,
	)
	rolled := nativeLiveRolledTrace(
		// This completion belongs to a purge whose begin and drain were in the
		// overwritten prefix. The complete retained cycle after it remains
		// independently auditable.
		udecx.TraceEndpointPurgeCompleteEnd,
		udecx.TraceEndpointPurgeBegin,
		udecx.TraceEndpointDriverQuiescent,
		udecx.TraceEndpointDrainEnd,
		udecx.TraceEndpointPurgeCompleteEnd,
		udecx.TraceEndpointCleanupEnd,
		udecx.TraceDeviceCleanupEnd,
	)
	rolledAudit, err := auditNativeLiveTeardown(rolled, udecx.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	if !rolledAudit.complete || rolledAudit.purgeCount != 1 ||
		!strings.Contains(rolledAudit.diagnostic, "retained lifecycle suffix") {
		t.Fatalf("complete retained suffix did not survive rollover: %+v", rolledAudit)
	}

	stalledAudit, err := auditNativeLiveTeardown(
		nativeLiveRolledTrace(udecx.TraceEndpointPurgeBegin), udecx.Stats{})
	if err != nil {
		t.Fatal(err)
	}
	if stalledAudit.complete ||
		!strings.Contains(stalledAudit.diagnostic, "driver-quiescent") ||
		!strings.Contains(stalledAudit.diagnostic, "retained lifecycle suffix") {
		t.Fatalf("retained stalled purge was hidden by rollover: %+v", stalledAudit)
	}

	audit, err := auditNativeLiveTeardown(trace, udecx.Stats{ReservedPorts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if audit.complete || !strings.Contains(audit.diagnostic, "ReservedPorts=1") {
		t.Fatalf("reserved port leak did not block teardown: %+v", audit)
	}
}
