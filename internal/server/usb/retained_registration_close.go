package usb

import (
	"context"
	"errors"
	"time"

	"github.com/Alia5/VIIPER/virtualbus"
)

// Guarded by serverLifecycleMu. This is a cold exact-Add admission fence, not
// a second retained owner or a replacement for session.close's neutral proof.
type retainedRegistrationClosure struct {
	key          retainedDeviceAdmissionKey
	registration virtualbus.DeviceMeta
	closing      bool
	done         chan struct{}
	removed      bool
	closeErr     error
	terminalErr  error
}

// A rejected duplicate reservation never owned or changed the device. Do not
// let it poison the actual owner's safe close. Accept only the sole, linearly
// wrapped busy sentinel: joining a real failure with busy must remain fatal.
func retainedRegistrationCloseFailure(err error) error {
	for current, depth := err, 0; current != nil && depth < 64; depth++ {
		if current == errRetainedImportBusy {
			return nil
		}
		wrapped, ok := current.(interface{ Unwrap() error })
		if !ok {
			break
		}
		current = wrapped.Unwrap()
	}
	return retainedServerTerminalFailure(err)
}

func retainedRegistrationCloseKey(registration virtualbus.DeviceMeta) (retainedDeviceAdmissionKey, bool) {
	reference, valid := exactRetainedImportOwnerReference(registration.Dev)
	if !valid || registration.Bus == nil || registration.Context == nil ||
		registration.RegistrationToken == 0 {
		return retainedDeviceAdmissionKey{}, false
	}
	return retainedDeviceAdmissionKey{device: reference, bus: registration.Bus,
		registrationToken: registration.RegistrationToken}, true
}

// Exact cancellation cleanup also covers a non-one-shot stream which ended
// before its registration was removed. Never drop a state still owned by a
// closer or an in-flight stream. Owner/admission quarantine tombstones remain
// in their existing server-owned registries; this map owns no such proof.
func (s *Server) forgetRetainedRegistrationClosure(registration virtualbus.DeviceMeta) {
	key, valid := retainedRegistrationCloseKey(registration)
	if s == nil || !valid {
		return
	}
	s.serverLifecycleMu.Lock()
	if state := s.retainedRegistrationClosures[key]; state != nil {
		s.forgetRetainedRegistrationClosureLocked(state)
	}
	s.serverLifecycleMu.Unlock()
}

func (s *Server) forgetRetainedRegistrationClosureLocked(state *retainedRegistrationClosure) {
	if state.registration.Context.Err() == nil {
		return
	}
	if state.closing {
		select {
		case <-state.done:
			// Existing waiters retain this completed result by pointer. The
			// canceled Add can no longer admit a new close or stream owner.
		default:
			return
		}
	}
	for _, stream := range s.retainedStreams {
		if stream.registrationClosure == state {
			return
		}
	}
	if s.retainedRegistrationClosures[state.key] == state {
		delete(s.retainedRegistrationClosures, state.key)
	}
}

// registerRetainedDeviceStream publishes a stream under the same lock as an
// exact management close. A selected-but-not-yet-registered import therefore
// cannot bind an owner after the closer has taken its stream snapshot.
func (s *Server) registerRetainedDeviceStream(cancel context.CancelFunc,
	registration virtualbus.DeviceMeta) (uint64, bool) {
	key, valid := retainedRegistrationCloseKey(registration)
	if s == nil || cancel == nil || !valid {
		return 0, false
	}
	s.busesMu.Lock()
	defer s.busesMu.Unlock()
	s.serverLifecycleMu.Lock()
	defer s.serverLifecycleMu.Unlock()
	if s.serverClosing || s.busses[registration.Meta.BusID] != registration.Bus ||
		!registration.Bus.AuthenticatesRegistration(registration) || registration.Context.Err() != nil {
		return 0, false
	}
	state := s.retainedRegistrationClosures[key]
	if state == nil {
		state = &retainedRegistrationClosure{key: key, registration: registration}
		s.retainedRegistrationClosures[key] = state
	}
	if state.closing || state.terminalErr != nil {
		return 0, false
	}
	return s.registerRetainedStreamLocked(cancel, state)
}

// CloseAndRemoveRetainedDeviceRegistrationIfPresent closes only this exact
// registration's USB streams and waits for the ordinary retained lifecycle,
// including acknowledged DisconnectNeutral, before allowing a management
// caller to close its feedback consumer. It never chooses by numeric address.
// It does not claim to detach or cancel auto-reattach in a Windows USB/IP client.
// A failure leaves the exact admission fenced and is never reported as success.
func (s *Server) CloseAndRemoveRetainedDeviceRegistrationIfPresent(
	registration virtualbus.DeviceMeta,
) (bool, error) {
	key, valid := retainedRegistrationCloseKey(registration)
	if s == nil || !valid || !registration.Bus.RecognizesRegistration(registration) {
		return false, errRetainedImportInvalid
	}
	deadline := s.retainedCloseDeadline()
	s.busesMu.Lock()
	s.serverLifecycleMu.Lock()
	state := s.retainedRegistrationClosures[key]
	if state != nil && state.closing {
		s.serverLifecycleMu.Unlock()
		s.busesMu.Unlock()
		return waitRetainedRegistrationClose(state, deadline)
	}
	if s.busses[registration.Meta.BusID] != registration.Bus ||
		!registration.Bus.AuthenticatesRegistration(registration) {
		s.serverLifecycleMu.Unlock()
		s.busesMu.Unlock()
		return false, nil
	}
	if state == nil {
		state = &retainedRegistrationClosure{key: key, registration: registration}
		s.retainedRegistrationClosures[key] = state
	}
	state.closing = true
	state.done = make(chan struct{})
	streams := make([]*retainedServerStream, 0, 1)
	for _, stream := range s.retainedStreams {
		if stream.registrationClosure == state {
			streams = append(streams, stream)
		}
	}
	s.serverLifecycleMu.Unlock()
	s.busesMu.Unlock()

	// Cancel stream ingress, not registration.Context or the broker socket.
	// The existing stream owner performs all drain/neutral/quarantine steps.
	for _, stream := range streams {
		stream.cancel()
	}
	var closeErr error
	for _, stream := range streams {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			closeErr = errRetainedImportCloseTimedOut
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case <-stream.done:
		case <-timer.C:
			closeErr = errRetainedImportCloseTimedOut
		}
		timer.Stop()
		if closeErr != nil {
			break
		}
	}
	s.serverLifecycleMu.Lock()
	closeErr = errors.Join(closeErr, state.terminalErr)
	s.serverLifecycleMu.Unlock()
	if closeErr == nil {
		// Safe one-shot close may already have removed this exact Add. The
		// present-at-entry capability still completed the requested retirement.
		_, closeErr = s.RemoveDeviceRegistrationIfPresent(registration)
	}
	s.serverLifecycleMu.Lock()
	state.removed, state.closeErr = closeErr == nil, closeErr
	close(state.done)
	if closeErr == nil {
		delete(s.retainedRegistrationClosures, key)
	} else {
		// Cancellation may have happened while this closer was still active;
		// its earlier callback correctly deferred cleanup until this boundary.
		s.forgetRetainedRegistrationClosureLocked(state)
	}
	s.serverLifecycleMu.Unlock()
	return closeErr == nil, closeErr
}

func waitRetainedRegistrationClose(state *retainedRegistrationClosure, deadline time.Time) (bool, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false, errRetainedImportCloseTimedOut
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-state.done:
		return state.removed, state.closeErr
	case <-timer.C:
		return false, errRetainedImportCloseTimedOut
	}
}
