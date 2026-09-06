package xboxone

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
)

func TestTimedFeedbackSlowAckPastExpiryStillPublishesNeutral(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		executor := newTimedFeedbackTestExecutor(t, timedFeedbackPublisherFunc(func(w ControllerPersonaCanonicalFeedbackWireV1, d time.Time) error {
			err := publisher.PublishControllerFeedback(w, d)
			time.Sleep(20 * time.Millisecond)
			return err
		}))
		command := canonicalFeedbackMotor(5, 1)
		command.DirectMotor.Duration = 1
		if err := executor.Execute(command, timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		frames := publisher.snapshotFrames()
		if len(frames) != 2 || frames[0].Command != controllerfeedback.CommandApply ||
			frames[1].Command != controllerfeedback.CommandNeutral || executor.active {
			t.Fatalf("late ACK omitted explicit neutral: %+v active=%t", frames, executor.active)
		}
	})
}

func TestTimedFeedbackDrainDoesNotHideInFlightUncertainty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		block := make(chan struct{})
		calls, failures := 0, 0
		executor := newTimedFeedbackTestExecutor(t, timedFeedbackPublisherFunc(func(w ControllerPersonaCanonicalFeedbackWireV1, d time.Time) error {
			calls++
			if calls == 2 {
				<-block
				return ErrProductionBrokerFeedbackAmbiguous
			}
			return publisher.PublishControllerFeedback(w, d)
		}))
		executor.onFailure = func(err error) {
			if !errors.Is(err, ErrProductionBrokerFeedbackAmbiguous) {
				t.Errorf("lost uncertainty: %v", err)
			}
			failures++
		}
		if err := executor.Execute(canonicalFeedbackMotor(5, 1), timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		done := make(chan error, 1)
		go func() { done <- executor.CancelAndDrain(timedFeedbackDeadline()) }()
		synctest.Wait()
		close(block)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if failures != 1 || executor.lastError == nil || executor.active {
			t.Fatalf("drain hid in-flight ambiguity: failures=%d error=%v active=%t", failures, executor.lastError, executor.active)
		}
	})
}

func TestTimedFeedbackFencedInitialPublicationIsNotDelivered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		executor := newTimedFeedbackTestExecutor(t, publisher)
		if err := executor.acquire(timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		defer executor.release()
		executor.mu.Lock()
		executor.reset = true
		executor.mu.Unlock()
		if err := executor.publishProgram(executor.epoch, timedFeedbackDeadline()); !errors.Is(err, ErrCanonicalFeedbackExecutorState) {
			t.Fatalf("unpublished fenced host action reported delivered: %v", err)
		}
		if len(publisher.snapshotFrames()) != 0 {
			t.Fatal("fenced publication reached consumer")
		}
	})
}

type timedFeedbackPublisherFunc func(ControllerPersonaCanonicalFeedbackWireV1, time.Time) error

func (publish timedFeedbackPublisherFunc) PublishControllerFeedback(wire ControllerPersonaCanonicalFeedbackWireV1, deadline time.Time) error {
	return publish(wire, deadline)
}

func newTimedFeedbackTestExecutor(t *testing.T, publisher ControllerPersonaCanonicalFeedbackPublisher) *controllerPersonaTimedFeedbackExecutor {
	t.Helper()
	core := newCanonicalFeedbackTestExecutor(t, publisher)
	// Inside synctest this follows the controllable clock, with no wall sleeps
	// or OS/QPC calls. Production uses the existing cross-process QPC domain.
	core.clock = func() (uint64, bool) { return uint64(time.Now().UnixMicro()), true }
	executor := newControllerPersonaTimedFeedbackExecutor(core)
	t.Cleanup(func() {
		if err := executor.CancelAndDrain(time.Now().Add(time.Second)); err != nil {
			t.Errorf("cleanup drain: %v", err)
		}
	})
	return executor
}

func timedFeedbackDeadline() time.Time { return time.Now().Add(time.Second) }

func TestTimedFeedbackCapturedWindowsRepeatNeutralAndPulse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		executor := newTimedFeedbackTestExecutor(t, publisher)
		packets := [][]byte{
			{0x09, 0x00, 0x01, 0x09, 0x00, 0x0f, 0, 0, 0, 0, 0xff, 0, 0xeb},
			{0x09, 0x00, 0x05, 0x09, 0x00, 0x0f, 0, 0, 0x0f, 0x0f, 0xff, 0, 0xeb},
		}
		for i, packet := range packets {
			body, err := DecodeRumbleBody(packet[SinglePacketHeaderSize:])
			if err != nil {
				t.Fatal(err)
			}
			execution := canonicalFeedbackMotor(5, uint64(i+1))
			execution.DirectMotor = body
			// The pure one-frame codec correctly cannot interpret a program.
			if _, err := ControllerPersonaCanonicalFeedbackFrame(canonicalFeedbackTestBinding(), execution, 1,
				ControllerPersonaCanonicalFeedbackStateUpdate); !errors.Is(err, ErrUnsupportedCanonicalFeedbackTiming) {
				t.Fatalf("pure encoder must retain timing boundary: %v", err)
			}
			start := time.Now()
			if err := executor.Execute(execution, timedFeedbackDeadline()); err != nil {
				t.Fatalf("Windows packet %d: %v", i, err)
			}
			if time.Now() != start {
				t.Fatal("host execution waited for the repeat program")
			}
			if executor.active != (i == 1) {
				t.Fatalf("packet %d active=%t", i, executor.active)
			}
		}
		frames := publisher.snapshotFrames()
		if len(frames) != 2 || frames[0].Command != controllerfeedback.CommandNeutral ||
			frames[1].Command != controllerfeedback.CommandApply ||
			frames[1].BodyLow != 9830 || frames[1].BodyHigh != 9830 ||
			frames[1].LeftTrigger != 0 || frames[1].RightTrigger != 0 {
			t.Fatalf("captured packet mapping: %+v", frames)
		}
		if executor.endsAt-executor.startedAt != 601_800_000 {
			t.Fatalf("repeat program length = %d us", executor.endsAt-executor.startedAt)
		}
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		if len(publisher.snapshotFrames()) != 5 {
			t.Fatal("sustained Windows program was not renewed")
		}
	})
}

func TestTimedFeedbackFiniteRepeatPreservesFourChannelsAndAbsoluteExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		executor := newTimedFeedbackTestExecutor(t, publisher)
		command := canonicalFeedbackMotor(5, 1)
		command.DirectMotor.Duration, command.DirectMotor.Repeat = 20, 2
		start := uint64(time.Now().UnixMicro())
		if err := executor.Execute(command, timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(600 * time.Millisecond)
		synctest.Wait()
		frames := publisher.snapshotFrames()
		if len(frames) != 7 || frames[6].Command != controllerfeedback.CommandNeutral || executor.active {
			t.Fatalf("finite program did not stop at 600 ms: %+v active=%t", frames, executor.active)
		}
		for i, frame := range frames {
			if frame.Sequence != uint64(i+1) {
				t.Fatalf("nonmonotonic renewal sequence: %+v", frames)
			}
			if frame.Command == controllerfeedback.CommandApply {
				if frame.BodyLow != 0x4000 || frame.BodyHigh != 0x8000 ||
					frame.LeftTrigger != 0xbfff || frame.RightTrigger != 0xffff {
					t.Fatalf("lost or swapped actuator: %+v", frame)
				}
				if frame.TimestampMicroseconds+frame.TimeToLiveMicroseconds > start+600_000 {
					t.Fatalf("lease outlives program: %+v", frame)
				}
			}
		}
		time.Sleep(time.Second)
		if len(publisher.snapshotFrames()) != len(frames) {
			t.Fatal("expired effect restarted")
		}
	})
}

func TestTimedFeedbackReplacementRejectsOldTimersAndForeignWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		executor := newTimedFeedbackTestExecutor(t, publisher)
		first := canonicalFeedbackMotor(5, 1)
		first.DirectMotor.Repeat = 5
		if err := executor.Execute(first, timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		oldEpoch := executor.epoch
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		foreign := canonicalFeedbackMotor(6, 2)
		if err := executor.Execute(foreign, timedFeedbackDeadline()); !errors.Is(err, ErrInvalidCanonicalFeedbackBinding) {
			t.Fatalf("foreign command: %v", err)
		}
		if executor.epoch != oldEpoch || !executor.active {
			t.Fatal("foreign work canceled valid program")
		}
		second := canonicalFeedbackMotor(5, 2) // wire sequence is already 3
		second.DirectMotor.Enabled = MotorLeftImpulse
		second.DirectMotor.Duration = 1
		if err := executor.Execute(second, timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		count := len(publisher.snapshotFrames())
		executor.renew(oldEpoch) // a callback already queued at replacement
		if len(publisher.snapshotFrames()) != count {
			t.Fatal("stale callback published through successor")
		}
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		frames := publisher.snapshotFrames()
		if frames[count-1].Sequence != 4 || frames[count-1].BodyLow != 0 ||
			frames[count-1].LeftTrigger == 0 || frames[count].Command != controllerfeedback.CommandNeutral {
			t.Fatalf("replacement sequence/channel/stop mismatch: %+v", frames)
		}
	})
}

func TestTimedFeedbackCancellationAndMaskedNeutralIgnoreTiming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		executor := newTimedFeedbackTestExecutor(t, publisher)
		for i, body := range []RumbleBodyV1{
			{Enabled: MotorAll, LeftImpulse: 100, Duration: 0, Delay: 255, Repeat: 255},
			{Enabled: 0, LeftImpulse: 100, Duration: 255, Delay: 255, Repeat: 255},
			{Enabled: MotorAll, Duration: 255, Delay: 255, Repeat: 255},
		} {
			command := canonicalFeedbackMotor(5, uint64(i+1))
			command.DirectMotor = body
			if err := executor.Execute(command, timedFeedbackDeadline()); err != nil {
				t.Fatal(err)
			}
			if executor.active || executor.timer != nil {
				t.Fatal("neutral/cancel scheduled an effect")
			}
		}
		for _, frame := range publisher.snapshotFrames() {
			if frame.Command != controllerfeedback.CommandNeutral {
				t.Fatalf("neutral incorrectly retires ownership or applies: %+v", frame)
			}
		}
	})
}

func TestTimedFeedbackResetAndTerminalDrainFenceRenewals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		executor := newTimedFeedbackTestExecutor(t, publisher)
		command := canonicalFeedbackMotor(5, 1)
		command.DirectMotor.Repeat = 255
		if err := executor.Execute(command, timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		oldEpoch := executor.epoch
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		if err := executor.ResetAndDrain(timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		count := len(publisher.snapshotFrames())
		executor.renew(oldEpoch)
		if len(publisher.snapshotFrames()) != count {
			t.Fatal("reset did not fence callback")
		}
		if err := executor.ResetNeutral(canonicalFeedbackClear(6, 2), timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		if err := executor.Execute(canonicalFeedbackMotor(6, 3), timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		newEpoch := executor.epoch
		if err := executor.CancelAndDrain(timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		if err := executor.DisconnectNeutral(canonicalFeedbackClear(7, 4), timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		frames := publisher.snapshotFrames()
		stop := frames[len(frames)-1]
		if stop.Command != controllerfeedback.CommandStop || stop.TimeToLiveMicroseconds != 250_000 ||
			stop.Sequence <= frames[len(frames)-2].Sequence {
			t.Fatalf("terminal Stop: %+v", stop)
		}
		executor.renew(newEpoch)
		time.Sleep(time.Second)
		if len(publisher.snapshotFrames()) != len(frames) {
			t.Fatal("post-stop Apply")
		}
		if err := executor.ResetAndDrain(timedFeedbackDeadline()); !errors.Is(err, ErrCanonicalFeedbackExecutorState) {
			t.Fatalf("terminal fence reopened: %v", err)
		}
	})
}

func TestTimedFeedbackDrainWaitsForInFlightRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		block := make(chan struct{})
		calls := 0
		executor := newTimedFeedbackTestExecutor(t, timedFeedbackPublisherFunc(func(w ControllerPersonaCanonicalFeedbackWireV1, d time.Time) error {
			calls++
			if calls == 2 {
				<-block
			}
			return publisher.PublishControllerFeedback(w, d)
		}))
		command := canonicalFeedbackMotor(5, 1)
		command.DirectMotor.Repeat = 2
		if err := executor.Execute(command, timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		done := make(chan error, 1)
		go func() { done <- executor.CancelAndDrain(timedFeedbackDeadline()) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("drain returned before renewal finished: %v", err)
		default:
		}
		close(block)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if executor.active || !executor.stopped {
			t.Fatal("drain fence lost")
		}
		if err := executor.DisconnectNeutral(canonicalFeedbackClear(6, 2), timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if calls != 3 {
			t.Fatalf("late publication after drain: %d", calls)
		}
	})
}

func TestTimedFeedbackRenewalFailureAndPanicAreContained(t *testing.T) {
	for _, panicAfterAcceptance := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejection", true: "panic-after-acceptance"}[panicAfterAcceptance], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				publisher := &recordingCanonicalFeedbackPublisher{}
				calls, failures := 0, 0
				executor := newTimedFeedbackTestExecutor(t, timedFeedbackPublisherFunc(func(w ControllerPersonaCanonicalFeedbackWireV1, d time.Time) error {
					calls++
					if calls == 2 && !panicAfterAcceptance {
						return errCanonicalFeedbackTestRejected
					}
					err := publisher.PublishControllerFeedback(w, d)
					if calls == 2 {
						panic("accepted then failed")
					}
					return err
				}))
				executor.onFailure = func(error) { failures++ }
				command := canonicalFeedbackMotor(5, 1)
				command.DirectMotor.Repeat = 255
				if err := executor.Execute(command, timedFeedbackDeadline()); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if calls != 2 || failures != 1 || executor.active || executor.lastError == nil {
					t.Fatalf("failed renewal continued/silent: calls=%d failures=%d active=%t error=%v", calls, failures, executor.active, executor.lastError)
				}
				snapshot, _ := executor.core.Snapshot()
				if snapshot.ExecutionInFlight {
					t.Fatal("panic wedged core inFlight")
				}
				if panicAfterAcceptance && !errors.Is(executor.lastError, ErrProductionBrokerFeedbackAmbiguous) {
					t.Fatal("panic misrepresented as proven non-delivery")
				}
				if err := executor.CancelAndDrain(timedFeedbackDeadline()); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestTimedFeedbackLateAcknowledgementCannotArmFailedProgram(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		calls := 0
		executor := newTimedFeedbackTestExecutor(t, timedFeedbackPublisherFunc(func(w ControllerPersonaCanonicalFeedbackWireV1, d time.Time) error {
			calls++
			err := publisher.PublishControllerFeedback(w, d)
			time.Sleep(20 * time.Millisecond)
			return err
		}))
		command := canonicalFeedbackMotor(5, 1)
		command.DirectMotor.Repeat = 255
		err := executor.Execute(command, time.Now().Add(10*time.Millisecond))
		if !errors.Is(err, ErrProductionBrokerFeedbackAmbiguous) || executor.active {
			t.Fatalf("late ACK armed program: %v active=%t", err, executor.active)
		}
		time.Sleep(time.Second)
		if calls != 1 {
			t.Fatal("failed acceptance caused late effects")
		}
	})
}

func TestCanonicalFeedbackAbsoluteExpiryIsSampledAtPublication(t *testing.T) {
	publisher := &recordingCanonicalFeedbackPublisher{}
	core := newCanonicalFeedbackTestExecutor(t, publisher)
	core.clock = func() (uint64, bool) { return 1_009_000, true }
	if err := core.executeUntil(canonicalFeedbackMotor(5, 1), timedFeedbackDeadline(), 1_010_000); err != nil {
		t.Fatal(err)
	}
	core.clock = func() (uint64, bool) { return 1_010_000, true }
	if err := core.executeUntil(canonicalFeedbackMotor(5, 2), timedFeedbackDeadline(), 1_010_000); err != nil {
		t.Fatal(err)
	}
	frames := publisher.snapshotFrames()
	if frames[0].TimeToLiveMicroseconds != 1_000 || frames[1].Command != controllerfeedback.CommandNeutral {
		t.Fatalf("publication extended expired effect: %+v", frames)
	}
}

func TestTimedFeedbackInvalidDeadlineAndDelayPreserveProgram(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		executor := newTimedFeedbackTestExecutor(t, publisher)
		command := canonicalFeedbackMotor(5, 1)
		if err := executor.Execute(command, timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		epoch := executor.epoch
		command.DirectMotor.Delay = 1
		if err := executor.Execute(command, timedFeedbackDeadline()); !errors.Is(err, ErrUnsupportedCanonicalFeedbackTiming) {
			t.Fatalf("guessed Delay: %v", err)
		}
		if err := executor.ResetAndDrain(time.Time{}); !errors.Is(err, ErrCanonicalFeedbackExecutorDeadline) {
			t.Fatal(err)
		}
		if err := executor.CancelAndDrain(time.Time{}); !errors.Is(err, ErrCanonicalFeedbackExecutorDeadline) {
			t.Fatal(err)
		}
		if !executor.active || executor.epoch != epoch || executor.reset || executor.stopped {
			t.Fatal("invalid request changed active program")
		}
		var absent *controllerPersonaTimedFeedbackExecutor
		if err := absent.ResetAndDrain(timedFeedbackDeadline()); !errors.Is(err, ErrCanonicalFeedbackExecutorUninitialized) {
			t.Fatal(err)
		}
		if err := absent.CancelAndDrain(timedFeedbackDeadline()); !errors.Is(err, ErrCanonicalFeedbackExecutorUninitialized) {
			t.Fatal(err)
		}
	})
}

func TestTimedFeedbackOutputFreeGenerationAdvanceCancelsOldProgram(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{}
		executor := newTimedFeedbackTestExecutor(t, publisher)
		if err := executor.Execute(canonicalFeedbackMotor(5, 1), timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		if err := executor.Execute(ControllerPersonaLocalExecution{Action: ControllerPersonaPerformReset, Generation: 5, Order: 2}, timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if executor.active || len(publisher.snapshotFrames()) != 1 {
			t.Fatal("old generation renewed after reset")
		}
		if err := executor.Execute(canonicalFeedbackMotor(6, 3), timedFeedbackDeadline()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestProductionPreparationUsesTimedExecutorAndReportsRenewalFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		device, preparation := testProductionRetainedUSBDevice(t)
		if device.adapter.local != preparation.timedExecutor {
			t.Fatal("production bypasses timing layer")
		}
		lease, ok := device.acquireBrokerStream()
		if !ok {
			t.Fatal("broker not available")
		}
		defer lease.release()
		if err := lease.consumerReady(); err != nil {
			t.Fatal(err)
		}
		preparation.timedExecutor.failProgram(0, errCanonicalFeedbackTestRejected) // no active program: no effect
		if !device.brokerConsumerReady {
			t.Fatal("inactive callback retired broker")
		}
		preparation.timedExecutor.active = true
		preparation.timedExecutor.failProgram(0, errCanonicalFeedbackTestRejected)
		if device.adapter.fatalLocalError == nil || device.brokerConsumerReady || device.brokerStreamActive || preparation.bridge.ready {
			t.Fatal("async failure did not reach retained owner and broker")
		}
		lease.release()
		if _, reopened := device.acquireBrokerStream(); reopened {
			t.Fatal("fatally failed persona admitted a replacement broker stream")
		}
		if err := lease.consumerReady(); !errors.Is(err, ErrProductionBrokerUnavailable) {
			t.Fatalf("failed consumer became ready again: %v", err)
		}
		if device.ProductionBrokerConsumerReady() || device.TryBeginProductionBrokerActivation() {
			t.Fatal("failed persona exposed ready/activation authority")
		}
	})
}
