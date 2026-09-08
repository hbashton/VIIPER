//go:build windows

package systray

const (
	shellRetryTimerID            = 0x56494950
	shellRetryMilliseconds       = 500
	maxShellRegistrationAttempts = 60
)

// initializeTray keeps native resource ownership separate from Explorer's
// availability. All callbacks are invoked on the window's locked OS thread.
func initializeTray(createInstance, createMenu func() error, ready func(), registerIcon func() error) error {
	if err := createInstance(); err != nil {
		return err
	}
	if err := createMenu(); err != nil {
		return err
	}
	ready()
	// A missing Explorer notification area is not a failed HWND/menu. The
	// registration owner retains the complete icon state and schedules retries.
	_ = registerIcon()
	return nil
}

// shellRegistration is serialized by the window thread and winTray.muNID.
// No timer goroutine is allowed to call Shell_NotifyIcon or recreate resources.
type shellRegistration struct {
	add         func() error
	modify      func() error
	armTimer    func() error
	killTimer   func()
	registered  bool
	timerActive bool
	stopped     bool
	attempts    int
}

func (r *shellRegistration) begin() error {
	if r.stopped {
		return nil
	}
	r.registered = false
	r.attempts = 0
	return r.tryAdd()
}

func (r *shellRegistration) retry() error {
	if r.stopped || r.registered || !r.timerActive {
		return nil
	}
	return r.tryAdd()
}

func (r *shellRegistration) tryAdd() error {
	r.attempts++
	err := r.add()
	// TaskbarCreated can already be queued when the first NIM_ADD succeeds.
	// A duplicate ADD is not a lost icon: update that same HWND/ID instead.
	if err != nil && r.modify != nil {
		if r.modify() == nil {
			err = nil
		}
	}
	if err != nil {
		if r.attempts >= maxShellRegistrationAttempts {
			r.cancelTimer()
		} else if !r.timerActive {
			if timerErr := r.armTimer(); timerErr != nil {
				return timerErr
			}
			r.timerActive = true
		}
		return err
	}
	r.registered = true
	r.cancelTimer()
	return nil
}

func (r *shellRegistration) cancelTimer() {
	if r.timerActive {
		r.killTimer()
		r.timerActive = false
	}
}

func (r *shellRegistration) stop() {
	r.stopped = true
	r.cancelTimer()
}
