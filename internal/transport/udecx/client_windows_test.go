//go:build windows

package udecx

import "testing"

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
