//go:build windows

package udecx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

var pipeHarnessSequence atomic.Uint64

type controlledDeadline struct {
	context.Context
	done chan struct{}
}

func newControlledDeadline() *controlledDeadline {
	return &controlledDeadline{Context: context.Background(), done: make(chan struct{})}
}

func (c *controlledDeadline) Done() <-chan struct{} { return c.done }

func (c *controlledDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *controlledDeadline) expire() { close(c.done) }

type ioctlResult struct {
	written uint32
	err     error
}

type pipeIOCPHarness struct {
	t       *testing.T
	client  *Client
	name    string
	pending chan *ioRequest
}

// Named-pipe connection requests are genuine cancellable Windows overlapped
// operations. Substituting only the syscall issuer exercises the production
// request pool, IOCP pump, timeout, cancellation, and close state machine
// without requiring an installed UDE driver on hosted CI.
func newPipeIOCPHarness(t *testing.T, name string) *pipeIOCPHarness {
	t.Helper()
	if name == "" {
		name = fmt.Sprintf(`\\.\pipe\viiper-udecx-iocp-%d-%d`, os.Getpid(), pipeHarnessSequence.Add(1))
	}
	namePointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateNamedPipe(
		namePointer,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 4096, 4096, 0, nil)
	if err != nil {
		t.Fatalf("create IOCP harness pipe: %v", err)
	}
	port, err := windows.CreateIoCompletionPort(handle, 0, 0, 1)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("associate IOCP harness pipe: %v", err)
	}

	pending := make(chan *ioRequest, 1)
	client := &Client{
		handle:         handle,
		completionPort: port,
		pumpDone:       make(chan struct{}),
		overlappedIssuer: func(handle windows.Handle, overlapped *windows.Overlapped) (uint32, error) {
			return 0, windows.ConnectNamedPipe(handle, overlapped)
		},
		pendingObserver: func(request *ioRequest) {
			pending <- request
		},
	}
	client.requestPool.New = func() any {
		return &ioRequest{done: make(chan ioCompletion, 1)}
	}
	go client.runCompletionPort(port)

	harness := &pipeIOCPHarness{t: t, client: client, name: name, pending: pending}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close IOCP harness: %v", err)
		}
	})
	return harness
}

func (h *pipeIOCPHarness) listen(ctx context.Context) <-chan ioctlResult {
	h.t.Helper()
	result := make(chan ioctlResult, 1)
	go func() {
		written, err := h.client.ioctl(ctx, 0, nil, nil)
		result <- ioctlResult{written: written, err: err}
	}()
	return result
}

func (h *pipeIOCPHarness) waitPending(result <-chan ioctlResult) *ioRequest {
	h.t.Helper()
	select {
	case request := <-h.pending:
		return request
	case completed := <-result:
		h.t.Fatalf("overlapped request completed before becoming pending: (%d, %v)", completed.written, completed.err)
		return nil
	case <-time.After(5 * time.Second):
		h.t.Fatal("overlapped request did not become pending")
		return nil
	}
}

func (h *pipeIOCPHarness) waitResult(result <-chan ioctlResult) ioctlResult {
	h.t.Helper()
	select {
	case completed := <-result:
		return completed
	case <-time.After(5 * time.Second):
		h.t.Fatal("overlapped request did not finish")
		return ioctlResult{}
	}
}

func (h *pipeIOCPHarness) connect() windows.Handle {
	h.t.Helper()
	namePointer, err := windows.UTF16PtrFromString(h.name)
	if err != nil {
		h.t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		namePointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		h.t.Fatalf("connect IOCP harness pipe: %v", err)
	}
	return handle
}

func TestWindowsClientIOCPStress(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previousProcs) })

	t.Run("deadline cancellation drains packets before request reuse", func(t *testing.T) {
		harness := newPipeIOCPHarness(t, "")
		var priorRequest *ioRequest
		reused := false
		for iteration := 0; iteration < 32; iteration++ {
			deadline := newControlledDeadline()
			result := harness.listen(deadline)
			request := harness.waitPending(result)
			if request == priorRequest {
				reused = true
			}
			deadline.expire()
			completed := harness.waitResult(result)
			if completed.written != 0 || !errors.Is(completed.err, context.DeadlineExceeded) {
				t.Fatalf("iteration %d cancellation = (%d, %v), want deadline exceeded", iteration, completed.written, completed.err)
			}
			select {
			case stale := <-request.done:
				t.Fatalf("iteration %d left stale completion %+v", iteration, stale)
			default:
			}
			priorRequest = request
		}
		if !reused {
			t.Fatal("stress loop did not reuse an OVERLAPPED request")
		}

		result := harness.listen(context.Background())
		harness.waitPending(result)
		peer := harness.connect()
		completed := harness.waitResult(result)
		if completed.err != nil || completed.written != 0 {
			t.Fatalf("completion after cancellation stress = (%d, %v), want success", completed.written, completed.err)
		}
		if err := windows.CloseHandle(peer); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("close drains pending IO and serializes callers", func(t *testing.T) {
		harness := newPipeIOCPHarness(t, "")
		result := harness.listen(context.Background())
		harness.waitPending(result)

		const callers = 32
		start := make(chan struct{})
		closeResults := make(chan error, callers)
		var ready sync.WaitGroup
		ready.Add(callers)
		for range callers {
			go func() {
				ready.Done()
				<-start
				closeResults <- harness.client.Close()
			}()
		}
		ready.Wait()
		close(start)
		for range callers {
			if err := <-closeResults; err != nil {
				t.Fatalf("concurrent Close: %v", err)
			}
		}
		completed := harness.waitResult(result)
		if !errors.Is(completed.err, windows.ERROR_OPERATION_ABORTED) {
			t.Fatalf("pending IO after Close = %v, want ERROR_OPERATION_ABORTED", completed.err)
		}
		select {
		case <-harness.client.pumpDone:
		default:
			t.Fatal("Close returned before the IOCP pump stopped")
		}
		if _, err := harness.client.ioctl(context.Background(), 0, nil, nil); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			t.Fatalf("IO after Close error=%v want ERROR_INVALID_HANDLE", err)
		}
	})

	t.Run("pump failure drains completion and closes admission", func(t *testing.T) {
		buffered := &ioRequest{done: make(chan ioCompletion, 1)}
		buffered.done <- ioCompletion{transferred: 547}
		if completed := completionAfterPumpStop(windows.InvalidHandle, buffered); completed.err != nil || completed.transferred != 547 {
			t.Fatalf("buffered completion after pump stop = %+v, want successful 547 bytes", completed)
		}
		select {
		case stale := <-buffered.done:
			t.Fatalf("pump-stop drain left stale completion %+v", stale)
		default:
		}

		harness := newPipeIOCPHarness(t, "")
		releaseIssue := make(chan struct{})
		harness.client.pendingObserver = func(request *ioRequest) {
			harness.pending <- request
			<-releaseIssue
		}
		result := harness.listen(context.Background())
		harness.waitPending(result)
		peer := harness.connect()
		if err := windows.PostQueuedCompletionStatus(harness.client.completionPort, 0, 0, nil); err != nil {
			t.Fatal(err)
		}
		select {
		case <-harness.client.pumpDone:
		case <-time.After(5 * time.Second):
			t.Fatal("forced IOCP pump stop did not complete")
		}
		close(releaseIssue)
		completed := harness.waitResult(result)
		if completed.err != nil || completed.written != 0 {
			t.Fatalf("kernel completion racing pump stop = (%d, %v), want success", completed.written, completed.err)
		}
		if err := windows.CloseHandle(peer); err != nil {
			t.Fatal(err)
		}

		second := harness.listen(context.Background())
		completed = harness.waitResult(second)
		if completed.err == nil || !strings.Contains(completed.err.Error(), "completion pump stopped") {
			t.Fatalf("new IO after forced pump stop error=%v", completed.err)
		}
		select {
		case request := <-harness.pending:
			t.Fatalf("pump failure admitted a new kernel request %p", request)
		default:
		}
	})

	t.Run("reconnect isolates old completion ports", func(t *testing.T) {
		name := fmt.Sprintf(`\\.\pipe\viiper-udecx-reconnect-%d-%d`, os.Getpid(), pipeHarnessSequence.Add(1))
		for iteration := 0; iteration < 16; iteration++ {
			oldClient := newPipeIOCPHarness(t, name)
			oldResult := oldClient.listen(context.Background())
			oldClient.waitPending(oldResult)
			if err := oldClient.client.Close(); err != nil {
				t.Fatalf("iteration %d close old connection: %v", iteration, err)
			}
			if completed := oldClient.waitResult(oldResult); !errors.Is(completed.err, windows.ERROR_OPERATION_ABORTED) {
				t.Fatalf("iteration %d old connection result=%v", iteration, completed.err)
			}

			newClient := newPipeIOCPHarness(t, name)
			newResult := newClient.listen(context.Background())
			newClient.waitPending(newResult)
			peer := newClient.connect()
			completed := newClient.waitResult(newResult)
			if completed.err != nil || completed.written != 0 {
				t.Fatalf("iteration %d new connection = (%d, %v), want success", iteration, completed.written, completed.err)
			}
			if err := windows.CloseHandle(peer); err != nil {
				t.Fatal(err)
			}
			if err := newClient.client.Close(); err != nil {
				t.Fatalf("iteration %d close new connection: %v", iteration, err)
			}
		}
	})
}
