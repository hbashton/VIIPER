# VIIPER tray startup recovery validation — 2026-09-08

## Scope and confidence

Source-level repair for [DS4Windows #69](https://github.com/hbashton/DS4Windows/issues/69),
reported on VIIPER 0.1.0 and again on 0.1.2. No Task Scheduler or registry
configuration was changed, no existing process was restarted, and no actual
notification icons, test windows, controller connections, or input transports
were opened by these tests.

The reporter's exact Explorer timing and task security/session configuration
remain unknown. Retained portable-lab VIIPER logs contained no matching
`systray error:` entries. The current VIIPER PID 30276 was only inspected
read-only. This is a demonstrated matching mechanism, not a claim of live
reporter acceptance or that every possible blank-icon cause is eliminated.

## Root mechanism

The pinned `fyne.io/systray` v1.12.1 Windows implementation creates the HWND,
then calls `Shell_NotifyIcon(NIM_ADD)` with an icon record containing only
`NIF_MESSAGE`. If this fails before Explorer's notification area is ready,
`registerSystray` returns without creating the menu, setting `initialized`, or
invoking `onReady`. A later `TaskbarCreated` can re-add the incomplete record;
the icon remains blank and menu operations still reject the uninitialized tray.

Relevant upstream source:

- [Pinned Windows backend](https://github.com/fyne-io/systray/blob/3266b44d2302c29b14a67b7f476fe0c1b05cb5b6/systray_windows.go):
  `initInstance`, `registerSystray`, `wndProc`, `isReady`.
- [Pinned callback registration](https://github.com/fyne-io/systray/blob/3266b44d2302c29b14a67b7f476fe0c1b05cb5b6/systray.go):
  `Register` starts `onReady` on its own goroutine after native readiness.
- The same affected sequence remains in the inspected upstream
  `f60f01be81c6` Windows source. VIIPER's earlier `c1b0bae` OS-thread-lock
  correction is retained and is not presented as a new fix.

## Repair

The Windows-used dependency files are pinned locally under
`internal/tray/systray`, with full byte-identical upstream Apache license and
explicit modification/provenance notices. Original copied Go files total
1,472 lines / 38,649 bytes; the repair is limited to registration
lifetime and the corresponding new helper/tests, plus explicit return/error
handling and trivial maintenance needed to lint the pinned source locally.
Non-Windows VIIPER continues
to use its existing no-op tray implementation.

Window/menu creation and the one-time readiness callback no longer depend on
Explorer accepting `NIM_ADD`. Pending icon/tooltip updates are retained. Failed
ADD first tries MODIFY for the same HWND/ID, so a queued duplicate
`TaskbarCreated` cannot lose an already-added icon. If both fail, the existing
window thread retries through one 500 ms HWND timer, capped at 60 attempts.
Success, exhaustion, and shutdown cancel that timer. A later genuine taskbar
notification can start another bounded recovery cycle.

Shutdown posts `WM_CLOSE`; the window owner cancels recovery, removes the icon,
and publishes exit. No polling goroutine, window/menu recreation loop, second
tray API, registry edit, or runtime dependency upgrade was introduced.

## Before/after and tests

The regression `TestExplorerUnavailableDoesNotSkipMenuOrReady` was run against
the extracted original ordering before the correction. It failed:

```text
shell failure poisoned local tray initialization:
err=Explorer not ready window=1 menu=0 ready=0 add=1
```

The same test passes with the corrected ordering. The final tray packages have
12 top-level tests including the existing version test, with two additional
named native-resource-failure subtests. The tests use mocked native registration
and timer callbacks. The production `winTray` registration methods and actual
window-procedure timer/taskbar dispatch are exercised without creating a HWND.

Verified using the existing local Go 1.27.0 Windows amd64 toolchain:

```text
go test ./internal/tray/... -count=1                 PASS
go test ./internal/tray/... -count=50                PASS
go test -race ./internal/tray/... -count=10          PASS
go vet ./internal/tray/...                          PASS
golangci-lint run ./internal/tray/...                0 issues
go test ./cmd/viiper ./internal/cmd \
  -run '^Test.*(Configuration|ReadVersion|ServerKey|ExplicitKey|Startup)' -count=1
                                                     PASS
GOOS=linux CGO_ENABLED=0 go build ./internal/tray     PASS
GOOS=linux go list -f '{{.GoFiles}}' ./internal/tray  [tray.go]
git diff --check                                   PASS
```

The first race-enabled link attempt could not find the existing GCC toolchain's
`ld`; adding that toolchain's bin directory to the test shell's process-local
PATH corrected the environment. The recorded successful race run used
`CGO_ENABLED=1` and that existing GCC. No software was installed.

The copied upstream Win32 naming generated ST1003 style warnings under the
repository's stricter lint policy. Only that naming rule is newly excluded for
the two pinned upstream-copy filenames. No errcheck, ineffassign, vet, or
security checks were disabled by this patch; the existing test lint policy is
unchanged. Deprecated file writing, redundant casts, unused procedure state,
and explicit native return/error handling were cleaned locally; the tiny
existing version test was formatted with gofmt.

Covered cases include failed initial registration followed by complete icon
recovery; one-time local initialization; duplicate ADD/MODIFY handling; finite
retry exhaustion and later taskbar recovery; unrelated and stale timer events;
same-thread callback execution; shutdown cancellation; timer-arm failure;
and native window/menu initialization failures that must not publish readiness.

Remaining acceptance: a portable interactive-session scheduled-login test on
the affected machine, including menu opening and Quit. This was deliberately
not performed against the user's active broker/controller session.
