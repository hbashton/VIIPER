//go:build windows

package udecx

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestIOCTLCodesMatchPackedHeader(t *testing.T) {
	wants := map[string]struct{ got, want uint32 }{
		"negotiate": {ioctlNegotiate, 0x22e400},
		"create":    {ioctlCreateDevice, 0x22e404},
		"destroy":   {ioctlDestroyDevice, 0x22e408},
		"dequeue":   {ioctlDequeueOperation, 0x22e40e},
		"complete":  {ioctlCompleteOperation, 0x22e411},
		"stats":     {ioctlQueryStats, 0x226414},
	}
	for name, pair := range wants {
		if pair.got != pair.want {
			t.Errorf("%s IOCTL=%#x want=%#x", name, pair.got, pair.want)
		}
	}
}

func TestParseMultiSZ(t *testing.T) {
	raw := []uint16{'a', 'b', 0, 'c', 0, 0, 'x'}
	got := parseMultiSZ(raw)
	if len(got) != 2 || got[0] != "ab" || got[1] != "c" {
		t.Fatalf("parseMultiSZ=%q", got)
	}
}

func TestCompletionPortRoutesExactOverlappedRequest(t *testing.T) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{completionPort: port, pumpDone: make(chan struct{})}
	go client.runCompletionPort()

	request := &ioRequest{done: make(chan ioCompletion, 1)}
	if err := windows.PostQueuedCompletionStatus(port, 547, 0, &request.overlapped); err != nil {
		t.Fatal(err)
	}
	select {
	case completion := <-request.done:
		if completion.err != nil || completion.transferred != 547 {
			t.Fatalf("completion=%+v want 547 successful bytes", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("completion pump did not route the exact OVERLAPPED request")
	}

	if err := windows.PostQueuedCompletionStatus(port, 0, completionPortCloseKey, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.pumpDone:
	case <-time.After(time.Second):
		t.Fatal("completion pump did not stop on its sentinel")
	}
	if err := windows.CloseHandle(port); err != nil {
		t.Fatal(err)
	}
}
