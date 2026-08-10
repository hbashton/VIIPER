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
	"time"

	"github.com/Alia5/VIIPER/internal/log"
	"golang.org/x/sys/windows/svc"
)

const NativeBrokerServiceName = "VIIPERNativeBroker"

const serviceStopTimeout = 30 * time.Second

type nativeBrokerService struct {
	run func(context.Context) error
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
	handler := &nativeBrokerService{run: func(ctx context.Context) error {
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
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()

	running := svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown,
	}
	changes <- running

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
				timer := time.NewTimer(serviceStopTimeout)
				select {
				case err := <-done:
					if !timer.Stop() {
						<-timer.C
					}
					if err != nil {
						return true, 1
					}
					return false, 0
				case <-timer.C:
					return true, 2
				}
			}
		}
	}
}
