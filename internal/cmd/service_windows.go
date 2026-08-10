//go:build windows

package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/internal/log"
	"golang.org/x/sys/windows/svc"
)

const NativeBrokerServiceName = "VIIPERNativeBroker"

const serviceStopTimeout = 30 * time.Second

type nativeBrokerService struct {
	run func(context.Context, func()) error
}

func (c *ServiceCommand) Run(logger *slog.Logger, rawLogger log.RawLogger) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service context: %w", err)
	}
	if !isService {
		return errors.New("the VIIPER native broker service command may only be started by Windows Service Control Manager")
	}
	if !strings.EqualFold(strings.TrimSpace(c.Transport), "native-ude") {
		return fmt.Errorf("the VIIPER native broker service requires --transport native-ude, got %q", c.Transport)
	}
	if strings.TrimSpace(c.KeyFile) == "" {
		path, pathErr := nativeServiceKeyFilePath()
		if pathErr != nil {
			return pathErr
		}
		c.KeyFile = path
	}
	c.serviceMode = true
	handler := &nativeBrokerService{run: func(ctx context.Context, ready func()) error {
		c.ready = ready
		return c.StartServer(ctx, logger, rawLogger)
	}}
	return svc.Run(NativeBrokerServiceName, handler)
}

func nativeServiceKeyFilePath() (string, error) {
	programData := strings.TrimSpace(os.Getenv("ProgramData"))
	if programData == "" {
		return "", errors.New("ProgramData is not set; refusing to place a machine service credential in a user profile")
	}
	if !filepath.IsAbs(programData) {
		return "", fmt.Errorf("ProgramData must be an absolute path: %s", programData)
	}
	return filepath.Join(filepath.Clean(programData), "VIIPER", keyFileName), nil
}

func (s *nativeBrokerService) Execute(
	_ []string,
	requests <-chan svc.ChangeRequest,
	changes chan<- svc.Status,
) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending, WaitHint: 15_000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	var readyOnce sync.Once
	done := make(chan error, 1)
	go func() {
		done <- s.run(ctx, func() { readyOnce.Do(func() { close(ready) }) })
	}()

	running := svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown,
	}
	starting := svc.Status{State: svc.StartPending, WaitHint: 15_000, CheckPoint: 1}
	for {
		select {
		case <-ready:
			changes <- running
			goto Running
		case err := <-done:
			changes <- svc.Status{State: svc.StopPending, WaitHint: 1_000}
			if err != nil {
				return true, 1
			}
			return true, 3
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				starting.CheckPoint++
				changes <- starting
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending, WaitHint: uint32(serviceStopTimeout / time.Millisecond)}
				cancel()
				return waitForServiceStop(done)
			}
		}
	}

Running:
	for {
		select {
		case err := <-done:
			changes <- svc.Status{State: svc.StopPending, WaitHint: 1_000}
			if err != nil {
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- running
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending, WaitHint: uint32(serviceStopTimeout / time.Millisecond)}
				cancel()
				return waitForServiceStop(done)
			}
		}
	}
}

func waitForServiceStop(done <-chan error) (bool, uint32) {
	timer := time.NewTimer(serviceStopTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return true, 1
		}
		return false, 0
	case <-timer.C:
		return true, 2
	}
}
