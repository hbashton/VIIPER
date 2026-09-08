//go:build windows

package systray

import (
	"errors"
	"reflect"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func TestExplorerUnavailableDoesNotSkipMenuOrReady(t *testing.T) {
	windowCount, menuCount, readyCount, addCount := 0, 0, 0, 0
	err := initializeTray(
		func() error { windowCount++; return nil },
		func() error { menuCount++; return nil },
		func() { readyCount++ },
		func() error { addCount++; return errors.New("Explorer not ready") },
	)
	if err != nil || windowCount != 1 || menuCount != 1 || readyCount != 1 || addCount != 1 {
		t.Fatalf("shell failure poisoned local tray initialization: err=%v window=%d menu=%d ready=%d add=%d",
			err, windowCount, menuCount, readyCount, addCount)
	}
}

func TestNativeResourcesAreReadyBeforeAnyShellAttempt(t *testing.T) {
	var events []string
	err := initializeTray(
		func() error { events = append(events, "window"); return nil },
		func() error { events = append(events, "menu"); return nil },
		func() { events = append(events, "ready") },
		func() error { events = append(events, "shell"); return nil },
	)
	if err != nil || !reflect.DeepEqual(events, []string{"window", "menu", "ready", "shell"}) {
		t.Fatalf("initialization order = %v, error=%v", events, err)
	}
}

func TestLateExplorerRecoversFullIconWithoutRecreatingWindowMenuOrCallbacks(t *testing.T) {
	windowCount, menuCount, readyCount, addCount := 0, 0, 0, 0
	armed, killed := 0, 0
	tray := &winTray{nid: &notifyIconData{Flags: 1}}
	tray.registration = &shellRegistration{
		add: func() error {
			addCount++
			if addCount == 1 {
				return errors.New("Explorer not ready")
			}
			if tray.nid.Icon != 42 || tray.nid.Flags != 7 || !tray.initialized.Load() {
				t.Fatalf("recovered a blank/uninitialized icon: %+v", tray.nid)
			}
			return nil
		},
		armTimer:  func() error { armed++; return nil },
		killTimer: func() { killed++ },
	}
	err := initializeTray(
		func() error { windowCount++; return nil },
		func() error { menuCount++; return nil },
		func() { readyCount++; tray.initialized.Store(true) },
		func() error { return tray.registerShellIcon(true) },
	)
	if err != nil || !tray.registration.timerActive || tray.registration.registered {
		t.Fatalf("expected pending registration, got err=%v, state=%+v", err, tray.registration)
	}
	// The real asynchronous onReady callback can supply icon/tooltip after
	// the first NIM_ADD. Recovery must use that retained, newest NID state.
	tray.nid.Icon = 42
	tray.nid.Flags = 7
	if err := tray.registerShellIcon(false); err != nil {
		t.Fatal(err)
	}
	if !tray.registration.registered || tray.registration.timerActive ||
		armed != 1 || killed != 1 || addCount != 2 ||
		windowCount != 1 || menuCount != 1 || readyCount != 1 {
		t.Fatalf("wrong recovery lifetime: window=%d menu=%d ready=%d add=%d arm=%d kill=%d state=%+v",
			windowCount, menuCount, readyCount, addCount, armed, killed, tray.registration)
	}
	// A queued timer event after KillTimer cannot add another duplicate icon.
	_ = tray.registerShellIcon(false)
	if addCount != 2 {
		t.Fatal("stale timer created a duplicate icon")
	}
}

func TestShellRetriesAreBoundedAndTaskbarEventCanStartFreshRecovery(t *testing.T) {
	attempts, armed, killed := 0, 0, 0
	available := false
	r := &shellRegistration{
		add: func() error {
			attempts++
			if !available {
				return errors.New("shell unavailable")
			}
			return nil
		},
		armTimer:  func() error { armed++; return nil },
		killTimer: func() { killed++ },
	}
	_ = r.begin()
	for i := 0; i < maxShellRegistrationAttempts+10; i++ {
		_ = r.retry()
	}
	if attempts != maxShellRegistrationAttempts || armed != 1 || killed != 1 || r.timerActive {
		t.Fatalf("retry budget escaped: add=%d arm=%d kill=%d state=%+v", attempts, armed, killed, r)
	}
	available = true
	if err := r.begin(); err != nil || !r.registered || r.timerActive {
		t.Fatalf("later TaskbarCreated could not recover: err=%v state=%+v", err, r)
	}
	if attempts != maxShellRegistrationAttempts+1 || armed != 1 {
		t.Fatal("successful later event unexpectedly created a timer")
	}
}

func TestExplorerRestartRetainsSingleLocalInitialization(t *testing.T) {
	adds := 0
	r := &shellRegistration{
		add:       func() error { adds++; return nil },
		armTimer:  func() error { t.Fatal("successful registration armed a timer"); return nil },
		killTimer: func() { t.Fatal("no timer was armed") },
	}
	if err := r.begin(); err != nil {
		t.Fatal(err)
	}
	if err := r.begin(); err != nil {
		t.Fatal(err)
	}
	if adds != 2 || !r.registered {
		t.Fatalf("restart state = %+v, adds=%d", r, adds)
	}
}

func TestQueuedTaskbarEventUpdatesAnAlreadyAddedIconInsteadOfRetryingDuplicates(t *testing.T) {
	adds, modifies := 0, 0
	r := &shellRegistration{
		add: func() error {
			adds++
			if adds > 1 {
				return errors.New("icon already exists")
			}
			return nil
		},
		modify:    func() error { modifies++; return nil },
		armTimer:  func() error { t.Fatal("existing icon must not trigger retries"); return nil },
		killTimer: func() { t.Fatal("no timer was armed") },
	}
	if err := r.begin(); err != nil {
		t.Fatal(err)
	}
	if err := r.begin(); err != nil {
		t.Fatal(err)
	}
	if !r.registered || r.timerActive || adds != 2 || modifies != 1 {
		t.Fatalf("queued event lost existing registration: adds=%d modifies=%d state=%+v", adds, modifies, r)
	}
}

func TestMissingIconRetriesWhenBothAddAndModifyFail(t *testing.T) {
	adds, modifies, arms := 0, 0, 0
	r := &shellRegistration{
		add:       func() error { adds++; return errors.New("no shell") },
		modify:    func() error { modifies++; return errors.New("no existing icon") },
		armTimer:  func() error { arms++; return nil },
		killTimer: func() {},
	}
	if err := r.begin(); err == nil {
		t.Fatal("both failed calls claimed success")
	}
	if err := r.retry(); err == nil {
		t.Fatal("failed retry claimed success")
	}
	if adds != 2 || modifies != 2 || arms != 1 || r.registered || !r.timerActive {
		t.Fatalf("missing icon was not retried: %+v", r)
	}
}

func TestWindowProcedureDispatchesOnlyItsOwnRecoveryTimer(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	owner := windows.GetCurrentThreadId()
	adds, arms, kills := 0, 0, 0
	tray := &winTray{wmTaskbarCreated: 0xC123}
	tray.registration = &shellRegistration{
		add: func() error {
			if windows.GetCurrentThreadId() != owner {
				t.Fatal("shell call left HWND thread")
			}
			adds++
			if adds == 1 {
				return errors.New("not ready")
			}
			return nil
		},
		armTimer:  func() error { arms++; return nil },
		killTimer: func() { kills++ },
	}
	// Creation-time broadcasts cannot initialize a second partial window.
	tray.wndProc(0, tray.wmTaskbarCreated, 0, 0)
	if adds != 0 {
		t.Fatal("uninitialized window reached the shell")
	}
	tray.initialized.Store(true)
	tray.wndProc(0, tray.wmTaskbarCreated, 0, 0)
	tray.wndProc(0, 0x0113, shellRetryTimerID+1, 0)
	if adds != 1 {
		t.Fatal("unrelated timer attempted tray recovery")
	}
	tray.wndProc(0, 0x0113, shellRetryTimerID, 0)
	tray.wndProc(0, 0x0113, shellRetryTimerID, 0)
	if adds != 2 || arms != 1 || kills != 1 || !tray.registration.registered {
		t.Fatalf("wrong window dispatch: add=%d arm=%d kill=%d state=%+v", adds, arms, kills, tray.registration)
	}
}

func TestShutdownCancelsTimerAndRejectsLateEventsOnTheSameOwnerThread(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	owner := windows.GetCurrentThreadId()
	checkOwner := func() {
		if windows.GetCurrentThreadId() != owner {
			t.Fatal("native callback left its owner thread")
		}
	}
	adds, armed, killed := 0, 0, 0
	tray := &winTray{}
	tray.initialized.Store(true)
	tray.registration = &shellRegistration{
		add:       func() error { checkOwner(); adds++; return errors.New("not ready") },
		armTimer:  func() error { checkOwner(); armed++; return nil },
		killTimer: func() { checkOwner(); killed++ },
	}
	_ = tray.registerShellIcon(true)
	tray.stopShellRegistration()
	tray.stopShellRegistration()
	_ = tray.registerShellIcon(false)
	_ = tray.registerShellIcon(true)
	if adds != 1 || armed != 1 || killed != 1 || tray.initialized.Load() ||
		!tray.registration.stopped || tray.registration.timerActive {
		t.Fatalf("shutdown leaked recovery: adds=%d armed=%d killed=%d state=%+v", adds, armed, killed, tray.registration)
	}
}

func TestTimerFailureDoesNotClaimItIsArmedOrBusyLoop(t *testing.T) {
	adds := 0
	timerError := errors.New("SetTimer failed")
	r := &shellRegistration{
		add:       func() error { adds++; return errors.New("not ready") },
		armTimer:  func() error { return timerError },
		killTimer: func() { t.Fatal("failed timer must not be cancelled") },
	}
	if err := r.begin(); !errors.Is(err, timerError) {
		t.Fatalf("got %v", err)
	}
	for i := 0; i < 10; i++ {
		_ = r.retry()
	}
	if adds != 1 || r.timerActive || r.registered {
		t.Fatalf("invalid timer failure state: %+v", r)
	}
}

func TestNativeResourceFailuresDoNotPublishReadyOrRegisterWithShell(t *testing.T) {
	for _, failMenu := range []bool{false, true} {
		t.Run(map[bool]string{false: "window", true: "menu"}[failMenu], func(t *testing.T) {
			failure := errors.New("native resource failed")
			err := initializeTray(
				func() error {
					if !failMenu {
						return failure
					}
					return nil
				},
				func() error { return failure },
				func() { t.Fatal("resource failure claimed ready") },
				func() error { t.Fatal("resource failure reached shell"); return nil },
			)
			if !errors.Is(err, failure) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
