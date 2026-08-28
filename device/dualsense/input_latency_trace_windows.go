//go:build windows && viiper_latency

package dualsense

import (
	"reflect"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/internal/testsupport/inputlatency"
)

func inputLatencyCounter() int64 {
	ticks, _ := inputlatency.Counter()
	return ticks
}

func inputLatencySource(d *DualSense) uintptr {
	if d == nil {
		return 0
	}
	return reflect.ValueOf(d).Pointer()
}

func traceInputBrokerReceived(d *DualSense, sequence uint32,
	receivedAt time.Time, ticks int64) {
	inputlatency.RecordBrokerReceived(
		inputLatencySource(d), sequence, receivedAt, ticks)
}

func traceInputTransportAdmitted(d *DualSense,
	claim inputpresentation.Claim, ticks int64) {
	inputlatency.RecordTransportAdmitted(
		inputLatencySource(d), claim.ReceivedAt,
		claim.Token, claim.Generation, ticks)
}
