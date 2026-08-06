package xboxseries

import (
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usb"
)

func init() { api.RegisterDevice("xboxseries", &handler{}) }

type handler struct{}

func (h *handler) CreateDevice(o *device.CreateOptions) (usb.Device, error) { return New(o) }

func (h *handler) StreamHandler() api.StreamHandlerFunc {
	return func(conn net.Conn, devPtr *usb.Device, logger *slog.Logger) error {
		if devPtr == nil || *devPtr == nil {
			return fmt.Errorf("nil device")
		}
		xdev, ok := (*devPtr).(*XboxSeries)
		if !ok {
			return fmt.Errorf("device is not xboxseries")
		}
		xdev.SetMotorCallback(func(state MotorState) {
			data, err := state.MarshalBinary()
			if err != nil {
				logger.Error("failed to marshal Series motor state", "error", err)
				return
			}
			if _, err := conn.Write(data); err != nil {
				logger.Error("failed to send Series motor state", "error", err)
			}
		})
		defer xdev.SetMotorCallback(nil)

		buf := make([]byte, 20)
		for {
			if _, err := io.ReadFull(conn, buf); err != nil {
				if err == io.EOF {
					return nil
				}
				return fmt.Errorf("read Series input state: %w", err)
			}
			var state InputState
			if err := state.UnmarshalBinary(buf); err != nil {
				return fmt.Errorf("unmarshal Series input state: %w", err)
			}
			xdev.UpdateInputState(state)
		}
	}
}

func (h *handler) UpdateMetaState(_ string, _ *usb.Device) error { return nil }
