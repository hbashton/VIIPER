package xboxone

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/usb"
)

// ProductionStreamHandler owns the sole full-duplex DS4Windows broker stream
// for an explicitly constructed Xbox persona. Server composition registers
// this handler deliberately; the device package must not import the internal
// API registry or mutate process-global routing from init().
func ProductionStreamHandler(
	conn net.Conn,
	devPtr *usb.Device,
	_ *slog.Logger,
) error {
	if conn == nil {
		return ErrProductionBrokerUnavailable
	}
	authenticated, ok := conn.(interface{ VIIPERAuthenticated() bool })
	if !ok || !authenticated.VIIPERAuthenticated() {
		return ErrProductionBrokerAuthenticationRequired
	}
	if devPtr == nil || *devPtr == nil {
		return ErrProductionBrokerUnavailable
	}
	controller, ok := (*devPtr).(*AuthorizedDormantRetainedUSBDevice)
	if !ok || controller.feedbackBridge == nil {
		return ErrProductionBrokerUnavailable
	}
	var lease controllerPersonaBrokerStreamLease
	if err := runProductionRegistrationAdmission(conn, controller, func() error {
		var acquired bool
		lease, acquired = controller.acquireBrokerStream()
		if !acquired {
			return ErrProductionBrokerUnavailable
		}
		return nil
	}); err != nil {
		return err
	}
	defer lease.release()

	var readScratch [productionBrokerMaximumPayload]byte
	reader := newProductionBrokerFrameReader(conn)
	ready, err := reader.read(readScratch[:])
	if err != nil {
		return err
	}
	if ready.typeID != productionBrokerConsumerReady ||
		ready.correlation != 0 {
		return errProductionBrokerWire
	}
	if err := runProductionRegistrationAdmission(conn, controller, lease.consumerReady); err != nil {
		return err
	}
	writer := newProductionBrokerFrameWriter(conn)
	if err := writer.write(productionBrokerConsumerReadyAck, 0, nil); err != nil {
		return err
	}

	stop := make(chan struct{})
	var stopOnce sync.Once
	stopWriter := func() { stopOnce.Do(func() { close(stop) }) }
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- writeCanonicalFeedbackStream(
			writer, controller.feedbackBridge, lease.token, stop)
		// A failed writer must wake a peer blocked in ReadFull. The outer API
		// handler owns an idempotent final Close as well.
		_ = conn.Close()
	}()

	readErr := readProductionBrokerStream(
		conn, reader, writer, lease, readScratch[:])
	stopWriter()
	_ = conn.Close()
	writeErr := <-writerDone
	if readErr == nil || errors.Is(readErr, net.ErrClosed) ||
		errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if writeErr == nil || errors.Is(writeErr, net.ErrClosed) ||
		errors.Is(writeErr, io.EOF) {
		writeErr = nil
	}
	return errors.Join(readErr, writeErr)
}

// The API wrapper supplies exact-registration fencing around the two short
// state transitions. Direct in-package composition retains its existing
// authenticated-connection contract; it does not perform numeric lookup.
func runProductionRegistrationAdmission(conn net.Conn, device *AuthorizedDormantRetainedUSBDevice, operation func() error) error {
	if admission, ok := conn.(interface {
		VIIPERAuthorizeXboxOneRegistration(*AuthorizedDormantRetainedUSBDevice, func() error) error
	}); ok {
		return admission.VIIPERAuthorizeXboxOneRegistration(device, operation)
	}
	return operation()
}

func readProductionBrokerStream(
	conn net.Conn,
	reader *productionBrokerFrameReader,
	writer *productionBrokerFrameWriter,
	lease controllerPersonaBrokerStreamLease,
	payloadScratch []byte,
) error {
	var inputFault error
	for {
		frame, err := reader.read(payloadScratch)
		if err != nil {
			return errors.Join(inputFault, err)
		}
		switch frame.typeID {
		case productionBrokerSemanticInput:
			status := []byte{productionBrokerAccepted}
			if err := lease.publishInput(
				frame.correlation, frame.payload); err != nil {
				status[0] = productionBrokerRejected
				if errors.Is(err, errRetainedInputHistoryFault) && inputFault == nil {
					inputFault = err
					// Bound BOTH directions before writing the rejection. A peer
					// cannot keep a retired input owner alive by sending more input
					// or ACKs, nor block the feedback writer indefinitely. This is
					// a teardown-only budget, not a new input/feedback polling timer.
					if deadlineErr := conn.SetDeadline(time.Now().Add(productionBrokerInputDrainTimeout)); deadlineErr != nil {
						return errors.Join(inputFault, deadlineErr)
					}
				}
				if writeErr := writer.write(productionBrokerSemanticInputAck,
					frame.correlation, status); writeErr != nil {
					return errors.Join(err, writeErr)
				}
				if errors.Is(err, errRetainedInputHistoryFault) {
					continue // only the original feedback/ACK lane remains useful
				}
				return fmt.Errorf("publish semantic Xbox input: %w", err)
			}
			if err := writer.write(productionBrokerSemanticInputAck,
				frame.correlation, status); err != nil {
				return err
			}
		case productionBrokerCanonicalAck:
			accepted := frame.payload[0] == productionBrokerAccepted
			if frame.payload[0] != productionBrokerAccepted &&
				frame.payload[0] != productionBrokerRejected {
				return errProductionBrokerWire
			}
			if err := lease.acknowledgeFeedback(
				frame.correlation, accepted); err != nil {
				return fmt.Errorf(
					"acknowledge canonical Xbox feedback: %w", err)
			}
		default:
			return errProductionBrokerWire
		}
	}
}

// Exceeds the default retained import's three five-second close phases, but
// never extends on traffic. Expiry is an ambiguous failure, never proof of Stop.
const productionBrokerInputDrainTimeout = 30 * time.Second

func writeCanonicalFeedbackStream(
	writer *productionBrokerFrameWriter,
	bridge *controllerPersonaFeedbackStreamBridge,
	token uint64,
	stop <-chan struct{},
) error {
	var revision uint64
	for {
		wire, next, ok := bridge.pendingAfter(token, revision, stop)
		if !ok {
			return nil
		}
		if err := writer.write(productionBrokerCanonicalFeedback,
			next, wire[:]); err != nil {
			return fmt.Errorf("write canonical Xbox feedback: %w", err)
		}
		revision = next
	}
}
