//go:build !windows

package cmd

import (
	"errors"
	"log/slog"

	"github.com/Alia5/VIIPER/internal/log"
)

func (c *ServiceCommand) Run(_ *slog.Logger, _ log.RawLogger) error {
	return errors.New("the VIIPER native broker service is available only on Windows")
}
