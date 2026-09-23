package dualsense

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestDualSenseInputSchedulerPreservesRapidTriggerPeakBeforeRelease(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	neutral := neutralInputState()
	dev.UpdateInputState(&neutral)
	state := neutral
	state.R2 = 1
	dev.UpdateInputState(&state)
	state.R2 = 80
	state.GyroX = 1200
	dev.UpdateInputState(&state)
	state.R2 = 255
	state.GyroX = 2400
	dev.UpdateInputState(&state)
	state.R2 = 0
	state.Buttons &^= ButtonR2
	state.GyroX = 3600
	dev.UpdateInputState(&state)

	var report [InputReportSize]byte
	if got := dev.BuildInputReportInto(report[:]); got != InputReportSize {
		t.Fatalf("peak report length=%d", got)
	}
	if report[6] != 255 || report[9]&byte(ButtonR2>>8) == 0 {
		t.Fatalf("first service did not present coherent R2 peak: % x", report[:11])
	}
	if got := dev.BuildInputReportInto(report[:]); got != InputReportSize {
		t.Fatalf("release report length=%d", got)
	}
	if report[6] != 0 || report[9]&byte(ButtonR2>>8) != 0 {
		t.Fatalf("release did not follow peak: % x", report[:11])
	}
	dev.BuildInputReportInto(report[:])
	if report[6] != 0 || report[9]&byte(ButtonR2>>8) != 0 {
		t.Fatalf("stale pressed state followed release: % x", report[:11])
	}

	snapshot := dev.InputSchedulerState()
	if snapshot.TransitionHighWater != 2 || snapshot.Overflows != 0 {
		t.Fatalf("unexpected queue telemetry: %+v", snapshot)
	}
}

func TestDualSenseInputSchedulerOrdersTruthfulPeakBeforeButtonTransitions(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	neutral := neutralInputState()
	dev.UpdateInputState(&neutral)

	state := neutral
	state.R2 = 1
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	dev.BuildInputReportInto(report[:]) // The initial press is already presented.
	if report[6] != 1 {
		t.Fatalf("initial press=%d", report[6])
	}

	state.R2 = 255 // Continuous peak; no unrelated control changed yet.
	state.GyroY = 111
	dev.UpdateInputState(&state)
	state.R2 = 80
	state.Buttons |= ButtonCross // Ordered transition after the truthful peak.
	state.GyroY = 222
	dev.UpdateInputState(&state)
	state.Buttons &^= ButtonCross
	state.GyroY = 333
	dev.UpdateInputState(&state)
	state.R2 = 0
	state.Buttons &^= ButtonR2
	state.GyroY = 444
	dev.UpdateInputState(&state)

	dev.BuildInputReportInto(report[:])
	if report[6] != 255 || report[8]&byte(ButtonCross) != 0 ||
		int16(binary.LittleEndian.Uint16(report[18:20])) != 111 {
		t.Fatalf("truthful peak snapshot was not ordered first: % x", report[:22])
	}
	dev.BuildInputReportInto(report[:])
	if report[6] != 80 || report[8]&byte(ButtonCross) == 0 {
		t.Fatalf("button down did not follow peak: % x", report[:11])
	}
	dev.BuildInputReportInto(report[:])
	if report[6] != 80 || report[8]&byte(ButtonCross) != 0 {
		t.Fatalf("button release was reordered: % x", report[:11])
	}
	dev.BuildInputReportInto(report[:])
	if report[6] != 0 || report[8]&byte(ButtonCross) != 0 {
		t.Fatalf("trigger release did not remain last: % x", report[:11])
	}
}

func TestDualSenseInputSchedulerEqualClockPreservesIndependentSimultaneousPeaksTruthfully(t *testing.T) {
	tests := []struct {
		name               string
		firstPeak          [2]uint8
		secondPeak         [2]uint8
		firstPresentation  [2]uint8
		secondPresentation [2]uint8
	}{
		{
			name:      "left peak first",
			firstPeak: [2]uint8{240, 90}, secondPeak: [2]uint8{80, 250},
			firstPresentation:  [2]uint8{240, 2},
			secondPresentation: [2]uint8{80, 250},
		},
		{
			name:      "right peak first",
			firstPeak: [2]uint8{90, 250}, secondPeak: [2]uint8{240, 80},
			firstPresentation:  [2]uint8{1, 250},
			secondPresentation: [2]uint8{240, 80},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dev, err := New(nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Force every receive observation onto the same clock reading. Peak
			// chronology must come from the scheduler receive ordinal, not a
			// timestamp tie-break that can reverse R2-first reports.
			receivedAt := time.Unix(123, 456)
			state := neutralInputState()
			dev.input.updateAt(&state, 0, receivedAt)
			state.L2, state.R2 = 1, 2
			dev.input.updateAt(&state, 0, receivedAt)
			state.L2, state.R2 = tc.firstPeak[0], tc.firstPeak[1]
			dev.input.updateAt(&state, 0, receivedAt)
			state.L2, state.R2 = tc.secondPeak[0], tc.secondPeak[1]
			dev.input.updateAt(&state, 0, receivedAt)
			state.L2, state.R2 = 0, 0
			state.Buttons &^= ButtonL2 | ButtonR2
			dev.input.updateAt(&state, 0, receivedAt)

			var report [InputReportSize]byte
			dev.BuildInputReportInto(report[:])
			if report[5] != tc.firstPresentation[0] ||
				report[6] != tc.firstPresentation[1] {
				t.Fatalf("first presentation invented or reordered trigger state: got=(%d,%d) want=(%d,%d)",
					report[5], report[6], tc.firstPresentation[0],
					tc.firstPresentation[1])
			}
			dev.BuildInputReportInto(report[:])
			if report[5] != tc.secondPresentation[0] ||
				report[6] != tc.secondPresentation[1] {
				t.Fatalf("second truthful peak missing: got=(%d,%d) want=(%d,%d)",
					report[5], report[6], tc.secondPresentation[0],
					tc.secondPresentation[1])
			}
			dev.BuildInputReportInto(report[:])
			if report[5] != 0 || report[6] != 0 ||
				report[9]&byte((ButtonL2|ButtonR2)>>8) != 0 {
				t.Fatalf("simultaneous release did not follow peaks: % x",
					report[:11])
			}
			dev.BuildInputReportInto(report[:])
			if report[5] != 0 || report[6] != 0 {
				t.Fatalf("stale simultaneous re-press followed release: % x",
					report[:11])
			}
		})
	}
}

func TestDualSenseInputSchedulerAllowsCooccurringCombinedTriggerPeak(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := neutralInputState()
	dev.UpdateInputState(&state)
	state.L2, state.R2 = 1, 2
	dev.UpdateInputState(&state)
	state.L2, state.R2 = 240, 250
	dev.UpdateInputState(&state)
	state.L2, state.R2 = 0, 0
	state.Buttons &^= ButtonL2 | ButtonR2
	dev.UpdateInputState(&state)

	var report [InputReportSize]byte
	dev.BuildInputReportInto(report[:])
	if report[5] != 240 || report[6] != 250 {
		t.Fatalf("co-occurring combined peak was not preserved: % x", report[:11])
	}
	dev.BuildInputReportInto(report[:])
	if report[5] != 0 || report[6] != 0 {
		t.Fatalf("release did not follow combined peak: % x", report[:11])
	}
}

func TestDualSenseInputSchedulerClaimedInitialKeepsTruthfulPeakSnapshots(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := neutralInputState()
	dev.UpdateInputState(&state)
	state.L2, state.R2 = 1, 2
	dev.UpdateInputState(&state)

	var report [InputReportSize]byte
	_, initialToken := dev.ClaimInputReport(report[:])
	if initialToken == 0 || report[5] != 1 || report[6] != 2 {
		t.Fatalf("initial simultaneous press was not claimed: % x", report[:11])
	}
	state.L2, state.R2 = 240, 90
	dev.UpdateInputState(&state)
	time.Sleep(time.Microsecond)
	state.L2, state.R2 = 80, 250
	dev.UpdateInputState(&state)
	state.L2, state.R2 = 0, 0
	state.Buttons &^= ButtonL2 | ButtonR2
	dev.UpdateInputState(&state)
	dev.CompleteInputReport(initialToken, true)

	dev.BuildInputReportInto(report[:])
	if report[5] != 240 || report[6] != 90 {
		t.Fatalf("claimed press was followed by invented first peak: % x",
			report[:11])
	}
	dev.BuildInputReportInto(report[:])
	if report[5] != 80 || report[6] != 250 {
		t.Fatalf("claimed press lost second truthful peak: % x", report[:11])
	}
	dev.BuildInputReportInto(report[:])
	if report[5] != 0 || report[6] != 0 {
		t.Fatalf("release did not follow claimed peak snapshots: % x", report[:11])
	}
}

func TestDualSenseInputSchedulerRetryInitialCannotMergeIndependentPeaks(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := neutralInputState()
	dev.UpdateInputState(&state)
	state.L2, state.R2 = 1, 2
	dev.UpdateInputState(&state)

	var report, failed [InputReportSize]byte
	_, failedToken := dev.ClaimInputReport(report[:])
	failed = report
	dev.CompleteInputReport(failedToken, false)
	state.L2, state.R2 = 240, 90
	dev.UpdateInputState(&state)
	time.Sleep(time.Microsecond)
	state.L2, state.R2 = 80, 250
	dev.UpdateInputState(&state)
	state.L2, state.R2 = 0, 0
	state.Buttons &^= ButtonL2 | ButtonR2
	dev.UpdateInputState(&state)

	dev.BuildInputReportInto(report[:])
	if report[5] != 1 || report[6] != 2 || report[7] != failed[7] ||
		binary.LittleEndian.Uint32(report[12:16]) !=
			binary.LittleEndian.Uint32(failed[12:16]) {
		t.Fatalf("failed initial transition was not retried as the same logical state:\n got=% x\nwant=% x",
			report[:16], failed[:16])
	}
	dev.BuildInputReportInto(report[:])
	if report[5] != 240 || report[6] != 90 {
		t.Fatalf("retry was not followed by first truthful peak: % x",
			report[:11])
	}
	dev.BuildInputReportInto(report[:])
	if report[5] != 80 || report[6] != 250 {
		t.Fatalf("retry did not preserve second truthful peak: % x", report[:11])
	}
	dev.BuildInputReportInto(report[:])
	if report[5] != 0 || report[6] != 0 {
		t.Fatalf("release did not follow retry peaks: % x", report[:11])
	}
}

func TestDualSenseGetReportSnapshotsWithoutSelectingOrAdvancing(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := neutralInputState()
	dev.UpdateInputState(&state)
	state.Buttons = ButtonCross
	dev.UpdateInputState(&state)

	var streaming [InputReportSize]byte
	dev.BuildInputReportInto(streaming[:])
	if streaming[7] != 1 {
		t.Fatalf("first streaming sequence=%d", streaming[7])
	}
	state.Buttons = ButtonCircle
	dev.UpdateInputState(&state)

	for attempt := 0; attempt < 2; attempt++ {
		control, handled := dev.HandleControl(hidClassIN, hidGetReport,
			uint16(reportTypeInput)<<8|uint16(ReportIDInput), 0,
			InputReportSize, nil)
		if !handled || len(control) != InputReportSize {
			t.Fatalf("GET_REPORT attempt %d: handled=%t length=%d",
				attempt, handled, len(control))
		}
		if control[7] != 1 || control[8]&byte(ButtonCross) == 0 ||
			control[8]&byte(ButtonCircle) != 0 {
			t.Fatalf("GET_REPORT advanced or selected pending input: % x", control[:11])
		}
	}

	dev.BuildInputReportInto(streaming[:])
	if streaming[7] != 2 || streaming[8]&byte(ButtonCircle) == 0 {
		t.Fatalf("streaming owner did not retain sequence/state ownership: % x",
			streaming[:11])
	}
}

func TestDualSenseVersionedInputSnapshotAdvancesOnlyOnPresentation(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var initial, claimed, snapshot [InputReportSize]byte
	n, initialVersion := dev.SnapshotInputReportInto(initial[:])
	if n != InputReportSize || initialVersion == 0 ||
		!dev.InputReportSnapshotCurrent(initialVersion) {
		t.Fatalf("initial snapshot n=%d version=%d current=%t",
			n, initialVersion, dev.InputReportSnapshotCurrent(initialVersion))
	}

	state := neutralInputState()
	dev.UpdateInputState(&state)
	state.Buttons = ButtonCross
	dev.UpdateInputState(&state)
	n, token := dev.ClaimInputReport(claimed[:])
	if n != InputReportSize || token == 0 {
		t.Fatalf("claim n=%d token=%d", n, token)
	}
	dev.CompleteInputReport(token, false)
	if !dev.InputReportSnapshotCurrent(initialVersion) {
		t.Fatal("failed presentation advanced the GET_REPORT version")
	}

	n, token = dev.ClaimInputReport(claimed[:])
	if n != InputReportSize || token == 0 {
		t.Fatalf("retry claim n=%d token=%d", n, token)
	}
	dev.CompleteInputReport(token, true)
	if dev.InputReportSnapshotCurrent(initialVersion) {
		t.Fatal("successful presentation left the old snapshot current")
	}
	n, currentVersion := dev.SnapshotInputReportInto(snapshot[:])
	if n != InputReportSize || currentVersion == initialVersion ||
		!dev.InputReportSnapshotCurrent(currentVersion) {
		t.Fatalf("current snapshot n=%d old=%d current=%d",
			n, initialVersion, currentVersion)
	}
	if snapshot != claimed {
		t.Fatal("versioned snapshot did not copy the successfully presented report")
	}

	allocations := testing.AllocsPerRun(1000, func() {
		_, version := dev.SnapshotInputReportInto(snapshot[:])
		if !dev.InputReportSnapshotCurrent(version) {
			panic("fresh snapshot is stale")
		}
	})
	if allocations != 0 {
		t.Fatalf("versioned snapshot allocated %.2f objects", allocations)
	}
}

func TestDualSenseInputReportCountersCommitOnlyAfterPresentation(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var initial, claimed, retry, snapshot [InputReportSize]byte
	n, initialVersion := dev.SnapshotInputReportInto(initial[:])
	if n != InputReportSize || initialVersion == 0 {
		t.Fatalf("initial snapshot n=%d version=%d", n, initialVersion)
	}
	if initial[7] != 0 || binary.LittleEndian.Uint32(initial[12:16]) != 0 ||
		initial[11] != 0 || initial[41] != 0 ||
		initial[54] != dualSenseUSBConnectStateWired {
		t.Fatalf("invalid initial report fidelity fields: % x", initial[:55])
	}

	state := neutralInputState()
	dev.UpdateInputState(&state)
	state.L2 = 160
	state.Buttons |= ButtonL2
	dev.UpdateInputState(&state)

	n, token := dev.ClaimInputReport(claimed[:])
	if n != InputReportSize || token == 0 || claimed[5] != state.L2 ||
		claimed[7] != 1 || binary.LittleEndian.Uint32(claimed[12:16]) != 1 ||
		claimed[11] != 0 || claimed[41] != 0 ||
		claimed[54] != dualSenseUSBConnectStateWired {
		t.Fatalf("invalid first claim: n=%d token=%d report=% x",
			n, token, claimed[:55])
	}
	dev.CompleteInputReport(token, false)

	n, version := dev.SnapshotInputReportInto(snapshot[:])
	if n != InputReportSize || version != initialVersion || snapshot != initial {
		t.Fatalf("failed claim changed committed GET_REPORT state: n=%d version=%d initial=%d",
			n, version, initialVersion)
	}

	n, token = dev.ClaimInputReport(retry[:])
	if n != InputReportSize || token == 0 || retry[5] != state.L2 ||
		retry[7] != 1 || binary.LittleEndian.Uint32(retry[12:16]) != 1 {
		t.Fatalf("failed claim did not retry the same counters: n=%d token=%d report=% x",
			n, token, retry[:16])
	}
	dev.CompleteInputReport(token, true)

	n, version = dev.SnapshotInputReportInto(snapshot[:])
	if n != InputReportSize || version == initialVersion || snapshot != retry {
		t.Fatalf("successful retry was not the committed snapshot: n=%d version=%d initial=%d",
			n, version, initialVersion)
	}

	n, token = dev.ClaimInputReport(claimed[:])
	if n != InputReportSize || token == 0 || claimed[7] != 2 ||
		binary.LittleEndian.Uint32(claimed[12:16]) != 2 {
		t.Fatalf("next presentation did not advance both counters once: n=%d token=%d report=% x",
			n, token, claimed[:16])
	}
	dev.CompleteInputReport(token, true)
}

func TestDualSenseInputReportCountersWrapTogether(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dev.input.mu.Lock()
	dev.input.sequence = ^uint8(0)
	dev.input.packetSequence = ^uint32(0)
	dev.input.mu.Unlock()

	var report, snapshot [InputReportSize]byte
	n, token := dev.ClaimInputReport(report[:])
	if n != InputReportSize || token == 0 || report[7] != 0 ||
		binary.LittleEndian.Uint32(report[12:16]) != 0 {
		t.Fatalf("wrapped claim n=%d token=%d report=% x", n, token,
			report[:16])
	}
	dev.CompleteInputReport(token, true)
	dev.SnapshotInputReportInto(snapshot[:])
	if snapshot != report {
		t.Fatal("wrapped counters were not committed with the presented report")
	}
}

func TestDualSenseInputReportVariantStatusLayout(t *testing.T) {
	const elapsed = 2 * time.Millisecond
	timestamp := uint32(elapsed.Microseconds() * 3)

	base := newDualSenseInputScheduler(BatteryFullyCharged, false)
	edge := newDualSenseInputScheduler(BatteryFullyCharged, true)
	var baseReport, edgeReport [InputReportSize]byte
	if n, token := base.beginClaim(base.timestampBase.Add(elapsed),
		BatteryFullyCharged, baseReport[:]); n != InputReportSize || token == 0 {
		t.Fatalf("base claim n=%d token=%d", n, token)
	}
	if n, token := edge.beginClaim(edge.timestampBase.Add(elapsed),
		BatteryFullyCharged, edgeReport[:]); n != InputReportSize || token == 0 {
		t.Fatalf("Edge claim n=%d token=%d", n, token)
	}

	if got := binary.LittleEndian.Uint32(baseReport[28:32]); got != timestamp {
		t.Fatalf("base sensor timestamp=%d, want %d", got, timestamp)
	}
	if got := binary.LittleEndian.Uint32(baseReport[49:53]); got != timestamp {
		t.Fatalf("base Timer2=%d, want %d", got, timestamp)
	}
	if got := binary.LittleEndian.Uint32(edgeReport[28:32]); got != timestamp {
		t.Fatalf("Edge sensor timestamp=%d, want %d", got, timestamp)
	}
	assertDualSenseEdgeUSBStatus(t, edgeReport[:])
}

func TestDualSenseInputTimestampWrapsModuloUint32(t *testing.T) {
	lastBeforeWrapMicroseconds := uint64(^uint32(0)) / 3
	cases := []struct {
		name                string
		elapsedMicroseconds int64
		want                uint32
	}{
		{name: "negative", elapsedMicroseconds: -1, want: 0},
		{
			name:                "last-before-wrap",
			elapsedMicroseconds: int64(lastBeforeWrapMicroseconds),
			want:                ^uint32(0),
		},
		{
			name:                "first-after-wrap",
			elapsedMicroseconds: int64(lastBeforeWrapMicroseconds + 1),
			want:                2,
		},
		{
			name:                "post-wrap",
			elapsedMicroseconds: int64(lastBeforeWrapMicroseconds + 2),
			want:                5,
		},
	}

	for _, edge := range []bool{false, true} {
		variant := "base"
		if edge {
			variant = "Edge"
		}
		t.Run(variant, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					scheduler := newDualSenseInputScheduler(
						BatteryFullyCharged, edge)
					now := scheduler.timestampBase.Add(
						time.Duration(tc.elapsedMicroseconds) * time.Microsecond)
					var report [InputReportSize]byte
					n, token := scheduler.beginClaim(now,
						BatteryFullyCharged, report[:])
					if n != InputReportSize || token == 0 {
						t.Fatalf("claim n=%d token=%d", n, token)
					}
					if got := binary.LittleEndian.Uint32(report[28:32]); got != tc.want {
						t.Fatalf("sensor timestamp=%#08x, want %#08x", got, tc.want)
					}
					if edge {
						assertDualSenseEdgeUSBStatus(t, report[:])
					} else if got := binary.LittleEndian.Uint32(report[49:53]); got != tc.want {
						t.Fatalf("base device timestamp=%#08x, want %#08x",
							got, tc.want)
					}
				})
			}
		})
	}
}

func TestDualSenseEdgeInputReportClaimAndGetReportSnapshot(t *testing.T) {
	dev, err := NewEdge(nil)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}

	var initial, claimed, snapshot [InputReportSize]byte
	n, initialVersion := dev.SnapshotInputReportInto(initial[:])
	if n != InputReportSize || initialVersion == 0 ||
		initial[7] != 0 || binary.LittleEndian.Uint32(initial[12:16]) != 0 {
		t.Fatalf("invalid initial Edge snapshot: n=%d version=%d report=% x",
			n, initialVersion, initial[:55])
	}
	assertDualSenseEdgeUSBStatus(t, initial[:])

	state := neutralInputState()
	dev.UpdateInputState(&state)
	state.L2 = 173
	state.Buttons |= ButtonL2
	dev.UpdateInputState(&state)
	n, token := dev.ClaimInputReport(claimed[:])
	if n != InputReportSize || token == 0 || claimed[5] != state.L2 ||
		claimed[9]&byte(ButtonL2>>8) == 0 || claimed[7] != 1 ||
		binary.LittleEndian.Uint32(claimed[12:16]) != 1 {
		t.Fatalf("invalid Edge claim: n=%d token=%d report=% x",
			n, token, claimed[:55])
	}
	assertDualSenseEdgeUSBStatus(t, claimed[:])
	dev.CompleteInputReport(token, true)

	n, version := dev.SnapshotInputReportInto(snapshot[:])
	if n != InputReportSize || version == initialVersion || snapshot != claimed {
		t.Fatalf("Edge GET_REPORT did not expose the committed claim: n=%d version=%d initial=%d",
			n, version, initialVersion)
	}
}

func TestDualSenseEdgeInvalidInputUsesNeutralStatusLayout(t *testing.T) {
	dev, err := NewEdge(nil)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	state := neutralInputState()
	state.L2 = 211
	state.Buttons = 1 << 31
	dev.UpdateInputState(&state)

	var report, snapshot [InputReportSize]byte
	n, token := dev.ClaimInputReport(report[:])
	if n != InputReportSize || token == 0 || report[1] != 128 ||
		report[2] != 128 || report[3] != 128 || report[4] != 128 ||
		report[5] != 0 || report[6] != 0 || report[8] != DPadUSBNeutral ||
		report[9] != 0 || report[10] != 0 || report[7] != 1 ||
		binary.LittleEndian.Uint32(report[12:16]) != 1 {
		t.Fatalf("invalid Edge controls were not neutralized: n=%d token=%d report=% x",
			n, token, report[:55])
	}
	assertDualSenseEdgeUSBStatus(t, report[:])
	dev.CompleteInputReport(token, true)
	dev.SnapshotInputReportInto(snapshot[:])
	if snapshot != report {
		t.Fatal("neutralized Edge report was not committed for GET_REPORT")
	}
	dev.input.mu.Lock()
	corruptReports := dev.input.corruptReports
	dev.input.mu.Unlock()
	if corruptReports != 1 {
		t.Fatalf("Edge corrupt report count=%d, want 1", corruptReports)
	}
}

func assertDualSenseEdgeUSBStatus(t *testing.T, report []byte) {
	t.Helper()
	if len(report) < 55 {
		t.Fatalf("short Edge input report: %d", len(report))
	}
	if report[11] != 0 || report[41] != 0 ||
		report[49] != dualSenseEdgeActiveProfile || report[50] != 0 ||
		report[51] != 0 || report[52] != 0 ||
		report[53] != BatteryFullyCharged ||
		report[54] != dualSenseUSBConnectStateWired {
		t.Fatalf("invalid Edge raw status fields: % x", report[11:55])
	}
}

func TestDualSenseCancelledClaimRetriesTransitionBeforeRelease(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := neutralInputState()
	dev.UpdateInputState(&state)
	state.R2 = 1
	dev.UpdateInputState(&state)

	var claimed [InputReportSize]byte
	n, token := dev.ClaimInputReport(claimed[:])
	if n != InputReportSize || token == 0 || claimed[6] == 0 {
		t.Fatalf("failed to claim trigger down: n=%d token=%d report=% x",
			n, token, claimed[:11])
	}
	state.R2 = 0
	state.Buttons &^= ButtonR2
	dev.UpdateInputState(&state)

	control, handled := dev.HandleControl(hidClassIN, hidGetReport,
		uint16(reportTypeInput)<<8|uint16(ReportIDInput), 0,
		InputReportSize, nil)
	if !handled || control[6] != 0 || control[7] != 0 ||
		control[9]&byte(ButtonR2>>8) != 0 {
		t.Fatalf("GET_REPORT exposed an unpresented claim: % x", control[:11])
	}

	dev.CompleteInputReport(token, false)
	n, retryToken := dev.ClaimInputReport(claimed[:])
	if n != InputReportSize || retryToken == 0 || claimed[6] == 0 ||
		claimed[9]&byte(ButtonR2>>8) == 0 || claimed[7] != 1 {
		t.Fatalf("failed down was not retried first: n=%d token=%d report=% x",
			n, retryToken, claimed[:11])
	}
	dev.CompleteInputReport(retryToken, true)

	n, releaseToken := dev.ClaimInputReport(claimed[:])
	if n != InputReportSize || releaseToken == 0 || claimed[6] != 0 ||
		claimed[9]&byte(ButtonR2>>8) != 0 || claimed[7] != 2 {
		t.Fatalf("release did not follow successful retry: n=%d token=%d report=% x",
			n, releaseToken, claimed[:11])
	}
	dev.CompleteInputReport(releaseToken, true)
}

func TestDualSenseCancelledContinuousPeakClaimRetriesBeforeRelease(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := neutralInputState()
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	dev.BuildInputReportInto(report[:])

	state.R2 = 1
	state.Buttons |= ButtonR2
	dev.UpdateInputState(&state)
	dev.BuildInputReportInto(report[:])
	if report[6] != 1 {
		t.Fatalf("initial press was not presented: % x", report[:11])
	}

	state.R2 = 255
	state.LX = 77
	dev.UpdateInputState(&state)
	n, peakToken := dev.ClaimInputReport(report[:])
	if n != InputReportSize || peakToken == 0 || report[6] != 255 {
		t.Fatalf("failed to claim continuous peak: n=%d token=%d report=% x",
			n, peakToken, report[:11])
	}

	state.R2 = 0
	state.Buttons &^= ButtonR2
	dev.UpdateInputState(&state)
	dev.CompleteInputReport(peakToken, false)

	n, retryToken := dev.ClaimInputReport(report[:])
	if n != InputReportSize || retryToken == 0 || report[6] != 255 ||
		report[9]&byte(ButtonR2>>8) == 0 {
		t.Fatalf("failed continuous peak was not recovered first: n=%d token=%d report=% x",
			n, retryToken, report[:11])
	}
	dev.CompleteInputReport(retryToken, true)

	n, releaseToken := dev.ClaimInputReport(report[:])
	if n != InputReportSize || releaseToken == 0 || report[6] != 0 ||
		report[9]&byte(ButtonR2>>8) != 0 {
		t.Fatalf("release did not follow recovered peak: n=%d token=%d report=% x",
			n, releaseToken, report[:11])
	}
	dev.CompleteInputReport(releaseToken, true)

	// Idle HID service repeats the committed release, never the failed peak.
	n, idleToken := dev.ClaimInputReport(report[:])
	if n != InputReportSize || idleToken == 0 || report[6] != 0 ||
		report[9]&byte(ButtonR2>>8) != 0 {
		t.Fatalf("stale peak replayed after release: n=%d token=%d report=% x",
			n, idleToken, report[:11])
	}
	dev.CompleteInputReport(idleToken, true)
}

func TestDualSenseFinalTriggerAnalogDigitalCoherence(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := neutralInputState()
	dev.UpdateInputState(&state)
	state.Buttons = ButtonL2 | ButtonR2
	// Explicit mapped digital presses with zero analog are normalized to the
	// smallest non-zero actuation before both classification and encoding.
	dev.UpdateInputState(&state)

	var report [InputReportSize]byte
	dev.BuildInputReportInto(report[:])
	if report[5] != 1 || report[6] != 1 ||
		report[9]&byte((ButtonL2|ButtonR2)>>8) != byte((ButtonL2|ButtonR2)>>8) {
		t.Fatalf("analog/digital trigger wire state disagreed: % x", report[:11])
	}
}

func TestDualSenseInputTelemetryExposesFixedBucketDistributions(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dev.SetInputTelemetryEnabled(true)
	dev.inputTransportTelemetry.frameReadToDecode.record(40 * time.Microsecond)
	dev.inputTransportTelemetry.frameReadToDecode.record(2 * time.Millisecond)
	dev.inputTransportTelemetry.decodeToPublish.record(15 * time.Microsecond)

	state := neutralInputState()
	state.Buttons = ButtonCross
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	dev.BuildInputReportInto(report[:])
	dev.BuildInputReportInto(report[:]) // Repeated HID idle is not a receive sample.

	snapshot := dev.InputTelemetryState()
	if !snapshot.Enabled || snapshot.FrameReadToDecode.Count != 2 ||
		snapshot.FrameReadToDecode.P50 != 50*time.Microsecond ||
		snapshot.FrameReadToDecode.P99 != 2*time.Millisecond ||
		snapshot.DecodeToPublication.Count != 1 ||
		snapshot.ReceiveToSelected.Count != 1 ||
		snapshot.ReceiveToPresented.Count != 1 {
		t.Fatalf("unexpected telemetry snapshot: %+v", snapshot)
	}
}

func TestDualSenseReceiveToSelectedSamplesOnceAcrossRetry(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	receivedAt := time.Unix(123, 456)
	selectedAt := receivedAt.Add(750 * time.Microsecond)
	state := neutralInputState()
	state.R2 = 220
	state.Buttons |= ButtonR2
	if !dev.input.updateAt(&state, 0, receivedAt) {
		t.Fatal("input update was rejected")
	}

	var report [InputReportSize]byte
	dev.input.mu.Lock()
	n, token := dev.input.beginClaim(selectedAt, BatteryFullyCharged, report[:])
	dev.input.mu.Unlock()
	if n != InputReportSize || token == 0 || report[6] != state.R2 {
		t.Fatalf("first selection failed: n=%d token=%d report=% x",
			n, token, report[:11])
	}
	dev.input.mu.Lock()
	dev.input.completeClaimAt(token, false, selectedAt.Add(time.Millisecond))
	dev.input.mu.Unlock()

	// The failed ordered state is selected again much later. Its sampled bit is
	// part of the retry entry, so only the original endpoint opportunity counts.
	retryAt := receivedAt.Add(3 * time.Millisecond)
	dev.input.mu.Lock()
	n, retryToken := dev.input.beginClaim(
		retryAt, BatteryFullyCharged, report[:],
	)
	dev.input.mu.Unlock()
	if n != InputReportSize || retryToken == 0 || report[6] != state.R2 {
		t.Fatalf("retry selection failed: n=%d token=%d report=% x",
			n, retryToken, report[:11])
	}
	presentedAt := receivedAt.Add(4 * time.Millisecond)
	dev.input.mu.Lock()
	dev.input.completeClaimAt(retryToken, true, presentedAt)
	dev.input.mu.Unlock()

	snapshot := dev.InputTelemetryState()
	if snapshot.ReceiveToSelected.Count != 1 ||
		snapshot.ReceiveToSelected.Maximum != 750*time.Microsecond ||
		snapshot.ReceiveToSelected.P99 != time.Millisecond {
		t.Fatalf("selection distribution double-counted retry: %+v",
			snapshot.ReceiveToSelected)
	}
	if snapshot.ReceiveToPresented.Count != 1 ||
		snapshot.ReceiveToPresented.Maximum != 4*time.Millisecond ||
		snapshot.ReceiveToPresented.P99 != 4*time.Millisecond {
		t.Fatalf("presentation distribution was not independent: %+v",
			snapshot.ReceiveToPresented)
	}

	args := dev.GetDeviceSpecificArgs()
	distributions, ok := args["inputLatencyDistributions"].(InputTelemetrySnapshot)
	if !ok || distributions.ReceiveToSelected.Count != 1 {
		t.Fatalf("DeviceSpecificArgs omitted selection distribution: %#v",
			args["inputLatencyDistributions"])
	}
	if got, ok := args["inputMaximumSelectionAgeUS"].(int64); !ok || got != 750 {
		t.Fatalf("DeviceSpecificArgs selection maximum = %#v",
			args["inputMaximumSelectionAgeUS"])
	}
}

func TestDualSenseInputGenerationRejectsStalePublisher(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	old := dev.input.currentGeneration()
	current := dev.beginInputStreamGeneration()
	if current == old || current == 0 {
		t.Fatalf("generation did not advance: old=%d current=%d", old, current)
	}
	state := neutralInputState()
	state.Buttons = ButtonTriangle
	if dev.updateInputStateForGeneration(old, &state) {
		t.Fatal("stale stream generation published input")
	}
	var report [InputReportSize]byte
	dev.BuildInputReportInto(report[:])
	if report[8]&byte(ButtonTriangle) != 0 {
		t.Fatalf("stale input reached HID report: % x", report[:11])
	}
}

func TestDualSenseInputReconnectPreservesAcceptedTransitions(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	generationA := dev.input.currentGeneration()
	state := neutralInputState()
	if !dev.updateInputStateForGeneration(generationA, &state) {
		t.Fatal("generation A baseline was rejected")
	}
	state.R2 = 200
	state.Buttons |= ButtonR2
	if !dev.updateInputStateForGeneration(generationA, &state) {
		t.Fatal("generation A press was rejected")
	}

	generationB := dev.beginInputStreamGeneration()
	if generationB == generationA || generationB == 0 {
		t.Fatalf("receive generation did not advance: A=%d B=%d",
			generationA, generationB)
	}
	stale := state
	stale.Buttons |= ButtonTriangle
	if dev.updateInputStateForGeneration(generationA, &stale) {
		t.Fatal("displaced generation A reader published after reconnect")
	}
	state.R2 = 0
	state.Buttons &^= ButtonR2
	if !dev.updateInputStateForGeneration(generationB, &state) {
		t.Fatal("generation B release was rejected")
	}

	var report [InputReportSize]byte
	n, pressToken := dev.ClaimInputReport(report[:])
	if n != InputReportSize || pressToken == 0 || report[6] != 200 ||
		report[9]&byte(ButtonR2>>8) == 0 ||
		report[8]&byte(ButtonTriangle) != 0 ||
		binary.LittleEndian.Uint32(report[12:16]) != 1 {
		t.Fatalf("accepted generation A press was not selected first: n=%d token=%d report=% x",
			n, pressToken, report[:11])
	}
	dev.CompleteInputReport(pressToken, true)
	n, releaseToken := dev.ClaimInputReport(report[:])
	if n != InputReportSize || releaseToken == 0 || report[6] != 0 ||
		report[9]&byte(ButtonR2>>8) != 0 ||
		binary.LittleEndian.Uint32(report[12:16]) != 2 {
		t.Fatalf("generation B release did not follow press: n=%d token=%d report=% x",
			n, releaseToken, report[:11])
	}
	dev.CompleteInputReport(releaseToken, true)
}

func TestDualSenseInputReconnectPreservesInflightClaimRecovery(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	generationA := dev.input.currentGeneration()
	state := neutralInputState()
	dev.updateInputStateForGeneration(generationA, &state)
	state.R2 = 180
	state.Buttons |= ButtonR2
	dev.updateInputStateForGeneration(generationA, &state)

	var report [InputReportSize]byte
	_, claimToken := dev.ClaimInputReport(report[:])
	if claimToken == 0 || report[6] != 180 {
		t.Fatalf("generation A press was not claimed: token=%d report=% x",
			claimToken, report[:11])
	}
	generationB := dev.beginInputStreamGeneration()
	state.R2 = 0
	state.Buttons &^= ButtonR2
	dev.updateInputStateForGeneration(generationB, &state)
	dev.CompleteInputReport(claimToken, false)

	_, retryToken := dev.ClaimInputReport(report[:])
	if retryToken == 0 || report[6] != 180 ||
		report[9]&byte(ButtonR2>>8) == 0 {
		t.Fatalf("reconnect erased failed in-flight press: token=%d report=% x",
			retryToken, report[:11])
	}
	dev.CompleteInputReport(retryToken, true)
	_, releaseToken := dev.ClaimInputReport(report[:])
	if releaseToken == 0 || report[6] != 0 ||
		report[9]&byte(ButtonR2>>8) != 0 {
		t.Fatalf("release did not follow recovered claim: token=%d report=% x",
			releaseToken, report[:11])
	}
	dev.CompleteInputReport(releaseToken, true)
}

func inputStateWithPhysicalMetadata(sensor uint32,
	metadata [InputStatePhysicalMetadataSize]byte) InputState {
	state := neutralInputState()
	state.PhysicalMetadataValid = true
	state.PhysicalSensorTimestamp = sensor
	state.PhysicalInputMetadata = metadata
	return state
}

func TestDualSenseInputEncoderPreservesPhysicalResonanceMetadata(t *testing.T) {
	physical := [InputStatePhysicalMetadataSize]byte{
		0x6A,                   // report byte 41: touch timestamp
		0x09,                   // report byte 42: R2 arm/status
		0x29,                   // report byte 43: L2 fired, arm position nine
		0x00, 0x00, 0x00, 0x00, // report bytes 44:48: host timestamp
		0x20,                   // report byte 48: L2 weapon effect
		0x31, 0x32, 0x33, 0x34, // report bytes 49:53
		0x27, // report byte 53: physical battery
		0x01, // report byte 54: physical Bluetooth state
		0xA5, // report byte 55: external-mic / haptics-filter status
	}
	const sensor = uint32(0x78563412)

	for _, edge := range []bool{false, true} {
		name := "base"
		var dev *DualSense
		var err error
		if edge {
			name = "Edge"
			dev, err = NewEdge(nil)
		} else {
			dev, err = New(nil)
		}
		if err != nil {
			t.Fatalf("%s New: %v", name, err)
		}

		state := inputStateWithPhysicalMetadata(sensor, physical)
		state.PhysicalMetadataEdgeLayout = edge
		state.L2 = 255
		state.Buttons = ButtonL2
		dev.UpdateInputState(&state)
		var report [InputReportSize]byte
		n, token := dev.ClaimInputReport(report[:])
		if n != InputReportSize || token == 0 {
			t.Fatalf("%s claim n=%d token=%d", name, n, token)
		}
		if report[5] != 255 || report[42] != 0x09 || report[43] != 0x29 ||
			report[48] != 0x20 {
			t.Fatalf("%s lost Resonance trigger vector: % x", name,
				report[5:49])
		}
		if got := binary.LittleEndian.Uint32(report[28:32]); got != sensor {
			t.Fatalf("%s sensor timestamp=%#x, want %#x", name, got, sensor)
		}
		for index := 0; index < physicalMetadataConnectState; index++ {
			if report[41+index] != physical[index] {
				t.Fatalf("%s physical report byte %d=%#x, want %#x", name,
					41+index, report[41+index], physical[index])
			}
		}
		if report[53] != physical[physicalMetadataBattery] {
			t.Fatalf("%s battery=%#x, want raw %#x", name, report[53],
				physical[physicalMetadataBattery])
		}
		if report[54] != physical[physicalMetadataConnectState]&0x07|dualSenseUSBConnectStateWired {
			t.Fatalf("%s exposed physical transport state %#x", name, report[54])
		}
		if report[55] != physical[physicalMetadataHeadsetStatus] {
			t.Fatalf("%s headset/filter status=%#x, want raw %#x", name,
				report[55], physical[physicalMetadataHeadsetStatus])
		}
		for index := 56; index < len(report); index++ {
			if report[index] != 0 {
				t.Fatalf("%s copied unauthenticatable physical tail at byte %d: %#x",
					name, index, report[index])
			}
		}
		if report[7] != 1 || binary.LittleEndian.Uint32(report[12:16]) != 1 {
			t.Fatalf("%s physical metadata replaced virtual counters: % x", name,
				report[7:16])
		}
		dev.CompleteInputReport(token, true)
	}
}

func TestDualSensePhysicalMetadataCrossLayoutSynthesizesTargetStatus(t *testing.T) {
	physical := [InputStatePhysicalMetadataSize]byte{
		0x6A, 0x09, 0x29, 1, 2, 3, 4, 0x20,
		0xDE, 0xAD, 0xBE, 0xEF, 0x27, 0x01,
	}
	const (
		physicalSensor = uint32(0x76543210)
		virtualTime    = uint32(0x44332211)
	)
	for _, test := range []struct {
		name       string
		sourceEdge bool
		targetEdge bool
		wantStatus [4]byte
	}{
		{
			name:       "Edge-source-to-base-target",
			sourceEdge: true,
			wantStatus: [4]byte{0x11, 0x22, 0x33, 0x44},
		},
		{
			name:       "base-source-to-Edge-target",
			targetEdge: true,
			wantStatus: [4]byte{dualSenseEdgeActiveProfile, 0, 0, 0},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := inputStateWithPhysicalMetadata(physicalSensor, physical)
			state.PhysicalMetadataEdgeLayout = test.sourceEdge
			var report [InputReportSize]byte
			if !encodeUSBInputReportInto(&state, BatteryFullyCharged, 7, 9,
				virtualTime, test.targetEdge, report[:]) {
				t.Fatal("encodeUSBInputReportInto rejected valid state")
			}
			if got := binary.LittleEndian.Uint32(report[28:32]); got != physicalSensor {
				t.Fatalf("sensor timestamp=%#x, want %#x", got, physicalSensor)
			}
			for index := 0; index < 8; index++ {
				if report[41+index] != physical[index] {
					t.Fatalf("shared metadata byte %d=%#x, want %#x",
						41+index, report[41+index], physical[index])
				}
			}
			if got := [4]byte(report[49:53]); got != test.wantStatus {
				t.Fatalf("target status=% x, want % x", got, test.wantStatus)
			}
			if report[53] != physical[physicalMetadataBattery] ||
				report[54] != physical[physicalMetadataConnectState]&0x07|dualSenseUSBConnectStateWired {
				t.Fatalf("battery/connect=% x", report[53:55])
			}
		})
	}
}

func TestDualSenseInputEncoderUsesTruthfulNeutralStatusFallback(t *testing.T) {
	for _, edge := range []bool{false, true} {
		name := "base"
		var dev *DualSense
		var err error
		if edge {
			name = "Edge"
			dev, err = NewEdge(nil)
		} else {
			dev, err = New(nil)
		}
		if err != nil {
			t.Fatalf("%s New: %v", name, err)
		}
		state := neutralInputState()
		dev.UpdateInputState(&state)
		var report [InputReportSize]byte
		n, token := dev.ClaimInputReport(report[:])
		if n != InputReportSize || token == 0 {
			t.Fatalf("%s claim n=%d token=%d", name, n, token)
		}
		if report[42] != dualSenseTriggerStatusNeutral ||
			report[43] != dualSenseTriggerStatusNeutral || report[48] != 0 ||
			report[53] != BatteryFullyCharged ||
			report[54] != dualSenseUSBConnectStateWired {
			t.Fatalf("%s invalid fallback metadata: % x", name, report[41:55])
		}
		if edge {
			if report[49] != dualSenseEdgeActiveProfile ||
				report[50] != 0 || report[51] != 0 || report[52] != 0 {
				t.Fatalf("Edge fallback lost generated profile status: % x",
					report[49:53])
			}
		}
		dev.CompleteInputReport(token, true)
	}
}

func TestDualSensePendingPeakCouplesLatestPhysicalTriggerStatus(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	metadata := [InputStatePhysicalMetadataSize]byte{
		0x10, 0xA0, 0x02, 1, 2, 3, 4, 0x23,
		0x41, 0x42, 0x43, 0x44, 0x25, 0x01,
	}
	state := inputStateWithPhysicalMetadata(0x11111111, metadata)
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	_, baselineToken := dev.ClaimInputReport(report[:])
	if baselineToken == 0 {
		t.Fatal("baseline was not claimed")
	}
	dev.CompleteInputReport(baselineToken, true)

	state.L2 = 1
	state.PhysicalSensorTimestamp = 0x22222222
	state.PhysicalInputMetadata[physicalMetadataR2Status] = 0xA1
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x02
	dev.UpdateInputState(&state)
	for index, step := range []struct {
		analog uint8
		status uint8
	}{
		{analog: 80, status: 0x12},
		{analog: 100, status: 0x13},
		{analog: 140, status: 0x14},
		{analog: 180, status: 0x15},
		{analog: 206, status: 0x26},
		{analog: 228, status: 0x27},
	} {
		state.L2 = step.analog
		state.PhysicalSensorTimestamp += 0x101
		state.PhysicalInputMetadata[physicalMetadataR2Status] =
			0xA2 + uint8(index)
		state.PhysicalInputMetadata[physicalMetadataL2Status] = step.status
		dev.UpdateInputState(&state)
	}
	state.L2 = 255
	state.PhysicalSensorTimestamp = 0x33333333
	state.PhysicalInputMetadata[physicalMetadataR2Status] = 0xA8
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x28
	dev.UpdateInputState(&state)
	// The physical mechanics settle at the same analog peak one report later.
	state.PhysicalSensorTimestamp = 0x44444444
	state.PhysicalInputMetadata[physicalMetadataR2Status] = 0xA9
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x29
	dev.UpdateInputState(&state)
	beforeRelease := dev.InputSchedulerState()
	if beforeRelease.TransitionDepth != 1 ||
		beforeRelease.TransitionHighWater != 1 ||
		!beforeRelease.ContinuousPending {
		t.Fatalf("metadata/mechanical evolution built ordered backlog: %+v",
			beforeRelease)
	}
	state.L2 = 0
	state.PhysicalInputMetadata[physicalMetadataR2Status] = 0xAA
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x09
	dev.UpdateInputState(&state)

	n, peakToken := dev.ClaimInputReport(report[:])
	if n != InputReportSize || peakToken == 0 || report[5] != 255 ||
		report[43] != 0x29 || report[48] != 0x23 {
		t.Fatalf("pending press did not carry stable physical peak: n=%d token=%d report=% x",
			n, peakToken, report[5:55])
	}
	if report[42] != 0xA1 ||
		binary.LittleEndian.Uint32(report[28:32]) != 0x22222222 {
		t.Fatalf("peak strengthening invented unrelated same-report state: % x",
			report[28:49])
	}
	dev.CompleteInputReport(peakToken, true)

	_, releaseToken := dev.ClaimInputReport(report[:])
	if releaseToken == 0 || report[5] != 0 || report[43] != 0x09 {
		t.Fatalf("release did not follow physical peak: token=%d report=% x",
			releaseToken, report[5:49])
	}
	dev.CompleteInputReport(releaseToken, true)
	_, idleToken := dev.ClaimInputReport(report[:])
	if idleToken == 0 || report[5] != 0 || report[43] != 0x09 {
		t.Fatalf("stale physical peak replayed after release: token=%d report=% x",
			idleToken, report[5:49])
	}
	dev.CompleteInputReport(idleToken, true)
}

func TestDualSenseFailedInitialRetryRemainsExactBeforeSettledPhysicalPeak(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	metadata := [InputStatePhysicalMetadataSize]byte{
		0x10, 0x09, 0x09, 1, 2, 3, 4, 0x20,
		0x41, 0x42, 0x43, 0x44, 0x25, 0x01,
	}
	state := inputStateWithPhysicalMetadata(0x11111111, metadata)
	dev.UpdateInputState(&state)
	var report, failed [InputReportSize]byte
	_, token := dev.ClaimInputReport(report[:])
	dev.CompleteInputReport(token, true)

	state.L2 = 1
	state.PhysicalSensorTimestamp = 0x22222222
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x02
	dev.UpdateInputState(&state)
	_, token = dev.ClaimInputReport(failed[:])
	if token == 0 || failed[5] != 1 || failed[43] != 0x02 {
		t.Fatalf("initial physical press was not claimed: token=%d report=% x",
			token, failed[5:55])
	}
	dev.CompleteInputReport(token, false)

	state.L2 = 255
	state.PhysicalSensorTimestamp = 0x33333333
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x28
	dev.UpdateInputState(&state)
	state.PhysicalSensorTimestamp = 0x44444444
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x29
	dev.UpdateInputState(&state)
	state.L2 = 0
	state.PhysicalSensorTimestamp = 0x55555555
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x09
	dev.UpdateInputState(&state)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report != failed {
		t.Fatalf("failed initial physical transition was mutated before retry:\n got=% x\nwant=% x",
			report[5:55], failed[5:55])
	}
	dev.CompleteInputReport(token, true)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[5] != 255 || report[43] != 0x29 ||
		binary.LittleEndian.Uint32(report[28:32]) != 0x44444444 {
		t.Fatalf("settled physical peak did not follow exact retry: token=%d report=% x",
			token, report[5:55])
	}
	dev.CompleteInputReport(token, true)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[5] != 0 || report[43] != 0x09 {
		t.Fatalf("release did not follow settled peak: token=%d report=% x",
			token, report[5:55])
	}
	dev.CompleteInputReport(token, true)
}

func TestDualSenseSettledStatusFollowsOlderQueuedControlTransitions(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	metadata := [InputStatePhysicalMetadataSize]byte{
		0x10, 0x09, 0x09, 1, 2, 3, 4, 0x20,
		0x41, 0x42, 0x43, 0x44, 0x25, 0x01,
	}
	state := inputStateWithPhysicalMetadata(0x11111111, metadata)
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	_, token := dev.ClaimInputReport(report[:])
	dev.CompleteInputReport(token, true)

	state.L2 = 1
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x02
	dev.UpdateInputState(&state)
	state.L2 = 255
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x28
	dev.UpdateInputState(&state)
	state.Buttons |= ButtonCross
	dev.UpdateInputState(&state)
	state.Buttons &^= ButtonCross
	dev.UpdateInputState(&state)
	state.PhysicalSensorTimestamp = 0x29292929
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x29
	dev.UpdateInputState(&state)
	state.L2 = 0
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x09
	dev.UpdateInputState(&state)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[5] != 1 || report[43] != 0x02 ||
		report[8]&byte(ButtonCross) != 0 {
		t.Fatalf("initial press was not preserved truthfully: token=%d report=% x",
			token, report[5:49])
	}
	dev.CompleteInputReport(token, true)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[43] != 0x28 ||
		report[8]&byte(ButtonCross) == 0 {
		t.Fatalf("button-down transition was rewritten: token=%d report=% x",
			token, report[5:49])
	}
	dev.CompleteInputReport(token, true)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[43] != 0x28 ||
		report[8]&byte(ButtonCross) != 0 {
		t.Fatalf("button-up transition was rewritten: token=%d report=% x",
			token, report[5:49])
	}
	dev.CompleteInputReport(token, true)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[5] != 255 || report[43] != 0x29 ||
		binary.LittleEndian.Uint32(report[28:32]) != 0x29292929 {
		t.Fatalf("settled complete peak was not promoted after controls: token=%d report=% x",
			token, report[5:55])
	}
	dev.CompleteInputReport(token, true)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[5] != 0 || report[43] != 0x09 {
		t.Fatalf("release did not follow settled peak: token=%d report=% x",
			token, report[5:49])
	}
	dev.CompleteInputReport(token, true)
}

func TestDualSenseTruthfulLaterTransitionDoesNotRewriteInitialStatus(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	metadata := [InputStatePhysicalMetadataSize]byte{
		0x10, 0x09, 0x09, 1, 2, 3, 4, 0x20,
		0x41, 0x42, 0x43, 0x44, 0x25, 0x01,
	}
	state := inputStateWithPhysicalMetadata(0x11111111, metadata)
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	_, token := dev.ClaimInputReport(report[:])
	dev.CompleteInputReport(token, true)

	state.L2 = 255
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x28
	dev.UpdateInputState(&state)
	state.Buttons |= ButtonCross
	state.PhysicalSensorTimestamp = 0x29292929
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x29
	dev.UpdateInputState(&state)
	state.L2 = 0
	state.Buttons &^= ButtonL2
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x09
	dev.UpdateInputState(&state)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[5] != 255 || report[43] != 0x28 ||
		report[8]&byte(ButtonCross) != 0 {
		t.Fatalf("initial peak was rewritten by later status: token=%d report=% x",
			token, report[5:49])
	}
	dev.CompleteInputReport(token, true)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[5] != 255 || report[43] != 0x29 ||
		report[8]&byte(ButtonCross) == 0 {
		t.Fatalf("truthful later transition was not preserved: token=%d report=% x",
			token, report[5:49])
	}
	dev.CompleteInputReport(token, true)

	_, token = dev.ClaimInputReport(report[:])
	if token == 0 || report[5] != 0 || report[43] != 0x09 {
		t.Fatalf("release did not follow later peak transition: token=%d report=% x",
			token, report[5:49])
	}
	dev.CompleteInputReport(token, true)
}

func TestDualSensePeakStatusDoesNotMergeAcrossPhysicalLayouts(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	metadata := [InputStatePhysicalMetadataSize]byte{
		0x10, 0x09, 0x02, 1, 2, 3, 4, 0x20,
		0x11, 0x12, 0x13, 0x14, 0x25, 0x01,
	}
	state := inputStateWithPhysicalMetadata(0x11111111, metadata)
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	_, baselineToken := dev.ClaimInputReport(report[:])
	dev.CompleteInputReport(baselineToken, true)

	state.L2 = 1
	state.PhysicalSensorTimestamp = 0x22222222
	dev.UpdateInputState(&state)
	state.L2 = 255
	state.PhysicalSensorTimestamp = 0x33333333
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x28
	dev.UpdateInputState(&state)
	// Simulate a reconnect/client boundary that changes the physical metadata
	// layout at the same analog peak. This later state must remain complete;
	// its Edge status cannot be folded into the pending base-layout press.
	state.PhysicalMetadataEdgeLayout = true
	state.PhysicalSensorTimestamp = 0x44444444
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x29
	copy(state.PhysicalInputMetadata[8:12], []byte{0x80, 0x51, 0x52, 0x53})
	dev.UpdateInputState(&state)
	state.L2 = 0
	state.PhysicalSensorTimestamp = 0x55555555
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x09
	dev.UpdateInputState(&state)

	_, pressToken := dev.ClaimInputReport(report[:])
	if pressToken == 0 || report[5] != 1 || report[43] != 0x02 ||
		[4]byte(report[49:53]) != [4]byte{0x11, 0x12, 0x13, 0x14} {
		t.Fatalf("base pending press was cross-layout mutated: token=%d report=% x",
			pressToken, report[5:55])
	}
	dev.CompleteInputReport(pressToken, true)

	_, peakToken := dev.ClaimInputReport(report[:])
	if peakToken == 0 || report[5] != 255 || report[43] != 0x29 ||
		binary.LittleEndian.Uint32(report[28:32]) != 0x44444444 {
		t.Fatalf("separate Edge-layout peak was not preserved: token=%d report=% x",
			peakToken, report[5:55])
	}
	if [4]byte(report[49:53]) == [4]byte{0x80, 0x51, 0x52, 0x53} {
		t.Fatalf("Edge raw49:52 leaked into base virtual layout: % x",
			report[49:53])
	}
	dev.CompleteInputReport(peakToken, true)

	_, releaseToken := dev.ClaimInputReport(report[:])
	if releaseToken == 0 || report[5] != 0 || report[43] != 0x09 {
		t.Fatalf("release did not follow separate layout peak: token=%d report=% x",
			releaseToken, report[5:55])
	}
	dev.CompleteInputReport(releaseToken, true)
}

func TestDualSensePhysicalMetadataRetryAndReconnectOrdering(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	generationA := dev.input.currentGeneration()
	metadata := [InputStatePhysicalMetadataSize]byte{
		1, 0x09, 0x09, 0, 0, 0, 0, 0, 1, 2, 3, 4, 0x21, 1,
	}
	state := inputStateWithPhysicalMetadata(0x11112222, metadata)
	dev.updateInputStateForGeneration(generationA, &state)
	var report, retry [InputReportSize]byte
	_, baselineToken := dev.ClaimInputReport(report[:])
	dev.CompleteInputReport(baselineToken, true)

	state.R2 = 190
	state.PhysicalSensorTimestamp = 0x33334444
	state.PhysicalInputMetadata[physicalMetadataR2Status] = 0x21
	state.PhysicalInputMetadata[physicalMetadataEffectStatus] = 0x02
	dev.updateInputStateForGeneration(generationA, &state)
	_, pressToken := dev.ClaimInputReport(report[:])
	if pressToken == 0 || report[6] != 190 || report[42] != 0x21 ||
		report[48] != 0x02 {
		t.Fatalf("generation A physical press claim: token=%d report=% x",
			pressToken, report[6:49])
	}

	generationB := dev.beginInputStreamGeneration()
	state.R2 = 0
	state.PhysicalSensorTimestamp = 0x55556666
	state.PhysicalInputMetadata[physicalMetadataR2Status] = 0x09
	state.PhysicalInputMetadata[physicalMetadataEffectStatus] = 0
	dev.updateInputStateForGeneration(generationB, &state)
	dev.CompleteInputReport(pressToken, false)

	_, retryToken := dev.ClaimInputReport(retry[:])
	if retryToken == 0 || retry != report {
		t.Fatalf("failed physical claim was not retried byte-for-byte: token=%d\n got=% x\nwant=% x",
			retryToken, retry[:55], report[:55])
	}
	dev.CompleteInputReport(retryToken, true)
	_, releaseToken := dev.ClaimInputReport(report[:])
	if releaseToken == 0 || report[6] != 0 || report[42] != 0x09 ||
		report[48] != 0 ||
		binary.LittleEndian.Uint32(report[28:32]) != 0x55556666 {
		t.Fatalf("generation B physical release did not follow retry: token=%d report=% x",
			releaseToken, report[6:55])
	}
	dev.CompleteInputReport(releaseToken, true)
}

func TestDualSenseInputHotPathAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("race instrumentation allocates; allocation contract is tested without -race")
	}
	scheduler := newDualSenseInputScheduler(BatteryFullyCharged, false)
	state := neutralInputState()
	scheduler.update(&state, 0)
	allocations := testing.AllocsPerRun(1000, func() {
		state.GyroX++
		scheduler.update(&state, 0)
	})
	if allocations != 0 {
		t.Fatalf("scheduler update allocated %.2f objects/run", allocations)
	}

	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	allocations = testing.AllocsPerRun(1000, func() {
		n, token := dev.ClaimInputReport(report[:])
		if n != 0 {
			dev.CompleteInputReport(token, true)
		}
	})
	if allocations != 0 {
		t.Fatalf("select/encode allocated %.2f objects/run", allocations)
	}

	edgeDev, err := NewEdge(nil)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	edgeDev.UpdateInputState(&state)
	allocations = testing.AllocsPerRun(1000, func() {
		n, token := edgeDev.ClaimInputReport(report[:])
		if n != 0 {
			edgeDev.CompleteInputReport(token, true)
		}
	})
	if allocations != 0 {
		t.Fatalf("Edge select/encode allocated %.2f objects/run", allocations)
	}

	// Exercise both receive-to-selection and receive-to-presentation histogram
	// updates with a newly received state on every measured iteration.
	telemetryDev, err := New(nil)
	if err != nil {
		t.Fatalf("New telemetry device: %v", err)
	}
	telemetryState := neutralInputState()
	allocations = testing.AllocsPerRun(1000, func() {
		telemetryState.GyroX++
		telemetryDev.UpdateInputState(&telemetryState)
		n, token := telemetryDev.ClaimInputReport(report[:])
		if n != InputReportSize || token == 0 {
			panic("received state was not claimed")
		}
		telemetryDev.CompleteInputReport(token, true)
	})
	if allocations != 0 {
		t.Fatalf("receive/select/present telemetry allocated %.2f objects/run",
			allocations)
	}

	var payload [InputStateSize]byte
	state.Touch1Active = true
	state.Touch1Tracking = 0
	state.Touch2Active = true
	state.Touch2Tracking = 0
	if err := state.MarshalInto(payload[:]); err != nil {
		t.Fatalf("MarshalInto: %v", err)
	}
	if payload[15] != 0 || payload[20] != 0 {
		t.Fatalf("active tracking ID zero was rewritten: %#x %#x",
			payload[15], payload[20])
	}
	var decoded InputState
	header := [...]byte{StreamFrameVersionV5, StreamFrameInputState, InputStateSize, 0}
	allocations = testing.AllocsPerRun(1000, func() {
		if state.MarshalInto(payload[:]) != nil {
			panic("V5 state marshal failed")
		}
		_ = framedStreamCRC(header[:], payload[:])
		_ = decoded.UnmarshalBinary(payload[:])
	})
	if allocations != 0 {
		t.Fatalf("V5 marshal/CRC/decode allocated %.2f objects/run", allocations)
	}

	state.PhysicalMetadataValid = true
	state.PhysicalSensorTimestamp = 0x12345678
	state.PhysicalInputMetadata = [InputStatePhysicalMetadataSize]byte{
		1, 0x09, 0x29, 2, 3, 4, 5, 0x20, 6, 7, 8, 9, 0x2A, 1,
	}
	var rawPayload [InputStateRawSize]byte
	var rawDecoded InputState
	rawHeader := [...]byte{
		StreamFrameVersionV5, StreamFrameInputState, InputStateRawSize, 0,
	}
	allocations = testing.AllocsPerRun(1000, func() {
		if state.MarshalRawInputInto(rawPayload[:]) != nil {
			panic("V5 raw-input state marshal failed")
		}
		_ = framedStreamCRC(rawHeader[:], rawPayload[:])
		_ = rawDecoded.UnmarshalBinary(rawPayload[:])
	})
	if allocations != 0 {
		t.Fatalf("V5 raw marshal/CRC/decode allocated %.2f objects/run",
			allocations)
	}
}

func BenchmarkDualSenseInputSchedulerUpdate(b *testing.B) {
	scheduler := newDualSenseInputScheduler(BatteryFullyCharged, false)
	state := neutralInputState()
	scheduler.update(&state, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		state.GyroX++
		scheduler.update(&state, 0)
	}
}

func BenchmarkDualSenseInputSelectEncode(b *testing.B) {
	dev, err := New(nil)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	state := neutralInputState()
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, token := dev.ClaimInputReport(report[:])
		dev.CompleteInputReport(token, true)
	}
}

func BenchmarkDualSenseEdgeInputSelectEncode(b *testing.B) {
	dev, err := NewEdge(nil)
	if err != nil {
		b.Fatalf("NewEdge: %v", err)
	}
	state := neutralInputState()
	dev.UpdateInputState(&state)
	var report [InputReportSize]byte
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, token := dev.ClaimInputReport(report[:])
		dev.CompleteInputReport(token, true)
	}
}

func BenchmarkDualSenseV5CRCDecode(b *testing.B) {
	state := neutralInputState()
	var payload [InputStateSize]byte
	if err := state.MarshalInto(payload[:]); err != nil {
		b.Fatalf("MarshalInto: %v", err)
	}
	header := [...]byte{StreamFrameVersionV5, StreamFrameInputState, InputStateSize, 0}
	var decoded InputState
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = framedStreamCRC(header[:], payload[:])
		_ = decoded.UnmarshalBinary(payload[:])
	}
}

func BenchmarkDualSenseV5RawInputCRCDecode(b *testing.B) {
	state := neutralInputState()
	state.PhysicalMetadataValid = true
	state.PhysicalSensorTimestamp = 0x12345678
	state.PhysicalInputMetadata = [InputStatePhysicalMetadataSize]byte{
		1, 0x09, 0x29, 2, 3, 4, 5, 0x20, 6, 7, 8, 9, 0x2A, 1,
	}
	var payload [InputStateRawSize]byte
	if err := state.MarshalRawInputInto(payload[:]); err != nil {
		b.Fatalf("MarshalRawInputInto: %v", err)
	}
	header := [...]byte{
		StreamFrameVersionV5, StreamFrameInputState, InputStateRawSize, 0,
	}
	var decoded InputState
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = framedStreamCRC(header[:], payload[:])
		_ = decoded.UnmarshalBinary(payload[:])
	}
}
