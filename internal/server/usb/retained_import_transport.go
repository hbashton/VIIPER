package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

const retainedImportDefaultLifecycleTimeout = 5 * time.Second

type retainedImportTerminalError struct {
	transport error
	teardown  error
}

func (terminal *retainedImportTerminalError) Error() string {
	if terminal == nil {
		return "retained import terminal error"
	}
	joined := errors.Join(terminal.transport, terminal.teardown)
	if joined == nil {
		return "retained import terminal error"
	}
	return joined.Error()
}

func (terminal *retainedImportTerminalError) Unwrap() []error {
	if terminal == nil {
		return nil
	}
	causes := make([]error, 0, 2)
	if terminal.transport != nil {
		causes = append(causes, terminal.transport)
	}
	if terminal.teardown != nil {
		causes = append(causes, terminal.teardown)
	}
	return causes
}

type retainedConnectionResponses struct {
	writer  *responseWriter
	batcher *batchingWriter
}

// retainedImportIngressGate keeps a pre-reply commit failure from closing the
// socket before OP_REP_IMPORT failure can be framed. The session still owns the
// exact ingress Close call; before arm that call is latched, and after a reply
// has been attempted finalize closes the underlying connection. A successful
// commit arms ordinary lifecycle Close before the success reply is written.
type retainedImportIngressGate struct {
	mu             sync.Mutex
	conn           net.Conn
	armed          bool
	closeRequested bool
	closed         bool
}

func newRetainedImportIngressGate(conn net.Conn) *retainedImportIngressGate {
	return &retainedImportIngressGate{conn: conn}
}

func (gate *retainedImportIngressGate) Close() error {
	if gate == nil || gate.conn == nil {
		return errRetainedImportInvalid
	}
	gate.mu.Lock()
	gate.closeRequested = true
	if !gate.armed || gate.closed {
		gate.mu.Unlock()
		return nil
	}
	gate.closed = true
	conn := gate.conn
	gate.mu.Unlock()
	return closeRetainedIngressConn(conn)
}

func (gate *retainedImportIngressGate) arm() error {
	if gate == nil || gate.conn == nil {
		return errRetainedImportInvalid
	}
	gate.mu.Lock()
	if gate.armed || gate.closed || gate.closeRequested {
		gate.mu.Unlock()
		return errRetainedImportInvalid
	}
	gate.armed = true
	gate.mu.Unlock()
	return nil
}

func (gate *retainedImportIngressGate) finalizeFailureReply() error {
	if gate == nil || gate.conn == nil {
		return errRetainedImportInvalid
	}
	gate.mu.Lock()
	if gate.closed {
		gate.mu.Unlock()
		return nil
	}
	gate.armed = true
	gate.closeRequested = true
	gate.closed = true
	conn := gate.conn
	gate.mu.Unlock()
	return closeRetainedIngressConn(conn)
}

func closeRetainedIngressConn(conn net.Conn) error {
	if conn == nil {
		return errRetainedImportInvalid
	}
	// Classify every leaf. A joined net.ErrClosed plus an invariant failure is
	// not an idempotent close merely because errors.Is finds the stopped
	// sentinel among its causes.
	return ordinaryServerTerminalFailure(conn.Close())
}

func newRetainedConnectionResponses(
	conn net.Conn,
	flushInterval time.Duration,
) retainedConnectionResponses {
	if flushInterval <= 0 {
		return retainedConnectionResponses{writer: newResponseWriter(conn, nil)}
	}
	batcher := newBatchingWriter(
		conn, writeBatcherBufferSize, flushInterval, writeBatcherFlushAtBytes)
	return retainedConnectionResponses{
		writer: newResponseWriter(batcher, batcher), batcher: batcher,
	}
}

func (responses retainedConnectionResponses) close() error {
	if responses.batcher == nil {
		return nil
	}
	return responses.batcher.Close()
}

// handleRetainedImport is the only production composition point for the
// retained import authority and parked scheduler. It is reachable only when
// both the server has an explicit nonzero authority ID and the selected device
// implements retainedusb.ImportDevice. Bind and scheduler activation complete
// before the successful OP_REP_IMPORT is allowed onto the wire.
func (s *Server) handleRetainedImport(
	conn net.Conn,
	selection importSelection,
	owner retainedusb.ImportOwner,
	deviceID uint64,
) (resultErr error) {
	if s == nil || s.config == nil || s.retainedImports == nil || interfaceIsNil(owner) ||
		deviceID == 0 || selection.retainedCallbacks == nil ||
		!selection.retainedCallbacks.authenticates(s, selection.dev) {
		_ = writeImportFailure(conn)
		return errRetainedImportInvalid
	}
	if selection.wireDeviceID == 0 {
		_ = writeImportFailure(conn)
		return errRetainedImportInvalid
	}
	// New copies ServerConfig; take the import's cold scalar snapshot before
	// reserving or binding an owner. Embedders bypassing CLI enum validation
	// must fail closed too, never activating an unbounded/invalid cadence.
	inputServiceMS := s.config.RetainedInputServiceMS
	if _, err := retainedInputServiceInterval(inputServiceMS); err != nil {
		_ = writeImportFailure(conn)
		return err
	}
	limits, limitsErr := invokeRetainedImportStep(
		"read retained import limits",
		time.Now().Add(s.retainedLifecycleTimeout()),
		func() (retainedusb.Limits, error) {
			return invokeRetainedLimits(owner)
		})
	if limitsErr == nil {
		limitsErr = selection.retainedCallbacks.bindLimits(limits)
	}
	if limitsErr != nil ||
		validateRetainedImportDescriptor(selection.descriptor, limits) != nil {
		selection.rejectRetainedCallbacks(errors.Join(
			limitsErr, validateRetainedImportDescriptor(selection.descriptor, limits)))
		_ = writeImportFailure(conn)
		if limitsErr != nil {
			return fmt.Errorf("read retained import limits: %w", limitsErr)
		}
		return fmt.Errorf("retained import descriptor and owner limits diverge")
	}
	if !selection.retainedCallbacks.authenticates(s, selection.dev) {
		_ = writeImportFailure(conn)
		return errRetainedImportQuarantined
	}
	deviceContext, owningBus := s.retainedDeviceContext(selection)
	if deviceContext == nil || owningBus == nil {
		_ = writeImportFailure(conn)
		return fmt.Errorf("retained import device does not belong to an active bus")
	}
	if deviceContext.Err() != nil ||
		!selection.retainedCallbacks.authenticates(s, selection.dev) {
		_ = writeImportFailure(conn)
		return fmt.Errorf("retained import device was removed during admission")
	}

	lifecycleTimeout := s.retainedLifecycleTimeout()
	streamContext, cancelStream := context.WithCancel(deviceContext)
	defer cancelStream()
	streamID, registered := s.registerRetainedDeviceStream(cancelStream, selection.registration)
	if !registered {
		_ = writeImportFailure(conn)
		return fmt.Errorf("retained import rejected while server is closing")
	}
	defer func() { s.finishRetainedStream(streamID, resultErr) }()

	reserveDeadline := time.Now().Add(lifecycleTimeout)
	reservation, lease, err := s.retainedImports.reserveWithIdentityObserver(
		deviceID, owner, reserveDeadline,
		selection.retainedCallbacks.bindOwnerIdentity)
	if err != nil {
		if errors.Is(err, errRetainedImportLifecycle) {
			selection.rejectRetainedCallbacks(err)
		}
		_ = writeImportFailure(conn)
		return fmt.Errorf("reserve retained import: %w", err)
	}
	if deviceContext.Err() != nil ||
		!selection.retainedCallbacks.authenticates(s, selection.dev) {
		abortErr := s.retainedImports.abort(reservation)
		_ = writeImportFailure(conn)
		return errors.Join(
			fmt.Errorf("retained import device was removed during reservation"),
			abortErr)
	}
	responses := newRetainedConnectionResponses(
		conn, s.config.WriteBatchFlushInterval)
	defer func() { _ = responses.close() }()
	readinessSnapshot, readinessErr := invokeRetainedImportStep(
		"read retained import readiness", time.Now().Add(lifecycleTimeout),
		func() (retainedSchedulerConstructionSnapshot, error) {
			epoch, readiness, err := sampleRetainedReadiness(owner, nil, 0)
			return retainedSchedulerConstructionSnapshot{
				ownerIdentity: lease.OwnerID,
				limits:        limits, readinessEpoch: epoch, readiness: readiness,
			}, err
		})
	if readinessErr != nil {
		selection.rejectRetainedCallbacks(readinessErr)
		abortErr := s.retainedImports.abort(reservation)
		_ = writeImportFailure(conn)
		return errors.Join(
			fmt.Errorf("snapshot retained import readiness: %w", readinessErr),
			abortErr)
	}
	if deviceContext.Err() != nil ||
		!selection.retainedCallbacks.authenticates(s, selection.dev) {
		abortErr := s.retainedImports.abort(reservation)
		_ = writeImportFailure(conn)
		return errors.Join(
			fmt.Errorf("retained import device was removed during readiness"),
			abortErr)
	}

	scheduler, err := newParkedRetainedImportScheduler(
		streamContext, reservation, owner, responses.writer, &readinessSnapshot)
	if err != nil {
		selection.rejectRetainedCallbacks(err)
		_ = s.retainedImports.abort(reservation)
		_ = writeImportFailure(conn)
		return fmt.Errorf("build retained import scheduler: %w", err)
	}
	if scheduler.limits != limits {
		cleanupErr := s.retainedImports.abortPrepared(
			reservation, time.Now().Add(lifecycleTimeout))
		_ = writeImportFailure(conn)
		return errors.Join(
			fmt.Errorf("retained import limits changed during composition"),
			cleanupErr)
	}
	if err := scheduler.configureEndpointCadence(selection.descriptor, inputServiceMS); err != nil {
		cleanupErr := s.retainedImports.abortPrepared(
			reservation, time.Now().Add(lifecycleTimeout))
		_ = writeImportFailure(conn)
		return errors.Join(
			fmt.Errorf("configure retained endpoint cadence: %w", err), cleanupErr)
	}
	if err := scheduler.configureControlLifecycle(
		lease.SessionGeneration,
		func(lifecycle controlLifecycleSetup) {
			s.applyDeliveredTransactionalControlLifecycle(
				nil, selection.dev, lifecycle)
		},
		func(
			setup [8]byte,
			direction retainedusb.Direction,
			transferLength uint32,
		) controlLifecycleSetup {
			usbipDirection := uint32(usbip.DirOut)
			if direction == retainedusb.DirectionIn {
				usbipDirection = usbip.DirIn
			}
			return s.parseControlLifecycleSubmissionDescriptor(
				selection.dev, selection.descriptor, 0, usbipDirection,
				transferLength, setup[:])
		},
	); err != nil {
		cleanupErr := s.retainedImports.abortPrepared(
			reservation, time.Now().Add(lifecycleTimeout))
		_ = writeImportFailure(conn)
		return errors.Join(
			fmt.Errorf("configure retained control lifecycle: %w", err),
			cleanupErr)
	}
	if deviceContext.Err() != nil ||
		!selection.retainedCallbacks.authenticates(s, selection.dev) {
		cleanupErr := s.retainedImports.abortPrepared(
			reservation, time.Now().Add(lifecycleTimeout))
		_ = writeImportFailure(conn)
		return errors.Join(
			fmt.Errorf("retained import device was removed before bind"),
			cleanupErr)
	}

	bindDeadline := time.Now().Add(lifecycleTimeout)
	activationDeadline := bindDeadline.Add(lifecycleTimeout)
	cleanupDeadline := activationDeadline.Add(lifecycleTimeout)
	ingress := newRetainedImportIngressGate(conn)
	session, err := s.retainedImports.commit(
		reservation, ingress, scheduler, bindDeadline, activationDeadline,
		cleanupDeadline)
	if err != nil {
		if !errors.Is(err, errRetainedImportBindRejected) {
			selection.rejectRetainedCallbacks(err)
		}
		replyErr := writeImportFailure(conn)
		closeErr := ingress.finalizeFailureReply()
		return &retainedImportTerminalError{
			transport: errors.Join(replyErr, closeErr),
			teardown:  fmt.Errorf("commit retained import: %w", err),
		}
	}
	if deviceContext.Err() != nil ||
		!selection.retainedCallbacks.authenticates(s, selection.dev) {
		replyErr := writeImportFailure(conn)
		closeErr := closeRetainedImportSession(
			session, lease, retainedusb.ImportCloseExplicitDetach,
			s.retainedCloseDeadline())
		finalizeErr := ingress.finalizeFailureReply()
		if closeErr != nil {
			selection.rejectRetainedCallbacks(closeErr)
			return &retainedImportTerminalError{
				transport: errors.Join(replyErr, finalizeErr),
				teardown:  closeErr,
			}
		}
		return errors.Join(replyErr, finalizeErr)
	}
	if err := ingress.arm(); err != nil {
		selection.rejectRetainedCallbacks(err)
		return &retainedImportTerminalError{
			teardown: errors.Join(err,
				s.closeRetainedImportAndRetireOneShot(
					session, lease, retainedusb.ImportCloseInvariantFailure,
					s.retainedCloseDeadline(), selection)),
		}
	}

	if err := responses.writer.write(
		buildImportSuccessPacket(selection), true, time.Now()); err != nil {
		closeErr := s.closeRetainedImportAndRetireOneShot(
			session, lease, retainedusb.ImportCloseWriteFailure,
			s.retainedCloseDeadline(), selection)
		if closeErr != nil {
			selection.rejectRetainedCallbacks(closeErr)
		}
		return &retainedImportTerminalError{
			transport: err,
			teardown:  closeErr,
		}
	}
	_ = conn.SetDeadline(time.Time{})
	// Retained ImportDevice owns the complete EP0 state machine and begins in
	// USB Default state. Endpoint routes become active only after its delivered
	// SET_CONFIGURATION completion publishes the accepted transition below.
	s.setDeviceConfiguration(selection.dev, 0)
	defer s.clearDeviceConfiguration(selection.dev)
	defer s.clearInterfaceAlt(selection.dev)

	reason, streamErr := s.handleRetainedURBStream(
		streamContext, conn, scheduler, selection.dev, selection.descriptor,
		selection.wireDeviceID)
	closeErr := s.closeRetainedImportAndRetireOneShot(
		session, lease, reason, s.retainedCloseDeadline(),
		selection)
	if closeErr != nil {
		selection.rejectRetainedCallbacks(closeErr)
		return &retainedImportTerminalError{
			transport: streamErr, teardown: closeErr,
		}
	}
	if streamErr == nil && reason == retainedusb.ImportCloseContextCanceled {
		return nil
	}
	return streamErr
}

func validateRetainedImportDescriptor(
	descriptor *rootusb.Descriptor,
	limits retainedusb.Limits,
) error {
	if descriptor == nil || !limits.Valid() {
		return errRetainedImportInvalid
	}
	if !validRetainedControlEndpointForSpeed(descriptor.Device) {
		return errRetainedImportInvalid
	}
	if descriptor.Device.BNumConfigurations != 1 ||
		descriptor.Configuration.BConfigurationValue == 0 {
		return errRetainedImportInvalid
	}
	if len(descriptor.Interfaces) != 1 || len(descriptor.Associations) != 0 ||
		limits.InterruptInRoute.InterfaceNumber !=
			limits.InterruptOutRoute.InterfaceNumber ||
		limits.InterruptInRoute.AlternateSetting !=
			limits.InterruptOutRoute.AlternateSetting {
		return errRetainedImportInvalid
	}
	retainedInterface := &descriptor.Interfaces[0]
	if retainedInterface.Descriptor.BInterfaceNumber !=
		limits.InterruptInRoute.InterfaceNumber ||
		retainedInterface.Descriptor.BInterfaceNumber != 0 ||
		retainedInterface.Descriptor.BAlternateSetting !=
			limits.InterruptInRoute.AlternateSetting ||
		retainedInterface.Descriptor.BAlternateSetting != 0 ||
		retainedInterface.Descriptor.BNumEndpoints != 2 ||
		len(retainedInterface.Endpoints) != 2 ||
		retainedInterface.HID != nil ||
		len(retainedInterface.ClassDescriptors) != 0 {
		return errRetainedImportInvalid
	}
	for endpointIndex := range retainedInterface.Endpoints {
		endpoint := &retainedInterface.Endpoints[endpointIndex]
		if len(endpoint.Trailing) != 0 ||
			len(endpoint.ClassDescriptors) != 0 {
			return errRetainedImportInvalid
		}
	}
	if err := validateRetainedInterruptRoute(
		descriptor, limits.InterruptInRoute, limits.MaximumInterruptIn); err != nil {
		return err
	}
	return validateRetainedInterruptRoute(
		descriptor, limits.InterruptOutRoute, limits.MaximumInterruptOut)
}

func validateRetainedInterruptRoute(
	descriptor *rootusb.Descriptor,
	route retainedusb.Route,
	maximum uint32,
) error {
	interfaceMatches := 0
	matches := 0
	for interfaceIndex := range descriptor.Interfaces {
		current := &descriptor.Interfaces[interfaceIndex]
		if current.Descriptor.BInterfaceNumber != route.InterfaceNumber ||
			current.Descriptor.BAlternateSetting != route.AlternateSetting {
			continue
		}
		interfaceMatches++
		if interfaceMatches != 1 ||
			int(current.Descriptor.BNumEndpoints) != len(current.Endpoints) {
			return errRetainedImportInvalid
		}
		for endpointIndex := range current.Endpoints {
			endpoint := current.Endpoints[endpointIndex]
			if endpoint.BEndpointAddress != route.EndpointAddress {
				continue
			}
			matches++
			if endpoint.BMAttributes != 0x03 ||
				!validRetainedInterruptEndpointForSpeed(
					descriptor.Device, endpoint) ||
				uint32(endpoint.WMaxPacketSize) != maximum ||
				endpoint.BInterval == 0 {
				return errRetainedImportInvalid
			}
		}
	}
	if matches != 1 {
		return errRetainedImportInvalid
	}
	return nil
}

func (s *Server) retainedLifecycleTimeout() time.Duration {
	if s != nil && s.config != nil && s.config.ConnectionTimeout > 0 {
		return s.config.ConnectionTimeout
	}
	return retainedImportDefaultLifecycleTimeout
}

func (s *Server) retainedCloseDeadline() time.Time {
	if s != nil {
		s.serverLifecycleMu.Lock()
		deadline := s.serverShutdownDeadline
		s.serverLifecycleMu.Unlock()
		if !deadline.IsZero() {
			return deadline
		}
	}
	return time.Now().Add(3 * s.retainedLifecycleTimeout())
}

func closeRetainedImportSession(
	session *retainedImportSession,
	lease retainedusb.ImportLease,
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) error {
	if session == nil {
		return errRetainedImportInvalid
	}
	_, err := session.close(lease, reason, deadline)
	return err
}

func (s *Server) closeRetainedImportAndRetireOneShot(
	session *retainedImportSession,
	lease retainedusb.ImportLease,
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
	selection importSelection,
) error {
	closeErr := closeRetainedImportSession(session, lease, reason, deadline)
	dev := selection.dev
	if closeErr != nil || dev == nil || selection.registration.Bus == nil {
		return closeErr
	}
	oneShot, ok := dev.(retainedusb.OneShotImportDevice)
	if !ok {
		return nil
	}
	removeAfterSafeDisconnect, policyErr := invokeRetainedImportStep(
		"retained one-shot retirement policy", deadline,
		func() (bool, error) {
			return oneShot.RetainedUSBRemoveAfterSafeDisconnect(), nil
		})
	if policyErr != nil {
		return fmt.Errorf("read retained one-shot retirement policy: %w",
			policyErr)
	}
	if !removeAfterSafeDisconnect {
		return nil
	}
	if s == nil {
		return errRetainedImportInvalid
	}
	if err := s.removeRetainedRegistrationIfPresent(
		selection.registration); err != nil {
		return fmt.Errorf("remove safely disconnected retained one-shot device: %w", err)
	}
	return nil
}

func (s *Server) removeRetainedRegistrationIfPresent(
	registration virtualbus.DeviceMeta,
) error {
	if s == nil || registration.Bus == nil || registration.Dev == nil ||
		registration.RegistrationToken == 0 {
		return errRetainedImportInvalid
	}
	_, err := s.RemoveDeviceRegistrationIfPresent(registration)
	return err
}

func (s *Server) retainedDeviceContext(
	selection importSelection,
) (context.Context, *virtualbus.VirtualBus) {
	if s == nil || selection.dev == nil || selection.bus == nil ||
		selection.deviceContext == nil || selection.registration.Bus != selection.bus {
		return nil, nil
	}
	s.busesMu.Lock()
	registered := s.busses[selection.meta.BusID]
	authenticates := registered == selection.bus &&
		selection.bus.AuthenticatesRegistration(selection.registration)
	s.busesMu.Unlock()
	if !authenticates {
		return nil, nil
	}
	return selection.deviceContext, selection.bus
}

// handleRetainedURBStream owns framing for the fixed retained control,
// interrupt-IN, and interrupt-OUT lanes. Unsupported endpoints and ISO shapes
// are consumed completely and stalled; malformed connection-level framing is
// terminal. The legacy endpoint scheduler is never constructed for this path.
func (s *Server) handleRetainedURBStream(
	ctx context.Context,
	conn net.Conn,
	scheduler *retainedSubmissionScheduler,
	dev rootusb.Device,
	descriptor *rootusb.Descriptor,
	expectedWireDeviceID uint32,
) (retainedusb.ImportCloseReason, error) {
	if ctx == nil || conn == nil || scheduler == nil ||
		dev == nil || descriptor == nil || expectedWireDeviceID == 0 {
		return retainedusb.ImportCloseInvariantFailure,
			errRetainedImportInvalid
	}

	readerDone := make(chan struct{})
	defer close(readerDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-scheduler.done:
			_ = conn.Close()
		case <-readerDone:
		}
	}()

	limits := scheduler.limits
	var header [urbHdrSize]byte
	var outPayloadScratch []byte
	var responseScratch []byte
	var unlinkScratch []byte
	isoPacketScratch := make([]usbip.IsoPacketDescriptor, maxIsoPackets)
	isoPacketWireScratch := make([]byte, maxIsoPackets*usbip.IsoPacketDescriptorSize)
	var framingSequence uint32
	framingReserved := false
	defer func() {
		if framingReserved {
			scheduler.abandonFramingSequence(framingSequence)
		}
	}()

	for {
		if err := usbip.ReadExactly(conn, header[:]); err != nil {
			return retainedReadFailure(ctx, scheduler, err)
		}
		command := binary.BigEndian.Uint32(
			header[urbHdrOffsetCommand : urbHdrOffsetCommand+4])
		seq := binary.BigEndian.Uint32(
			header[urbHdrOffsetSeqnum : urbHdrOffsetSeqnum+4])
		dir := binary.BigEndian.Uint32(
			header[urbHdrOffsetDir : urbHdrOffsetDir+4])
		ep := binary.BigEndian.Uint32(
			header[urbHdrOffsetEp : urbHdrOffsetEp+4])
		wireDeviceID := binary.BigEndian.Uint32(
			header[urbHdrOffsetDevid : urbHdrOffsetDevid+4])
		if wireDeviceID != expectedWireDeviceID {
			return retainedusb.ImportCloseInvariantFailure,
				fmt.Errorf(
					"retained URB device %#08x does not match import %#08x (seq=%d)",
					wireDeviceID, expectedWireDeviceID, seq)
		}
		if err := scheduler.reserveFramingSequence(seq); err != nil {
			return retainedusb.ImportCloseInvariantFailure,
				fmt.Errorf("reserve retained framing seq %d: %w", seq, err)
		}
		framingSequence = seq
		framingReserved = true
		if dir != usbip.DirOut && dir != usbip.DirIn {
			return retainedusb.ImportCloseInvariantFailure,
				fmt.Errorf("invalid retained URB direction %d (seq=%d)", dir, seq)
		}
		if ep > 15 {
			return retainedusb.ImportCloseInvariantFailure,
				fmt.Errorf("invalid retained URB endpoint %d (seq=%d)", ep, seq)
		}

		if command == usbip.CmdUnlinkCode {
			unlinkSequence := binary.BigEndian.Uint32(
				header[urbHdrOffsetUnlink : urbHdrOffsetUnlink+4])
			removed, err := scheduler.unlink(unlinkSequence, time.Now())
			if err != nil {
				return retainedusb.ImportCloseInvariantFailure, err
			}
			status := int32(0)
			if removed {
				status = errConnReset
			}
			unlinkScratch = buildRetUnlinkPacket(unlinkScratch, seq, status)
			if err := scheduler.finishImmediateFramingSequence(seq); err != nil {
				return retainedusb.ImportCloseInvariantFailure, err
			}
			framingReserved = false
			if err := scheduler.responses.write(
				unlinkScratch, true, time.Now()); err != nil {
				return retainedusb.ImportCloseWriteFailure, err
			}
			continue
		}
		if command != usbip.CmdSubmitCode {
			return retainedusb.ImportCloseInvariantFailure,
				fmt.Errorf("unsupported retained USB/IP command %d (seq=%d)",
					command, seq)
		}

		transferLength := binary.BigEndian.Uint32(
			header[urbHdrOffsetLength : urbHdrOffsetLength+4])
		if transferLength > maximumTransferSize {
			return retainedusb.ImportCloseInvariantFailure,
				fmt.Errorf("retained transfer length %d exceeds limit %d",
					transferLength, maximumTransferSize)
		}
		packetCount := int32(binary.BigEndian.Uint32(
			header[urbHdrOffsetPackets : urbHdrOffsetPackets+4]))
		if packetCount < -1 || packetCount > maxIsoPackets {
			return retainedusb.ImportCloseInvariantFailure,
				fmt.Errorf("invalid retained ISO packet count %d", packetCount)
		}
		if ep == 0 && s.logger != nil {
			setup := header[urbHdrOffsetSetup:urbHdrSize]
			s.logger.Debug("retained EP0 submit",
				"seq", seq,
				"direction", dir,
				"transferLength", transferLength,
				"bmRequestType", setup[0],
				"bRequest", setup[1],
				"wValue", binary.LittleEndian.Uint16(setup[2:4]),
				"wIndex", binary.LittleEndian.Uint16(setup[4:6]),
				"wLength", binary.LittleEndian.Uint16(setup[6:8]))
		}

		var outPayload []byte
		if dir == usbip.DirOut && transferLength > 0 {
			outPayloadScratch = resizeBytes(
				outPayloadScratch, int(transferLength))
			outPayload = outPayloadScratch
			if err := usbip.ReadExactly(conn, outPayload); err != nil {
				return retainedusb.ImportCloseReadFailure,
					fmt.Errorf("read retained OUT payload: %w", err)
			}
		}

		var isoPackets []usbip.IsoPacketDescriptor
		if packetCount > 0 {
			isoPackets = isoPacketScratch[:packetCount]
			if err := readIsoPacketDescriptors(
				conn, isoPacketWireScratch, isoPackets); err != nil {
				return retainedusb.ImportCloseReadFailure,
					fmt.Errorf("read retained ISO descriptors: %w", err)
			}
			for index := range isoPackets {
				isoPackets[index].ActualLength = 0
				isoPackets[index].Status = errPipe
			}
			responseScratch = buildRetSubmitPacket(
				responseScratch, seq, errPipe, 0, nil, isoPackets, true)
			if err := scheduler.finishImmediateFramingSequence(seq); err != nil {
				return retainedusb.ImportCloseInvariantFailure, err
			}
			framingReserved = false
			if err := scheduler.responses.write(
				responseScratch, true, time.Now()); err != nil {
				return retainedusb.ImportCloseWriteFailure, err
			}
			continue
		}

		envelope, accepted := retainedEnvelopeForSubmission(
			limits, seq, dir, ep, transferLength,
			header[urbHdrOffsetSetup:urbHdrSize], outPayload,
			scheduler.session)
		if !accepted {
			responseScratch = buildRetSubmitPacket(
				responseScratch, seq, errPipe, 0, nil, nil, false)
			if err := scheduler.finishImmediateFramingSequence(seq); err != nil {
				return retainedusb.ImportCloseInvariantFailure, err
			}
			framingReserved = false
			if err := scheduler.responses.write(
				responseScratch, true, time.Now()); err != nil {
				return retainedusb.ImportCloseWriteFailure, err
			}
			continue
		}
		if err := scheduler.enqueueReservedAfterVisibleCompletion(envelope); err != nil {
			if errors.Is(err, errRetainedSubmissionInactiveRoute) {
				responseScratch = buildRetSubmitPacket(
					responseScratch, seq, errPipe, 0, nil, nil, false)
				if finishErr := scheduler.finishImmediateFramingSequence(seq); finishErr != nil {
					return retainedusb.ImportCloseInvariantFailure, finishErr
				}
				framingReserved = false
				if writeErr := scheduler.responses.write(
					responseScratch, true, time.Now()); writeErr != nil {
					return retainedusb.ImportCloseWriteFailure, writeErr
				}
				continue
			}
			if errors.Is(err, errRetainedSubmissionQueueFull) {
				responseScratch = buildRetSubmitPacket(
					responseScratch, seq, errNoSpace, 0, nil, nil, false)
				if finishErr := scheduler.finishImmediateFramingSequence(seq); finishErr != nil {
					return retainedusb.ImportCloseInvariantFailure, finishErr
				}
				framingReserved = false
				if writeErr := scheduler.responses.write(
					responseScratch, true, time.Now()); writeErr != nil {
					return retainedusb.ImportCloseWriteFailure, writeErr
				}
				continue
			}
			return retainedusb.ImportCloseInvariantFailure,
				fmt.Errorf("enqueue retained submission seq %d: %w", seq, err)
		}
		framingReserved = false
	}
}

func retainedImportWireDeviceID(meta usbip.ExportMeta) (uint32, error) {
	const maximumComponent = uint32(0xffff)
	if meta.BusID == 0 || meta.BusID > maximumComponent ||
		meta.DevID == 0 || meta.DevID > maximumComponent {
		return 0, fmt.Errorf(
			"%w: retained USB/IP bus/device address exceeds 16 bits",
			errRetainedImportInvalid)
	}
	return meta.BusID<<16 | meta.DevID, nil
}

func retainedReadFailure(
	ctx context.Context,
	scheduler *retainedSubmissionScheduler,
	err error,
) (retainedusb.ImportCloseReason, error) {
	if scheduler != nil {
		scheduler.mu.Lock()
		failure := scheduler.failure
		retirement := scheduler.retirement
		scheduler.mu.Unlock()
		if failure != nil {
			return retainedusb.ImportCloseInvariantFailure,
				errors.Join(err, failure, retainedImportRetirementDiagnostic(retirement))
		}
		if retirement.Valid() {
			return retainedusb.ImportCloseOwnerRequested,
				errors.Join(err, retainedImportRetirementDiagnostic(retirement))
		}
	}
	if ctx != nil && ctx.Err() != nil {
		return retainedusb.ImportCloseContextCanceled, nil
	}
	if errors.Is(err, io.EOF) || isClientDisconnect(err) {
		return retainedusb.ImportClosePeerDisconnect, err
	}
	return retainedusb.ImportCloseReadFailure, err
}

func retainedEnvelopeForSubmission(
	limits retainedusb.Limits,
	sequence uint32,
	direction uint32,
	endpoint uint32,
	transferLength uint32,
	setup []byte,
	outPayload []byte,
	bindingGeneration uint64,
) (retainedSubmissionEnvelope, bool) {
	if len(setup) != 8 || bindingGeneration == 0 {
		return retainedSubmissionEnvelope{}, false
	}
	envelope := retainedSubmissionEnvelope{
		sequence: sequence, transferLength: transferLength,
		bindingGeneration: bindingGeneration,
	}
	copy(envelope.setup[:], setup)
	switch direction {
	case usbip.DirIn:
		envelope.direction = retainedusb.DirectionIn
	case usbip.DirOut:
		envelope.direction = retainedusb.DirectionOut
	default:
		return retainedSubmissionEnvelope{}, false
	}

	if endpoint == 0 {
		envelope.lane = retainedusb.LaneControl
		setupIn := envelope.setup[0]&0x80 != 0
		setupLength := uint32(binary.LittleEndian.Uint16(envelope.setup[6:8]))
		if setupIn != (envelope.direction == retainedusb.DirectionIn) ||
			setupLength != transferLength {
			return retainedSubmissionEnvelope{}, false
		}
		if envelope.direction == retainedusb.DirectionIn {
			// wLength is the host's requested maximum, not the device's actual
			// response capacity. The setup field already bounds this value to
			// uint16; the retained scheduler separately caps the writable response
			// window at MaximumControlResponse. Rejecting a larger, self-consistent
			// request here would incorrectly stall lawful short descriptors.
			if len(outPayload) != 0 {
				return retainedSubmissionEnvelope{}, false
			}
		} else {
			if transferLength > limits.MaximumControlOut ||
				uint64(len(outPayload)) != uint64(transferLength) {
				return retainedSubmissionEnvelope{}, false
			}
			envelope.data = outPayload
		}
		return envelope, true
	}

	if envelope.setup != ([8]byte{}) || endpoint > 15 {
		return retainedSubmissionEnvelope{}, false
	}
	switch envelope.direction {
	case retainedusb.DirectionIn:
		route := limits.InterruptInRoute
		if uint32(route.EndpointAddress&0x0f) != endpoint ||
			transferLength == 0 || transferLength > limits.MaximumInterruptIn {
			return retainedSubmissionEnvelope{}, false
		}
		envelope.lane = retainedusb.LaneInterruptIn
		envelope.route = route
	case retainedusb.DirectionOut:
		route := limits.InterruptOutRoute
		if uint32(route.EndpointAddress&0x0f) != endpoint ||
			transferLength == 0 || transferLength > limits.MaximumInterruptOut ||
			uint64(len(outPayload)) != uint64(transferLength) {
			return retainedSubmissionEnvelope{}, false
		}
		envelope.lane = retainedusb.LaneInterruptOut
		envelope.route = route
		envelope.data = outPayload
	default:
		return retainedSubmissionEnvelope{}, false
	}
	return envelope, true
}
