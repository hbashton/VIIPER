package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

type fakeNativePackageTransaction struct {
	events              []string
	fail                string
	closeErr            error
	rollbackErr         error
	snapshot            nativePackageServiceSnapshot
	installStarted      chan struct{}
	rollbackHadDeadline bool
}

func (f *fakeNativePackageTransaction) event(name string) error {
	f.events = append(f.events, name)
	if f.fail == name {
		return errors.New(name + " failure")
	}
	return nil
}

func (f *fakeNativePackageTransaction) Preflight(context.Context) error {
	return f.event("preflight")
}

func (f *fakeNativePackageTransaction) InspectService(context.Context) (nativePackageServiceSnapshot, error) {
	return f.snapshot, f.event("inspect")
}

func (f *fakeNativePackageTransaction) Prepare(
	_ context.Context, snapshot nativePackageServiceSnapshot,
) error {
	if snapshot != f.snapshot {
		return errors.New("service snapshot changed")
	}
	return f.event("prepare")
}

func (f *fakeNativePackageTransaction) InstallDriverAndBroker(ctx context.Context) error {
	if err := f.event("install"); err != nil {
		return err
	}
	if f.installStarted != nil {
		close(f.installStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeNativePackageTransaction) VerifyAuthenticatedHealth(context.Context) error {
	return f.event("verify")
}

func (f *fakeNativePackageTransaction) Commit(context.Context) error {
	return f.event("commit")
}

func (f *fakeNativePackageTransaction) Rollback(ctx context.Context) error {
	f.events = append(f.events, "rollback")
	if ctx.Err() != nil {
		return errors.New("rollback inherited canceled context")
	}
	_, f.rollbackHadDeadline = ctx.Deadline()
	return f.rollbackErr
}

func (f *fakeNativePackageTransaction) Close() error {
	f.events = append(f.events, "close")
	return f.closeErr
}

func nativePackageTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNativePackageTransactionCommitsOnlyAfterAuthenticatedHealth(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageTransaction{snapshot: nativePackageServiceSnapshot{
		disposition: nativePackageServiceWeakExactOwned, wasRunning: true,
	}}
	if err := runNativePackageTransaction(context.Background(), nativePackageTestLogger(), fake); err != nil {
		t.Fatalf("run transaction: %v", err)
	}
	want := []string{"preflight", "inspect", "prepare", "install", "verify", "commit", "close"}
	if !reflect.DeepEqual(fake.events, want) {
		t.Fatalf("events=%v want=%v", fake.events, want)
	}
}

func TestNativePackageTransactionFailureMatrix(t *testing.T) {
	t.Parallel()
	for _, fail := range []string{"preflight", "inspect", "prepare", "install", "verify", "commit"} {
		fail := fail
		t.Run(fail, func(t *testing.T) {
			t.Parallel()
			fake := &fakeNativePackageTransaction{fail: fail}
			err := runNativePackageTransaction(context.Background(), nativePackageTestLogger(), fake)
			if err == nil || !strings.Contains(err.Error(), fail+" failure") {
				t.Fatalf("error=%v", err)
			}
			rollbackExpected := fail == "prepare" || fail == "install" || fail == "verify" || fail == "commit"
			rollbackSeen := false
			for _, event := range fake.events {
				rollbackSeen = rollbackSeen || event == "rollback"
			}
			if rollbackSeen != rollbackExpected {
				t.Fatalf("events=%v rollbackExpected=%v", fake.events, rollbackExpected)
			}
			if fake.events[len(fake.events)-1] != "close" {
				t.Fatalf("transaction did not close: %v", fake.events)
			}
		})
	}
}

func TestNativePackageTransactionRejectsCancellationBeforeMutation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeNativePackageTransaction{fail: "install"}
	cancel()
	err := runNativePackageTransaction(ctx, nativePackageTestLogger(), fake)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v events=%v", err, fake.events)
	}
	if !reflect.DeepEqual(fake.events, []string{"close"}) {
		t.Fatalf("events=%v", fake.events)
	}
}

func TestNativePackageTransactionCancellationReconcilesWithBoundedRollback(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeNativePackageTransaction{installStarted: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- runNativePackageTransaction(ctx, nativePackageTestLogger(), fake)
	}()
	<-fake.installStarted
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v events=%v", err, fake.events)
	}
	if !fake.rollbackHadDeadline {
		t.Fatal("rollback did not receive its own bounded deadline")
	}
	want := []string{"preflight", "inspect", "prepare", "install", "rollback", "close"}
	if !reflect.DeepEqual(fake.events, want) {
		t.Fatalf("events=%v want=%v", fake.events, want)
	}
}

func TestNativePackageBrokerCommitRejectsUnboundTokenBeforePlatformCall(t *testing.T) {
	t.Parallel()
	command := NativePackageBrokerCommit{
		TokenFile:           `C:\Program Files\VIIPER\.viiper.transaction.test.token`,
		ExpectedTokenSHA256: "not-a-hash",
		TargetUserSID:       "S-1-5-21-1-2-3-1001",
	}
	err := command.Run(nativePackageTestLogger())
	if err == nil || !strings.Contains(err.Error(), "64 hexadecimal") {
		t.Fatalf("error=%v", err)
	}
}

func TestNativePackageBrokerCommitRejectsInvalidDeadlineBeforePlatformCall(t *testing.T) {
	t.Parallel()
	command := NativePackageBrokerCommit{
		TokenFile:                 `C:\Program Files\VIIPER\.viiper.transaction.test.token`,
		ExpectedTokenSHA256:       strings.Repeat("a", 64),
		TargetUserSID:             "S-1-5-21-1-2-3-1001",
		TransactionDeadlineUnixMS: "not-a-deadline",
	}
	err := command.Run(nativePackageTestLogger())
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("error=%v", err)
	}
}

func TestNativePackageTransactionReportsRollbackAndCloseFailures(t *testing.T) {
	t.Parallel()
	fake := &fakeNativePackageTransaction{
		fail: "verify", rollbackErr: errors.New("rollback failed"), closeErr: errors.New("close failed"),
	}
	err := runNativePackageTransaction(context.Background(), nativePackageTestLogger(), fake)
	for _, fragment := range []string{"verify failure", "rollback failed", "close failed"} {
		if err == nil || !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error=%v missing %q", err, fragment)
		}
	}
}

func TestNativePackageRebootRequiredPreservesInstallerExitCode(t *testing.T) {
	t.Parallel()
	cause := errors.New("helper safely rolled back")
	err := fmt.Errorf("install native package: %w", &nativePackageRebootRequiredError{cause: cause})
	var exitCoder interface{ ExitCode() int }
	if !errors.As(err, &exitCoder) || exitCoder.ExitCode() != nativePackageRebootRequiredCode {
		t.Fatalf("error=%v exitCoder=%T", err, exitCoder)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("reboot-required error lost cause: %v", err)
	}
}

func TestNativePackageRequestFailsClosed(t *testing.T) {
	t.Parallel()
	base := nativePackageRequest{
		brokerSource: `C:\bundle\viiper.exe`, packageDirectory: `C:\bundle\driver`,
		submissionManifest: `C:\bundle\submission.json`, sourceRevision: strings.Repeat("a", 40),
		driverHelper: `C:\bundle\ViiperUdeCtl.exe`, expectedBrokerSHA256: strings.Repeat("b", 64),
		expectedHelperSHA256: strings.Repeat("c", 64), targetUserSID: "S-1-5-21-1-2-3-1001",
		expectedManifestSHA256: strings.Repeat("d", 64),
	}
	if err := base.validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	cases := map[string]func(*nativePackageRequest){
		"relative package": func(r *nativePackageRequest) { r.packageDirectory = `driver` },
		"short revision":   func(r *nativePackageRequest) { r.sourceRevision = "abc" },
		"bad broker hash":  func(r *nativePackageRequest) { r.expectedBrokerSHA256 = strings.Repeat("z", 64) },
		"embedded NUL":     func(r *nativePackageRequest) { r.submissionManifest += "\x00evil" },
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
