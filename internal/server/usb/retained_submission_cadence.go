package usb

import (
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	rootusb "github.com/Alia5/VIIPER/usb"
)

// configureEndpointCadence snapshots the already-validated descriptor and the
// bounded experimental IN service policy before activation. The policy never
// changes the advertised descriptor, OUT cadence, or EP0. USB/IP URB arrival is
// not itself a USB service clock: the host may refill a pipeline immediately
// after every RET_SUBMIT. Without this fence
// an unchanged input image can be presented thousands of times per millisecond.
func (scheduler *retainedSubmissionScheduler) configureEndpointCadence(
	descriptor *rootusb.Descriptor,
	retainedInputServiceMS int,
) error {
	if scheduler == nil || validateRetainedImportDescriptor(descriptor, scheduler.limits) != nil {
		return errRetainedSubmissionInvalidLimits
	}
	inputServiceInterval, err := retainedInputServiceInterval(retainedInputServiceMS)
	if err != nil {
		return err
	}
	var intervals [3]time.Duration
	for _, endpoint := range descriptor.Interfaces[0].Endpoints {
		lane := retainedusb.LaneInterruptOut
		if endpoint.BEndpointAddress == scheduler.limits.InterruptInRoute.EndpointAddress {
			lane = retainedusb.LaneInterruptIn
		}
		index, _ := lane.Index()
		intervals[index] = usbServiceInterval(descriptor.Device.Speed, endpoint.BInterval)
		if lane == retainedusb.LaneInterruptIn && inputServiceInterval != 0 {
			intervals[index] = inputServiceInterval
		}
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.started || scheduler.closed || scheduler.importActivated ||
		scheduler.ctx.Err() != nil || scheduler.endpointCadenceConfigured || !scheduler.emptyLocked() {
		return errRetainedSubmissionInvalidRequest
	}
	scheduler.endpointIntervals = intervals
	scheduler.endpointCadenceConfigured = true
	return nil
}

// Only a terminal preparation consumes a service opportunity. A Pending owner
// remains eligible on its readiness edge, without another polling layer. The
// first request is immediate; small lateness preserves phase and a full missed
// interval re-anchors instead of bursting through expired opportunities. Socket
// completion does not advance the service clock.
func (scheduler *retainedSubmissionScheduler) advanceEndpointCadenceLocked(index int, now time.Time) {
	interval := scheduler.endpointIntervals[index]
	if interval <= 0 {
		return
	}
	next := scheduler.endpointNextService[index]
	if next.IsZero() || now.Sub(next) >= interval {
		next = now
	}
	scheduler.endpointNextService[index] = next.Add(interval)
}

func (scheduler *retainedSubmissionScheduler) resetEndpointCadenceLocked(lifecycle controlLifecycleSetup) {
	for _, lane := range [...]retainedusb.Lane{retainedusb.LaneInterruptIn, retainedusb.LaneInterruptOut} {
		index, _ := lane.Index()
		route := scheduler.limits.InterruptInRoute
		if lane == retainedusb.LaneInterruptOut {
			route = scheduler.limits.InterruptOutRoute
		}
		if lifecycle.kind == controlLifecycleSetConfiguration ||
			(lifecycle.kind == controlLifecycleSetInterface && lifecycle.interfaceNumber == route.InterfaceNumber) ||
			(lifecycle.kind == controlLifecycleClearEndpointHalt && lifecycle.endpointAddress == route.EndpointAddress) {
			scheduler.endpointNextService[index] = time.Time{}
		}
	}
}
