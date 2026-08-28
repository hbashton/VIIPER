//go:build !windows || !viiper_latency

package dualsense

import (
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
)

// These helpers compile away in every ordinary production build. The live
// evidence hook exists only in an explicit Windows viiper_latency build.
func inputLatencyCounter() int64 { return 0 }

func traceInputBrokerReceived(*DualSense, uint32, time.Time, int64) {}

func traceInputTransportAdmitted(
	*DualSense, inputpresentation.Claim, int64,
) {
}
