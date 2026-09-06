package xbox360

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usb"
)

func init() {
	api.RegisterDevice("xbox360", &handler{})
}

type handler struct{}

func (h *handler) CreateDevice(o *device.CreateOptions) (usb.Device, error) { return New(o) }

func (h *handler) StreamHandler() api.StreamHandlerFunc {
	return func(conn net.Conn, devPtr *usb.Device, logger *slog.Logger) error {
		if devPtr == nil || *devPtr == nil {
			return fmt.Errorf("nil device")
		}
		xdev, ok := (*devPtr).(*Xbox360)
		if !ok {
			return fmt.Errorf("device is not xbox360")
		}
		producerLease, acquired := xdev.acquireInputProducer()
		if !acquired {
			return fmt.Errorf("xbox360 input producer is already connected")
		}
		defer xdev.releaseInputProducer()

		xdev.SetRumbleCallback(func(rumble XRumbleState) {
			data, err := rumble.MarshalBinary()
			if err != nil {
				logger.Error("failed to marshal rumble", "error", err)
				return
			}
			if _, err := conn.Write(data); err != nil {
				logger.Error("failed to send rumble", "error", err)
			}
		})
		defer xdev.SetRumbleCallback(nil)

		buf := make([]byte, 20)
		overflowLogged := false
		for {
			if _, err := io.ReadFull(conn, buf); err != nil {
				if err == io.EOF {
					logger.Info("client disconnected")
					return nil
				}
				return fmt.Errorf("read input state: %w", err)
			}
			receivedAt := time.Now()

			var state InputState
			if err := state.UnmarshalBinary(buf); err != nil {
				return fmt.Errorf("unmarshal input state: %w", err)
			}
			var disposition inputpresentation.FixedReportPublishDisposition
			producerLease, disposition = xdev.publishInputStateWithLease(
				producerLease, state, receivedAt)
			if disposition.Accepted() {
				continue
			}
			switch disposition {
			case inputpresentation.FixedReportPublishRejectedOverflow:
				if !overflowLogged {
					logger.Warn("xbox360 compatibility journal rejected an input state; queued transitions remain ordered")
					overflowLogged = true
				}
			case inputpresentation.FixedReportPublishFaultedOverflow,
				inputpresentation.FixedReportPublishRejectedNeutralPending,
				inputpresentation.FixedReportPublishRejectedResynchronizationRequired,
				inputpresentation.FixedReportPublishRejectedStaleProducer:
				// Overflow or strict age policy purged the history and owns a mandatory
				// neutral. The freshest complete producer state is staged
				// for explicit resynchronization after that neutral commits.
				// A stale lease without fault state is a USB lifecycle fence;
				// that pre-boundary frame is deliberately discarded and the
				// successor lease applies to the next frame.
			default:
				return fmt.Errorf("publish xbox360 input state: disposition %d",
					disposition)
			}
		}
	}
}

func (h *handler) UpdateMetaState(meta string, dev *usb.Device) error {
	return nil
}
