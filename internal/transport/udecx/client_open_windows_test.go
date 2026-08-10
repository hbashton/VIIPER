//go:build windows

package udecx

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const testNativeInterfacePath = `\\?\VIIPER#native#broker`

func TestNativeBrokerOpenRemainsExclusive(t *testing.T) {
	if nativeBrokerShareMode != 0 {
		t.Fatalf("native broker CreateFile share mode=%d, want exclusive mode 0", nativeBrokerShareMode)
	}
}

func TestNativeAcquisitionRediscoversUntilPriorOwnerCleanupCompletes(t *testing.T) {
	discoverCalls := 0
	openCalls := 0
	waitCalls := 0
	closeCalls := 0
	wantHandle := windows.Handle(0x1234)

	handle, err := acquireNativeController(context.Background(), nativeAcquisitionOps{
		discover: func(context.Context) ([]string, error) {
			discoverCalls++
			if discoverCalls == 1 {
				return nil, nil
			}
			return []string{testNativeInterfacePath}, nil
		},
		open: func(context.Context, string) (windows.Handle, error) {
			openCalls++
			if openCalls == 1 {
				return windows.InvalidHandle, windows.ERROR_SHARING_VIOLATION
			}
			return wantHandle, nil
		},
		close: func(windows.Handle) error {
			closeCalls++
			return nil
		},
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
	}, nativeAcquisitionPolicy{attempts: 4, interval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if handle != wantHandle {
		t.Fatalf("handle=%#x want=%#x", handle, wantHandle)
	}
	if discoverCalls != 3 || openCalls != 2 || waitCalls != 2 || closeCalls != 0 {
		t.Fatalf("calls discover=%d open=%d wait=%d close=%d, want 3/2/2/0",
			discoverCalls, openCalls, waitCalls, closeCalls)
	}
}

func TestNativeAcquisitionClassifiesBoundedInterfaceAbsence(t *testing.T) {
	discoverCalls := 0
	waitCalls := 0

	_, err := acquireNativeController(context.Background(), nativeAcquisitionOps{
		discover: func(context.Context) ([]string, error) {
			discoverCalls++
			return nil, nil
		},
		open: func(context.Context, string) (windows.Handle, error) {
			t.Fatal("open called without a discovered interface")
			return windows.InvalidHandle, nil
		},
		close: func(windows.Handle) error { return nil },
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
	}, nativeAcquisitionPolicy{attempts: 4, interval: time.Millisecond})

	var acquisitionErr *AcquisitionError
	if !errors.As(err, &acquisitionErr) {
		t.Fatalf("error=%v, want *AcquisitionError", err)
	}
	if acquisitionErr.Kind != AcquisitionInterfaceUnavailable || acquisitionErr.Attempts != 4 {
		t.Fatalf("acquisition error=%+v, want unavailable after 4 attempts", acquisitionErr)
	}
	if !acquisitionErr.Temporary() || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("error=%v, want temporary ERROR_FILE_NOT_FOUND", err)
	}
	if discoverCalls != 4 || waitCalls != 3 {
		t.Fatalf("calls discover=%d wait=%d, want 4/3", discoverCalls, waitCalls)
	}
}

func TestNativeAcquisitionClassifiesBoundedOwnerCleanup(t *testing.T) {
	openCalls := 0
	waitCalls := 0

	_, err := acquireNativeController(context.Background(), nativeAcquisitionOps{
		discover: func(context.Context) ([]string, error) {
			return []string{testNativeInterfacePath}, nil
		},
		open: func(context.Context, string) (windows.Handle, error) {
			openCalls++
			return windows.InvalidHandle, windows.ERROR_SHARING_VIOLATION
		},
		close: func(windows.Handle) error { return nil },
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
	}, nativeAcquisitionPolicy{attempts: 3, interval: time.Millisecond})

	var acquisitionErr *AcquisitionError
	if !errors.As(err, &acquisitionErr) {
		t.Fatalf("error=%v, want *AcquisitionError", err)
	}
	if acquisitionErr.Kind != AcquisitionOwnerCleanupInProgress || acquisitionErr.Attempts != 3 {
		t.Fatalf("acquisition error=%+v, want owner cleanup after 3 attempts", acquisitionErr)
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("error=%v, want ERROR_SHARING_VIOLATION", err)
	}
	if openCalls != 3 || waitCalls != 2 {
		t.Fatalf("calls open=%d wait=%d, want 3/2", openCalls, waitCalls)
	}
}

func TestNativeAcquisitionReturnsTerminalErrorsWithoutRetry(t *testing.T) {
	tests := []struct {
		name      string
		discover  func(context.Context) ([]string, error)
		open      func(context.Context, string) (windows.Handle, error)
		want      error
		wantOpens int
	}{
		{
			name: "discovery failure",
			discover: func(context.Context) ([]string, error) {
				return nil, windows.ERROR_INVALID_DATA
			},
			open: func(context.Context, string) (windows.Handle, error) {
				return windows.InvalidHandle, nil
			},
			want: windows.ERROR_INVALID_DATA,
		},
		{
			name: "access denied",
			discover: func(context.Context) ([]string, error) {
				return []string{testNativeInterfacePath}, nil
			},
			open: func(context.Context, string) (windows.Handle, error) {
				return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
			},
			want:      windows.ERROR_ACCESS_DENIED,
			wantOpens: 1,
		},
		{
			name: "path not found is not interface absence",
			discover: func(context.Context) ([]string, error) {
				return []string{testNativeInterfacePath}, nil
			},
			open: func(context.Context, string) (windows.Handle, error) {
				return windows.InvalidHandle, windows.ERROR_PATH_NOT_FOUND
			},
			want:      windows.ERROR_PATH_NOT_FOUND,
			wantOpens: 1,
		},
		{
			name: "device disconnected",
			discover: func(context.Context) ([]string, error) {
				return []string{testNativeInterfacePath}, nil
			},
			open: func(context.Context, string) (windows.Handle, error) {
				return windows.InvalidHandle, windows.ERROR_DEVICE_NOT_CONNECTED
			},
			want:      windows.ERROR_DEVICE_NOT_CONNECTED,
			wantOpens: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			openCalls := 0
			waitCalls := 0
			_, err := acquireNativeController(context.Background(), nativeAcquisitionOps{
				discover: test.discover,
				open: func(ctx context.Context, path string) (windows.Handle, error) {
					openCalls++
					return test.open(ctx, path)
				},
				close: func(windows.Handle) error { return nil },
				wait: func(context.Context, time.Duration) error {
					waitCalls++
					return nil
				},
			}, nativeAcquisitionPolicy{attempts: 5, interval: time.Millisecond})
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
			var acquisitionErr *AcquisitionError
			if errors.As(err, &acquisitionErr) {
				t.Fatalf("terminal error was classified transient: %+v", acquisitionErr)
			}
			if openCalls != test.wantOpens || waitCalls != 0 {
				t.Fatalf("calls open=%d wait=%d, want %d/0", openCalls, waitCalls, test.wantOpens)
			}
		})
	}
}

func TestNativeAcquisitionRejectsAmbiguousOwnershipWithoutRetry(t *testing.T) {
	waitCalls := 0
	openCalls := 0
	_, err := acquireNativeController(context.Background(), nativeAcquisitionOps{
		discover: func(context.Context) ([]string, error) {
			return []string{"first", "second"}, nil
		},
		open: func(context.Context, string) (windows.Handle, error) {
			openCalls++
			return windows.InvalidHandle, nil
		},
		close: func(windows.Handle) error { return nil },
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
	}, nativeAcquisitionPolicy{attempts: 5, interval: time.Millisecond})
	if err == nil || openCalls != 0 || waitCalls != 0 {
		t.Fatalf("error=%v open=%d wait=%d, want terminal ambiguity", err, openCalls, waitCalls)
	}
}

func TestNativeAcquisitionCancellationInterruptsRetryWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	waitCalls := 0
	_, err := acquireNativeController(ctx, nativeAcquisitionOps{
		discover: func(context.Context) ([]string, error) { return nil, nil },
		open: func(context.Context, string) (windows.Handle, error) {
			return windows.InvalidHandle, nil
		},
		close: func(windows.Handle) error { return nil },
		wait: func(ctx context.Context, _ time.Duration) error {
			waitCalls++
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	}, nativeAcquisitionPolicy{attempts: 20, interval: time.Hour})
	if !errors.Is(err, context.Canceled) || waitCalls != 1 {
		t.Fatalf("error=%v wait=%d, want prompt cancellation on first wait", err, waitCalls)
	}
}

func TestNativeAcquisitionProductionWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- waitForNativeAcquisition(ctx, time.Hour) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled native acquisition wait did not return promptly")
	}
}

func TestNativeAcquisitionCancellationAfterDiscoveryDoesNotOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	openCalls := 0
	_, err := acquireNativeController(ctx, nativeAcquisitionOps{
		discover: func(context.Context) ([]string, error) {
			cancel()
			return []string{testNativeInterfacePath}, nil
		},
		open: func(context.Context, string) (windows.Handle, error) {
			openCalls++
			return windows.Handle(0x1234), nil
		},
		close: func(windows.Handle) error { return nil },
		wait:  func(context.Context, time.Duration) error { return nil },
	}, nativeAcquisitionPolicy{attempts: 2, interval: time.Millisecond})
	if !errors.Is(err, context.Canceled) || openCalls != 0 {
		t.Fatalf("error=%v open=%d, want cancellation before open", err, openCalls)
	}
}

func TestNativeAcquisitionCancellationAfterOpenClosesHandle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wantHandle := windows.Handle(0x4321)
	closeCalls := 0
	_, err := acquireNativeController(ctx, nativeAcquisitionOps{
		discover: func(context.Context) ([]string, error) {
			return []string{testNativeInterfacePath}, nil
		},
		open: func(context.Context, string) (windows.Handle, error) {
			cancel()
			return wantHandle, nil
		},
		close: func(handle windows.Handle) error {
			closeCalls++
			if handle != wantHandle {
				t.Fatalf("closed handle=%#x want=%#x", handle, wantHandle)
			}
			return nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
	}, nativeAcquisitionPolicy{attempts: 2, interval: time.Millisecond})
	if !errors.Is(err, context.Canceled) || closeCalls != 1 {
		t.Fatalf("error=%v closes=%d, want canceled and one close", err, closeCalls)
	}
}

func TestNativeAcquisitionClosesUnexpectedHandleReturnedWithError(t *testing.T) {
	badHandle := windows.Handle(0x1001)
	wantHandle := windows.Handle(0x1002)
	openCalls := 0
	closed := make([]windows.Handle, 0, 1)

	handle, err := acquireNativeController(context.Background(), nativeAcquisitionOps{
		discover: func(context.Context) ([]string, error) {
			return []string{testNativeInterfacePath}, nil
		},
		open: func(context.Context, string) (windows.Handle, error) {
			openCalls++
			if openCalls == 1 {
				return badHandle, windows.ERROR_SHARING_VIOLATION
			}
			return wantHandle, nil
		},
		close: func(handle windows.Handle) error {
			closed = append(closed, handle)
			return nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
	}, nativeAcquisitionPolicy{attempts: 2, interval: time.Millisecond})
	if err != nil || handle != wantHandle {
		t.Fatalf("handle=%#x error=%v, want %#x", handle, err, wantHandle)
	}
	if len(closed) != 1 || closed[0] != badHandle {
		t.Fatalf("closed=%v, want [%#x]", closed, badHandle)
	}
}

func TestNativeAcquisitionTreatsHandleCleanupFailureAsTerminal(t *testing.T) {
	closeErr := errors.New("close failed")
	waitCalls := 0
	_, err := acquireNativeController(context.Background(), nativeAcquisitionOps{
		discover: func(context.Context) ([]string, error) {
			return []string{testNativeInterfacePath}, nil
		},
		open: func(context.Context, string) (windows.Handle, error) {
			return windows.Handle(0x1001), windows.ERROR_SHARING_VIOLATION
		},
		close: func(windows.Handle) error { return closeErr },
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
	}, nativeAcquisitionPolicy{attempts: 3, interval: time.Millisecond})
	if !errors.Is(err, closeErr) || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("error=%v, want joined open and handle-cleanup failures", err)
	}
	var acquisitionErr *AcquisitionError
	if errors.As(err, &acquisitionErr) || waitCalls != 0 {
		t.Fatalf("error=%v wait=%d, cleanup failure must be terminal", err, waitCalls)
	}
}

func TestNativeAcquisitionRetryClassificationIsExact(t *testing.T) {
	tests := []struct {
		err      error
		wantKind AcquisitionErrorKind
	}{
		{windows.ERROR_FILE_NOT_FOUND, AcquisitionInterfaceUnavailable},
		{windows.ERROR_SHARING_VIOLATION, AcquisitionOwnerCleanupInProgress},
		{windows.ERROR_PATH_NOT_FOUND, 0},
		{windows.ERROR_DEVICE_NOT_CONNECTED, 0},
		{windows.ERROR_BUSY, 0},
		{windows.ERROR_ACCESS_DENIED, 0},
	}
	for _, test := range tests {
		got := classifyNativeAcquisitionError(test.err, 7)
		if test.wantKind == 0 {
			if got != nil {
				t.Errorf("error %v classified as %+v, want terminal", test.err, got)
			}
			continue
		}
		if got == nil || got.Kind != test.wantKind || got.Attempts != 7 {
			t.Errorf("error %v classified as %+v, want kind=%d attempts=7", test.err, got, test.wantKind)
		}
	}
}
