//go:build windows && cgo && viiper_latency

package usbipprobe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/Alia5/VIIPER/_testing/e2e/sdl"
	"github.com/Alia5/VIIPER/internal/testsupport/inputlatency"
)

// SDLConsumer binds the probe to one already-opened SDL gamepad. The live
// wrapper remains responsible for proving that gamepad is the exact virtual
// device created by its isolated USB/IP session before constructing this type.
// It must also give the probe exclusive ownership of SDL's event queue on the
// thread which initialized SDL: SDL's poll/wait APIs consume unrelated events
// while searching for the selected gamepad's south-button edge.
type SDLConsumer struct {
	gamepad       *sdl.Gamepad
	clockIdentity string
}

// SDLClockIdentity returns the only identity SDLConsumer will report for
// SDL_GetTicksNS in this process. The caller cannot relabel the event clock;
// the exact loaded SDL artifact remains separate Workload provenance.
func SDLClockIdentity() string {
	return fmt.Sprintf("sdl3-ticks-ns/process=%d", os.Getpid())
}

func NewSDLConsumer(gamepad *sdl.Gamepad) (*SDLConsumer, error) {
	if gamepad == nil {
		return nil, errors.New("SDL consumer requires an opened gamepad")
	}
	return &SDLConsumer{
		gamepad: gamepad, clockIdentity: SDLClockIdentity(),
	}, nil
}

func (consumer *SDLConsumer) ClockIdentity() string {
	if consumer == nil {
		return ""
	}
	return consumer.clockIdentity
}

func (consumer *SDLConsumer) Current() (bool, error) {
	if consumer == nil || consumer.gamepad == nil {
		return false, errors.New("SDL consumer is not initialized")
	}
	return consumer.gamepad.GetButton(sdl.GamepadButtonSouth), nil
}

func (consumer *SDLConsumer) Fence() (uint64, error) {
	if consumer == nil || consumer.gamepad == nil {
		return 0, errors.New("SDL consumer is not initialized")
	}
	ticks := sdl.TicksNS()
	if ticks == 0 {
		return 0, errors.New("SDL_GetTicksNS returned zero")
	}
	return ticks, nil
}

func (consumer *SDLConsumer) Poll() (ConsumerEvent, bool, error) {
	if consumer == nil || consumer.gamepad == nil {
		return ConsumerEvent{}, false, errors.New("SDL consumer is not initialized")
	}
	event, present, err := consumer.gamepad.PollButtonTransition(
		sdl.GamepadButtonSouth)
	if err != nil || !present {
		return ConsumerEvent{}, present, err
	}
	observedTicks, err := inputlatency.Counter()
	if err != nil {
		return ConsumerEvent{}, false, err
	}
	return ConsumerEvent{
		Down: event.Down, EventTicks: event.TimestampNS,
		ObservedTicks: observedTicks,
	}, true, nil
}

func (consumer *SDLConsumer) Wait(ctx context.Context,
	timeout time.Duration) (ConsumerEvent, bool, error) {
	if consumer == nil || consumer.gamepad == nil {
		return ConsumerEvent{}, false, errors.New("SDL consumer is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return ConsumerEvent{}, false, err
	}
	timeoutMS := durationMillisecondsCeiling(timeout)
	event, present, err := consumer.gamepad.WaitButtonTransition(
		sdl.GamepadButtonSouth, timeoutMS)
	if err != nil || !present {
		if err == nil {
			err = ctx.Err()
		}
		return ConsumerEvent{}, present, err
	}
	observedTicks, err := inputlatency.Counter()
	if err != nil {
		return ConsumerEvent{}, false, err
	}
	return ConsumerEvent{
		Down: event.Down, EventTicks: event.TimestampNS,
		ObservedTicks: observedTicks,
	}, true, nil
}

func durationMillisecondsCeiling(duration time.Duration) int32 {
	if duration <= 0 {
		return 1
	}
	milliseconds := (duration + time.Millisecond - 1) / time.Millisecond
	if milliseconds > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(milliseconds)
}
