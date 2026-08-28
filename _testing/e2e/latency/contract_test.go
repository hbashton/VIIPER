package latency

import (
	"math"
	"testing"
)

func TestBoundaryProfilesDoNotInventPhysicalTimestamp(t *testing.T) {
	apiStages := RequiredStages(BoundaryAPIToConsumer)
	if len(apiStages) != 4 || apiStages[0] != StageClientPublished {
		t.Fatalf("API boundary stages = %v", apiStages)
	}
	for _, stage := range apiStages {
		if stage == StagePhysicalInputRead {
			t.Fatal("API-to-consumer profile contains a physical-input timestamp")
		}
	}
	physicalStages := RequiredStages(BoundaryPhysicalToConsumer)
	if len(physicalStages) != 5 || physicalStages[0] != StagePhysicalInputRead {
		t.Fatalf("physical boundary stages = %v", physicalStages)
	}
}

func TestValidateCaptureEnforcesBoundaryAndLifecycle(t *testing.T) {
	clock := testClock()
	api := evidenceFor(BoundaryAPIToConsumer, PathHealthy, TransitionPress, clock, 1_000_000, 1, "")
	total, spans, err := ValidateCapture(PathHealthy, BoundaryAPIToConsumer,
		TransitionPress, clock, testPlan().Workload, "", api)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1_000_000 || len(spans) != 3 {
		t.Fatalf("total=%d spans=%v", total, spans)
	}
	stale := api
	stale.Observation.ObservedEventTicks = stale.Observation.PrePublishFenceTicks
	if _, _, err := ValidateCapture(PathHealthy, BoundaryAPIToConsumer,
		TransitionPress, clock, testPlan().Workload, "", stale); err == nil {
		t.Fatal("stale consumer event was accepted")
	}
	if _, _, err := ValidateCapture(PathHealthy, BoundaryPhysicalToConsumer,
		TransitionPress, clock, testPlan().Workload, "", api); err == nil {
		t.Fatal("API evidence was accepted as physical-to-consumer evidence")
	}
	if _, _, err := ValidateCapture(PathRecovery, BoundaryAPIToConsumer,
		TransitionPress, clock, testPlan().Workload, "scripted-reconnect", api); err == nil {
		t.Fatal("healthy API evidence was accepted as recovery evidence")
	}

	recovery := evidenceFor(BoundaryRecoveryToConsumer, PathRecovery, TransitionPress,
		clock, 10_000_000, 2, "scripted-reconnect")
	if _, _, err := ValidateCapture(PathRecovery, BoundaryRecoveryToConsumer,
		TransitionPress, clock, testPlan().Workload, "scripted-reconnect", recovery); err != nil {
		t.Fatal(err)
	}
	recovery.Lifecycle.GenerationAfter = recovery.Lifecycle.GenerationBefore
	if _, _, err := ValidateCapture(PathRecovery, BoundaryRecoveryToConsumer,
		TransitionPress, clock, testPlan().Workload, "scripted-reconnect", recovery); err == nil {
		t.Fatal("recovery without a generation advance was accepted")
	}
}

func TestValidateCaptureRejectsUnknownPathAndTransition(t *testing.T) {
	clock := testClock()
	evidence := evidenceFor(BoundaryAPIToConsumer, PathHealthy, TransitionPress,
		clock, 1_000_000, 1, "")
	if _, _, err := ValidateCapture(Path("other"), BoundaryAPIToConsumer,
		TransitionPress, clock, testPlan().Workload, "", evidence); err == nil {
		t.Fatal("unknown path was accepted")
	}
	evidence.Observation.Transition = Transition("other")
	if _, _, err := ValidateCapture(PathHealthy, BoundaryAPIToConsumer,
		Transition("other"), clock, testPlan().Workload, "", evidence); err == nil {
		t.Fatal("unknown transition was accepted when the observation matched it")
	}
}

func TestValidateCaptureRejectsClockRegressionAndOverflow(t *testing.T) {
	clock := testClock()
	evidence := evidenceFor(BoundaryAPIToConsumer, PathHealthy, TransitionPress,
		clock, 1_000_000, 1, "")
	evidence.Timeline.Timestamps[2].Ticks = evidence.Timeline.Timestamps[1].Ticks
	if _, _, err := ValidateCapture(PathHealthy, BoundaryAPIToConsumer,
		TransitionPress, clock, testPlan().Workload, "", evidence); err == nil {
		t.Fatal("non-monotonic stage clock was accepted")
	}
	if _, err := counterIntervalNS(1, math.MaxInt64, 1); err == nil {
		t.Fatal("overflowing counter conversion was accepted")
	}
}
