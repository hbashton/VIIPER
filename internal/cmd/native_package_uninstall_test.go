package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type fakeNativePackageUninstallTransaction struct {
	events              []string
	fail                string
	preflightErr        error
	closeErr            error
	restoreErr          error
	removeResult        nativePackageRemoveResult
	snapshot            nativePackageUninstallServiceSnapshot
	topologyGeneration  string
	topologyMutations   int
	trustMutations      int
	cancelAt            string
	cancel              context.CancelFunc
	restoreHadDeadline  bool
	cleanupHadDeadline  bool
	finalizeHadDeadline bool
	unsafeStop          bool
	cleanupReboot       bool
}

func (f *fakeNativePackageUninstallTransaction) event(name string) error {
	f.events = append(f.events, name)
	if f.cancelAt == name && f.cancel != nil {
		f.cancel()
	}
	if f.fail == name {
		return errors.New(name + " failure")
	}
	return nil
}

func (f *fakeNativePackageUninstallTransaction) LockTrust(context.Context) error {
	return f.event("trust-lock")
}

func (f *fakeNativePackageUninstallTransaction) LockPackage(context.Context) error {
	return f.event("package-lock")
}

func (f *fakeNativePackageUninstallTransaction) FinalizeTrust(ctx context.Context) error {
	_, f.finalizeHadDeadline = ctx.Deadline()
	f.trustMutations++
	return f.event("trust-finalize")
}

func (f *fakeNativePackageUninstallTransaction) LockService(context.Context) error {
	return f.event("service-lock")
}

func (f *fakeNativePackageUninstallTransaction) Preflight(context.Context) error {
	if err := f.event("preflight"); err != nil {
		return err
	}
	return f.preflightErr
}

func (f *fakeNativePackageUninstallTransaction) InspectService(context.Context) (nativePackageUninstallServiceSnapshot, error) {
	return f.snapshot, f.event("inspect")
}

func (f *fakeNativePackageUninstallTransaction) StopService(
	_ context.Context, snapshot nativePackageUninstallServiceSnapshot,
) error {
	if snapshot != f.snapshot {
		return errors.New("service snapshot changed")
	}
	f.topologyMutations++
	f.topologyGeneration = "stopped"
	err := f.event("stop")
	if err != nil && f.unsafeStop {
		return &nativePackageUninstallUnsafeRestoreError{cause: err}
	}
	return err
}

func (f *fakeNativePackageUninstallTransaction) RemoveDriver(context.Context) (nativePackageRemoveResult, error) {
	f.topologyMutations++
	f.topologyGeneration = "removed"
	return f.removeResult, f.event("remove")
}

func (f *fakeNativePackageUninstallTransaction) Cleanup(
	ctx context.Context, snapshot nativePackageUninstallServiceSnapshot,
) (bool, error) {
	if snapshot != f.snapshot {
		return false, errors.New("service snapshot changed")
	}
	f.topologyMutations++
	f.topologyGeneration = "cleaned"
	_, f.cleanupHadDeadline = ctx.Deadline()
	return f.cleanupReboot, f.event("cleanup")
}

func (f *fakeNativePackageUninstallTransaction) RestoreService(
	ctx context.Context, snapshot nativePackageUninstallServiceSnapshot,
) error {
	if snapshot != f.snapshot {
		return errors.New("service snapshot changed")
	}
	f.topologyMutations++
	f.topologyGeneration = "restored"
	f.events = append(f.events, "restore")
	_, f.restoreHadDeadline = ctx.Deadline()
	return f.restoreErr
}

func (f *fakeNativePackageUninstallTransaction) Close() error {
	f.events = append(f.events, "close")
	return f.closeErr
}

func TestNativePackageUninstallUsesFixedLockAndCommitOrder(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageUninstallTransaction{snapshot: nativePackageUninstallServiceSnapshot{
		exists: true, wasRunning: true,
	}}
	if err := runNativePackageUninstallTransaction(
		context.Background(), nativePackageTestLogger(), fake,
	); err != nil {
		t.Fatalf("run uninstall: %v", err)
	}
	want := []string{
		"trust-lock", "package-lock", "service-lock", "preflight", "inspect",
		"stop", "remove", "cleanup", "trust-finalize", "close",
	}
	if !reflect.DeepEqual(fake.events, want) {
		t.Fatalf("events=%v want=%v", fake.events, want)
	}
	if !fake.cleanupHadDeadline {
		t.Fatal("committed driver removal cleanup did not receive a bounded reconciliation context")
	}
	if !fake.finalizeHadDeadline {
		t.Fatal("local-test trust finalization did not receive a bounded reconciliation context")
	}
}

func TestNativePackageLocalTestUninstallTopologyAuthorityMatrix(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		state string
		want  bool
	}{
		{state: "preparing", want: false},
		{state: "pending", want: true},
		{state: "owned", want: true},
		{state: "uninstalling", want: true},
		{state: "cleared", want: false},
		{state: "absent", want: false},
		{state: "unknown", want: false},
	} {
		test := test
		t.Run(test.state, func(t *testing.T) {
			t.Parallel()
			if got := nativePackageLocalTestUninstallMayMutateTopology(test.state); got != test.want {
				t.Fatalf("state=%q mayMutate=%v want=%v", test.state, got, test.want)
			}
		})
	}
}

func TestNativePackageProductionUninstallLocalTrustAdmission(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		states  []string
		wantErr bool
	}{
		{name: "absent"},
		{name: "cleared", states: []string{"cleared"}},
		{name: "preparing", states: []string{"preparing"}, wantErr: true},
		{name: "pending", states: []string{"pending"}, wantErr: true},
		{name: "owned", states: []string{"owned"}, wantErr: true},
		{name: "uninstalling", states: []string{"uninstalling"}, wantErr: true},
		{name: "multiple", states: []string{"cleared", "owned"}, wantErr: true},
		{name: "unknown", states: []string{"future"}, wantErr: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := nativePackageProductionUninstallLocalTrustAdmission(test.states)
			if (err != nil) != test.wantErr {
				t.Fatalf("states=%v error=%v wantErr=%v", test.states, err, test.wantErr)
			}
		})
	}
}

func TestNativePackageSettledLocalTestUninstallCannotRemoveProductionSuccessor(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"absent", "cleared"} {
		state := state
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			// Model local owned -> uninstall -> cleared -> production install,
			// followed by a replay of the now-stale source-bound local request.
			const productionGeneration = "production-successor"
			fake := &fakeNativePackageUninstallTransaction{
				preflightErr:       &nativePackageUninstallAlreadySettledError{state: state},
				topologyGeneration: productionGeneration,
				snapshot:           nativePackageUninstallServiceSnapshot{exists: true, wasRunning: true},
			}
			if err := runNativePackageUninstallTransaction(
				context.Background(), nativePackageTestLogger(), fake,
			); err != nil {
				t.Fatalf("settled stale local uninstall: %v", err)
			}
			wantEvents := []string{
				"trust-lock", "package-lock", "service-lock", "preflight", "close",
			}
			if !reflect.DeepEqual(fake.events, wantEvents) {
				t.Fatalf("stale local uninstall crossed topology boundary: events=%v want=%v", fake.events, wantEvents)
			}
			if fake.topologyMutations != 0 || fake.trustMutations != 0 {
				t.Fatalf("stale local uninstall mutated successor: topology=%d trust=%d",
					fake.topologyMutations, fake.trustMutations)
			}
			if fake.topologyGeneration != productionGeneration {
				t.Fatalf("production topology changed to %q", fake.topologyGeneration)
			}
		})
	}
}

func TestNativePackageUninstallFailureMatrix(t *testing.T) {
	t.Parallel()
	for _, fail := range []string{
		"trust-lock", "package-lock", "service-lock", "preflight", "inspect", "stop", "remove", "cleanup", "trust-finalize",
	} {
		fail := fail
		t.Run(fail, func(t *testing.T) {
			t.Parallel()
			fake := &fakeNativePackageUninstallTransaction{
				fail: fail, snapshot: nativePackageUninstallServiceSnapshot{exists: true},
			}
			if fail == "remove" {
				fake.removeResult.serviceRestoreVerified = true
			}
			err := runNativePackageUninstallTransaction(
				context.Background(), nativePackageTestLogger(), fake,
			)
			if err == nil || !strings.Contains(err.Error(), fail+" failure") {
				t.Fatalf("error=%v events=%v", err, fake.events)
			}
			restoreExpected := fail == "stop" || fail == "remove"
			restoreSeen := false
			for _, event := range fake.events {
				restoreSeen = restoreSeen || event == "restore"
			}
			if restoreSeen != restoreExpected {
				t.Fatalf("events=%v restoreExpected=%v", fake.events, restoreExpected)
			}
			if restoreSeen && !fake.restoreHadDeadline {
				t.Fatal("service restoration did not receive a bounded independent context")
			}
			if fake.events[len(fake.events)-1] != "close" {
				t.Fatalf("transaction did not close: %v", fake.events)
			}
		})
	}
}

func TestNativePackageUninstallCancellationBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cancelAt    string
		wantEvents  []string
		wantRestore bool
	}{
		{cancelAt: "trust-lock", wantEvents: []string{"trust-lock", "close"}},
		{cancelAt: "package-lock", wantEvents: []string{"trust-lock", "package-lock", "close"}},
		{cancelAt: "service-lock", wantEvents: []string{"trust-lock", "package-lock", "service-lock", "close"}},
		{cancelAt: "preflight", wantEvents: []string{"trust-lock", "package-lock", "service-lock", "preflight", "close"}},
		{cancelAt: "inspect", wantEvents: []string{"trust-lock", "package-lock", "service-lock", "preflight", "inspect", "close"}},
		{cancelAt: "stop", wantEvents: []string{"trust-lock", "package-lock", "service-lock", "preflight", "inspect", "stop", "restore", "close"}, wantRestore: true},
	}
	for _, test := range cases {
		test := test
		t.Run(test.cancelAt, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			fake := &fakeNativePackageUninstallTransaction{
				cancelAt: test.cancelAt, cancel: cancel,
				snapshot: nativePackageUninstallServiceSnapshot{exists: true},
			}
			err := runNativePackageUninstallTransaction(ctx, nativePackageTestLogger(), fake)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v events=%v", err, fake.events)
			}
			if !reflect.DeepEqual(fake.events, test.wantEvents) {
				t.Fatalf("events=%v want=%v", fake.events, test.wantEvents)
			}
			if test.wantRestore && !fake.restoreHadDeadline {
				t.Fatal("cancellation restoration did not receive an independent deadline")
			}
		})
	}
}

func TestNativePackageUninstallHelperSuccessAtDeadlineStillCleans(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeNativePackageUninstallTransaction{cancelAt: "remove", cancel: cancel}
	if err := runNativePackageUninstallTransaction(ctx, nativePackageTestLogger(), fake); err != nil {
		t.Fatalf("authoritative helper success was contradicted by caller cancellation: %v", err)
	}
	if !slicesContainString(fake.events, "cleanup") || slicesContainString(fake.events, "restore") {
		t.Fatalf("helper success reconciliation events=%v", fake.events)
	}
}

func TestNativePackageUninstallReportsRestoreAndCloseFailures(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageUninstallTransaction{
		fail: "remove", restoreErr: errors.New("restore failure"), closeErr: errors.New("close failure"),
		removeResult: nativePackageRemoveResult{serviceRestoreVerified: true},
		snapshot:     nativePackageUninstallServiceSnapshot{exists: true},
	}
	err := runNativePackageUninstallTransaction(context.Background(), nativePackageTestLogger(), fake)
	for _, fragment := range []string{"remove failure", "restore failure", "close failure"} {
		if err == nil || !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error=%v missing %q", err, fragment)
		}
	}
}

func TestNativePackageUninstallLeavesBrokerStoppedWhenDriverRollbackIsUnverified(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageUninstallTransaction{
		fail:         "remove",
		removeResult: nativePackageRemoveResult{serviceRestoreVerified: false},
		snapshot:     nativePackageUninstallServiceSnapshot{exists: true},
	}
	err := runNativePackageUninstallTransaction(context.Background(), nativePackageTestLogger(), fake)
	if err == nil || !strings.Contains(err.Error(), "deliberately left stopped") {
		t.Fatalf("error=%v events=%v", err, fake.events)
	}
	if slicesContainString(fake.events, "restore") {
		t.Fatalf("unverified driver rollback restarted broker: %v", fake.events)
	}
}

func TestNativePackageUninstallDoesNotRestoreAbsentBroker(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageUninstallTransaction{
		fail: "remove", snapshot: nativePackageUninstallServiceSnapshot{},
	}
	err := runNativePackageUninstallTransaction(context.Background(), nativePackageTestLogger(), fake)
	if err == nil || !strings.Contains(err.Error(), "remove failure") {
		t.Fatalf("error=%v events=%v", err, fake.events)
	}
	if strings.Contains(err.Error(), "deliberately left stopped") || slicesContainString(fake.events, "restore") {
		t.Fatalf("absent broker was treated as restorable state: error=%v events=%v", err, fake.events)
	}
}

func TestNativePackageUninstallLeavesBrokerStoppedWhenManagedFileIdentityChanges(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageUninstallTransaction{
		fail: "stop", unsafeStop: true,
		snapshot: nativePackageUninstallServiceSnapshot{exists: true, wasRunning: true},
	}
	err := runNativePackageUninstallTransaction(context.Background(), nativePackageTestLogger(), fake)
	if err == nil || !strings.Contains(err.Error(), "deliberately left stopped") {
		t.Fatalf("error=%v events=%v", err, fake.events)
	}
	if slicesContainString(fake.events, "restore") {
		t.Fatalf("changed managed file identity restarted broker: %v", fake.events)
	}
}

func TestNativePackageUninstallRebootSuccessCleansBefore3010(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageUninstallTransaction{
		removeResult: nativePackageRemoveResult{rebootRequired: true},
	}
	err := runNativePackageUninstallTransaction(context.Background(), nativePackageTestLogger(), fake)
	var exitCoder interface{ ExitCode() int }
	if !errors.As(err, &exitCoder) || exitCoder.ExitCode() != nativePackageRebootRequiredCode {
		t.Fatalf("error=%v exitCoder=%T", err, exitCoder)
	}
	want := []string{
		"trust-lock", "package-lock", "service-lock", "preflight", "inspect",
		"stop", "remove", "cleanup", "close",
	}
	if !reflect.DeepEqual(fake.events, want) {
		t.Fatalf("events=%v want=%v", fake.events, want)
	}
}

func TestNativePackageUninstallDoesNotReport3010WhenOwnedCleanupFails(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageUninstallTransaction{
		fail:         "cleanup",
		removeResult: nativePackageRemoveResult{rebootRequired: true},
	}
	err := runNativePackageUninstallTransaction(
		context.Background(), nativePackageTestLogger(), fake,
	)
	if err == nil || !strings.Contains(err.Error(), "cleanup failure") ||
		!strings.Contains(err.Error(), "restart still required") {
		t.Fatalf("error=%v events=%v", err, fake.events)
	}
	var exitCoder interface{ ExitCode() int }
	if errors.As(err, &exitCoder) {
		t.Fatalf("partial owned cleanup was misreported as reboot-success exit %d", exitCoder.ExitCode())
	}
	if slicesContainString(fake.events, "restore") {
		t.Fatalf("driverless broker was restored after cleanup failure: %v", fake.events)
	}
}

func TestNativePackageUninstallSelfImageCleanupAggregates3010(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageUninstallTransaction{cleanupReboot: true}
	err := runNativePackageUninstallTransaction(context.Background(), nativePackageTestLogger(), fake)
	var exitCoder interface{ ExitCode() int }
	if !errors.As(err, &exitCoder) || exitCoder.ExitCode() != nativePackageRebootRequiredCode {
		t.Fatalf("error=%v exitCoder=%T", err, exitCoder)
	}
	if !slicesContainString(fake.events, "cleanup") || slicesContainString(fake.events, "restore") {
		t.Fatalf("self-image cleanup reconciliation events=%v", fake.events)
	}
}

func TestNativePackageUninstallIdempotentAbsenceStillReconcilesDriver(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageUninstallTransaction{snapshot: nativePackageUninstallServiceSnapshot{}}
	if err := runNativePackageUninstallTransaction(
		context.Background(), nativePackageTestLogger(), fake,
	); err != nil {
		t.Fatalf("idempotent uninstall: %v", err)
	}
	for _, required := range []string{"stop", "remove", "cleanup"} {
		if !strings.Contains(strings.Join(fake.events, ","), required) {
			t.Fatalf("absence skipped %s reconciliation: %v", required, fake.events)
		}
	}
}

func TestNativePackageRemoveStructuredExitSemantics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		line        string
		exit        int
		reboot      bool
		wantErr     bool
		errContains string
	}{
		{name: "success", line: "result=success operation=remove changed=1 rebootRequired=0 rollback=not-needed exitCode=0", exit: 0},
		{name: "idempotent success", line: "result=success operation=remove changed=0 rebootRequired=0 rollback=not-needed exitCode=0", exit: 0},
		{name: "reboot success", line: "result=success operation=remove changed=1 rebootRequired=1 rollback=not-needed exitCode=3010", exit: 3010, reboot: true},
		{name: "preflight", line: `result=error operation=remove changed=0 rebootRequired=0 rollback=not-needed exitCode=4 phase="remove-topology" win32Error=13 message="rejected"`, exit: 4, wantErr: true, errContains: "before mutation"},
		{name: "rolled back", line: `result=error operation=remove changed=1 rebootRequired=0 rollback=succeeded exitCode=1 phase="remove-driver" win32Error=5 message="failed"`, exit: 1, wantErr: true, errContains: "rolled back"},
		{name: "rolled back pending reboot", line: `result=error operation=remove changed=1 rebootRequired=1 rollback=succeeded exitCode=1 phase="remove-driver" win32Error=5 message="failed"`, exit: 1, wantErr: true, errContains: "rolled back"},
		{name: "rollback failed", line: `result=error operation=remove changed=1 rebootRequired=1 rollback=failed exitCode=3 phase="remove-rollback" win32Error=5 nestedExitCode=1 message="failed" recoveryRecord="C:\\ProgramData\\active-v2" recoveryRecordWritten=0 recoveryRecordPhase="journal-write" recoveryRecordWin32Error=112 recoveryRecordMessage="full" recoveryBackup="C:\\ProgramData\\backup" recoveryBackupRetained=1`, exit: 3, wantErr: true, errContains: "rollback failed"},
		{name: "exit mismatch", line: "result=success operation=remove changed=1 rebootRequired=0 rollback=not-needed exitCode=0", exit: 1, wantErr: true, errContains: "disagreed"},
		{name: "invalid 3010", line: "result=success operation=remove changed=1 rebootRequired=0 rollback=not-needed exitCode=3010", exit: 3010, wantErr: true, errContains: "invalid reboot-success"},
		{name: "unchanged 3010", line: "result=success operation=remove changed=0 rebootRequired=1 rollback=not-needed exitCode=3010", exit: 3010, wantErr: true, errContains: "invalid reboot-success"},
		{name: "success trailing field", line: "result=success operation=remove changed=1 rebootRequired=0 rollback=not-needed exitCode=0 phase=spoof", exit: 0, wantErr: true, errContains: "warning"},
		{name: "error missing evidence", line: `result=error operation=remove changed=0 rebootRequired=0 rollback=not-needed exitCode=4 phase="remove-topology"`, exit: 4, wantErr: true, errContains: "win32Error"},
		{name: "unstructured", line: "removed", exit: 0, wantErr: true, errContains: "exactly one"},
		{name: "duplicate proof", line: "result=success operation=remove changed=0 rebootRequired=0 rollback=not-needed exitCode=0\nresult=success operation=remove changed=0 rebootRequired=0 rollback=not-needed exitCode=0", exit: 0, wantErr: true, errContains: "exactly one"},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := parseNativePackageRemoveProof(test.line, test.exit)
			if (err != nil) != test.wantErr {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if test.errContains != "" && (err == nil || !strings.Contains(err.Error(), test.errContains)) {
				t.Fatalf("error=%v missing %q", err, test.errContains)
			}
			if err == nil && result.rebootRequired != test.reboot {
				t.Fatalf("reboot=%v want=%v", result.rebootRequired, test.reboot)
			}
			if (test.name == "preflight" || test.name == "rolled back") &&
				!result.serviceRestoreVerified {
				t.Fatal("structured no-mutation/rollback proof did not authorize exact broker restoration")
			}
			if (test.name == "rollback failed" || test.name == "unstructured" ||
				test.name == "rolled back pending reboot") &&
				result.serviceRestoreVerified {
				t.Fatal("indeterminate helper outcome authorized broker restoration")
			}
		})
	}
}

func TestNativePackageRemoveRetainedTombstoneProofIsExactAndBounded(t *testing.T) {
	t.Parallel()
	tombstone := `C:\ProgramData\VIIPER-UdeCx-RemoveTransactions\settled-v2-` + strings.Repeat("a", 64)
	base := "result=success operation=remove changed=1 rebootRequired=0 rollback=not-needed exitCode=0"
	warning := " warning=\"remove-settled-cleanup-retained\" warningWin32Error=5 retainedTombstone=" + strconv.Quote(tombstone)
	result, err := parseNativePackageRemoveProof(base+warning, 0)
	if err != nil {
		t.Fatalf("parse exact retained tombstone proof: %v", err)
	}
	if result.retainedTombstone != tombstone || result.retainedTombstoneWin32Error != 5 {
		t.Fatalf("retained tombstone result=%+v", result)
	}
	rolledBack := `result=error operation=remove changed=1 rebootRequired=0 rollback=succeeded exitCode=1 phase="remove-driver" win32Error=5 message="rolled back"`
	result, err = parseNativePackageRemoveProof(rolledBack+warning, 1)
	if err == nil || !strings.Contains(err.Error(), "rolled back") ||
		result.retainedTombstone != tombstone || result.retainedTombstoneWin32Error != 5 ||
		!result.serviceRestoreVerified {
		t.Fatalf("rolled-back retained tombstone result=%+v error=%v", result, err)
	}

	cases := map[string]string{
		"missing code":       base + ` warning="remove-settled-cleanup-retained" retainedTombstone=` + strconv.Quote(tombstone),
		"zero code":          base + ` warning="remove-settled-cleanup-retained" warningWin32Error=0 retainedTombstone=` + strconv.Quote(tombstone),
		"overflow code":      base + ` warning="remove-settled-cleanup-retained" warningWin32Error=4294967296 retainedTombstone=` + strconv.Quote(tombstone),
		"wrong warning":      base + ` warning="unknown" warningWin32Error=5 retainedTombstone=` + strconv.Quote(tombstone),
		"relative path":      base + ` warning="remove-settled-cleanup-retained" warningWin32Error=5 retainedTombstone="settled-v2-` + strings.Repeat("a", 64) + `"`,
		"wrong directory":    base + ` warning="remove-settled-cleanup-retained" warningWin32Error=5 retainedTombstone=` + strconv.Quote(`C:\Other\settled-v2-`+strings.Repeat("a", 64)),
		"bad identity":       base + ` warning="remove-settled-cleanup-retained" warningWin32Error=5 retainedTombstone=` + strconv.Quote(strings.TrimSuffix(tombstone, "a")+"g"),
		"duplicate warning":  base + warning + warning,
		"trailing field":     base + warning + ` ignored=1`,
		"reordered tuple":    base + ` warning="remove-settled-cleanup-retained" retainedTombstone=` + strconv.Quote(tombstone) + ` warningWin32Error=5`,
		"unescaped path":     base + ` warning="remove-settled-cleanup-retained" warningWin32Error=5 retainedTombstone="C:\ProgramData"`,
		"duplicate base key": base + ` changed=1`,
	}
	for name, line := range cases {
		name, line := name, line
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if result, err := parseNativePackageRemoveProof(line, 0); err == nil {
				t.Fatalf("malformed warning proof accepted: %+v", result)
			}
		})
	}
}

func TestNativePackageUninstallSurfacesRetainedTombstoneWarning(t *testing.T) {
	t.Parallel()
	tombstone := `C:\ProgramData\VIIPER-UdeCx-RemoveTransactions\settled-v2-` + strings.Repeat("b", 64)
	fake := &fakeNativePackageUninstallTransaction{
		removeResult: nativePackageRemoveResult{
			retainedTombstone:           tombstone,
			retainedTombstoneWin32Error: 5,
		},
	}
	var records bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&records, nil))
	if err := runNativePackageUninstallTransaction(context.Background(), logger, fake); err != nil {
		t.Fatalf("run uninstall: %v", err)
	}
	for _, evidence := range []string{
		"Native remove journal retired with a retained settled tombstone",
		"warning=remove-settled-cleanup-retained",
		"win32Error=5",
		"retainedTombstone=" + tombstone,
	} {
		if !strings.Contains(records.String(), evidence) {
			t.Fatalf("warning log %q missing %q", records.String(), evidence)
		}
	}
}

func slicesContainString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestNativePackageUninstallRequestFailsClosed(t *testing.T) {
	t.Parallel()
	base := nativePackageUninstallRequest{
		driverHelper:         `C:\bundle\ViiperUdeCtl.exe`,
		expectedHelperSHA256: strings.Repeat("a", 64),
		targetUserSID:        "S-1-5-21-1-2-3-1001",
	}
	if err := base.validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	cases := map[string]func(*nativePackageUninstallRequest){
		"empty helper":    func(r *nativePackageUninstallRequest) { r.driverHelper = "" },
		"empty SID":       func(r *nativePackageUninstallRequest) { r.targetUserSID = "" },
		"relative helper": func(r *nativePackageUninstallRequest) { r.driverHelper = "ViiperUdeCtl.exe" },
		"wrong helper":    func(r *nativePackageUninstallRequest) { r.driverHelper = `C:\bundle\other.exe` },
		"bad hash":        func(r *nativePackageUninstallRequest) { r.expectedHelperSHA256 = strings.Repeat("z", 64) },
		"embedded NUL":    func(r *nativePackageUninstallRequest) { r.targetUserSID += "\x00evil" },
	}
	for name, mutate := range cases {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request := base
			mutate(&request)
			if err := request.validate(); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

func TestNativePackageLocalTestUninstallRequestIsAllOrNothing(t *testing.T) {
	t.Parallel()
	base := nativePackageUninstallRequest{
		driverHelper:                       `C:\bundle\ViiperUdeCtl.exe`,
		expectedHelperSHA256:               strings.Repeat("a", 64),
		targetUserSID:                      "S-1-5-21-1-2-3-1001",
		sourceRevision:                     strings.Repeat("b", 40),
		localTestCertificatePath:           `C:\bundle\ViiperUdeTest.cer`,
		expectedLocalTestCertificateSHA256: strings.Repeat("c", 64),
		expectedLocalTestPackageLockSHA256: strings.Repeat("d", 64),
	}
	if err := base.validate(); err != nil {
		t.Fatalf("valid local-test request: %v", err)
	}
	if !base.localTestTrustRequested() {
		t.Fatal("complete local-test identity was not detected")
	}
	production := base
	production.sourceRevision = ""
	production.localTestCertificatePath = ""
	production.expectedLocalTestCertificateSHA256 = ""
	production.expectedLocalTestPackageLockSHA256 = ""
	if err := production.validate(); err != nil {
		t.Fatalf("valid production request: %v", err)
	}
	if production.localTestTrustRequested() {
		t.Fatal("production request was treated as local-test")
	}
	cases := map[string]func(*nativePackageUninstallRequest){
		"missing revision":     func(r *nativePackageUninstallRequest) { r.sourceRevision = "" },
		"bad revision":         func(r *nativePackageUninstallRequest) { r.sourceRevision = "abc" },
		"missing certificate":  func(r *nativePackageUninstallRequest) { r.localTestCertificatePath = "" },
		"relative certificate": func(r *nativePackageUninstallRequest) { r.localTestCertificatePath = "ViiperUdeTest.cer" },
		"wrong certificate":    func(r *nativePackageUninstallRequest) { r.localTestCertificatePath = `C:\bundle\other.cer` },
		"missing certificate hash": func(r *nativePackageUninstallRequest) {
			r.expectedLocalTestCertificateSHA256 = ""
		},
		"bad package lock hash": func(r *nativePackageUninstallRequest) {
			r.expectedLocalTestPackageLockSHA256 = strings.Repeat("z", 64)
		},
		"certificate NUL": func(r *nativePackageUninstallRequest) {
			r.localTestCertificatePath += "\x00evil"
		},
	}
	for name, mutate := range cases {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request := base
			mutate(&request)
			if err := request.validate(); err == nil {
				t.Fatal("incomplete or malformed local-test identity accepted")
			}
		})
	}
}

func TestNativePackageUninstallNilTransaction(t *testing.T) {
	t.Parallel()
	err := runNativePackageUninstallTransaction(context.Background(), nativePackageTestLogger(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("error=%v", err)
	}
}

func Example_parseNativePackageRemoveProof() {
	result, err := parseNativePackageRemoveProof(
		"result=success operation=remove changed=0 rebootRequired=0 rollback=not-needed exitCode=0", 0,
	)
	fmt.Println(result.rebootRequired, err)
	// Output: false <nil>
}
