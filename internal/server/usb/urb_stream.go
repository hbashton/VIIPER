package usb

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"time"

	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

const maximumVersionedInputReportSize = 1024

func (s *Server) handleUrbStream(conn net.Conn, dev usb.Device) error {
	_ = conn.SetDeadline(time.Time{})
	initialConfiguration := descriptorConfigurationValue(dev.GetDescriptor())
	if _, ownsEndpointZero := dev.(usb.TransactionalControlDevice); ownsEndpointZero {
		// A complete EP0 owner begins at the USB Default-state configuration
		// boundary. Legacy devices retain the historical preselected descriptor
		// configuration because changing it would alter their endpoint routing.
		initialConfiguration = 0
	}
	s.setDeviceConfiguration(dev, initialConfiguration)
	defer func() {
		s.resetInterfaceAlts(dev)
		s.clearDeviceConfiguration(dev)
	}()

	var wireWriter = conn
	var batcher *batchingWriter
	if s.config.WriteBatchFlushInterval > 0 {
		batcher = newBatchingWriter(
			conn,
			writeBatcherBufferSize,
			s.config.WriteBatchFlushInterval,
			writeBatcherFlushAtBytes,
		)
		wireWriter = nil
		defer func() { _ = batcher.Close() }()
	}
	var responseDestination interface{ Write([]byte) (int, error) } = wireWriter
	if batcher != nil {
		responseDestination = batcher
	}
	responses := newResponseWriter(responseDestination, batcher)

	var owningBus *virtualbus.VirtualBus
	s.busesMu.Lock()
	buses := make([]*virtualbus.VirtualBus, 0, len(s.busses))
	for _, bus := range s.busses {
		buses = append(buses, bus)
	}
	s.busesMu.Unlock()
	for _, bus := range buses {
		if slices.Contains(bus.Devices(), dev) {
			owningBus = bus
			break
		}
	}
	if owningBus == nil {
		return fmt.Errorf("device does not belong to any bus")
	}

	deviceContext := owningBus.GetDeviceContext(dev)
	if deviceContext == nil {
		return fmt.Errorf("no device context available from bus")
	}

	schedulers := newEndpointSchedulers(deviceContext, dev, responses, conn)
	s.registerEndpointSchedulers(schedulers)
	defer func() {
		schedulers.close()
		s.unregisterEndpointSchedulers(schedulers)
	}()
	go s.emitEndpointDiagnostics(schedulers.ctx, schedulers)
	go func() {
		<-schedulers.ctx.Done()
		_ = conn.Close()
	}()

	var outPayloadScratch []byte
	isoPacketScratch := make([]usbip.IsoPacketDescriptor, maxIsoPackets)
	isoPacketWireScratch := make([]byte, maxIsoPackets*usbip.IsoPacketDescriptorSize)
	var responseScratch []byte
	var unlinkScratch []byte
	var inputReportScratch [maximumVersionedInputReportSize]byte
	var header [urbHdrSize]byte

	for {
		if err := usbip.ReadExactly(conn, header[:]); err != nil {
			if failure := schedulers.failure(); failure != nil {
				return failure
			}
			if deviceContext.Err() != nil {
				s.scheduleEmptyBusCleanup(owningBus.BusID(), owningBus)
				return nil
			}
			return fmt.Errorf("read URB header: %w", err)
		}

		command := binary.BigEndian.Uint32(
			header[urbHdrOffsetCommand : urbHdrOffsetCommand+4],
		)
		seq := binary.BigEndian.Uint32(header[urbHdrOffsetSeqnum : urbHdrOffsetSeqnum+4])
		dir := binary.BigEndian.Uint32(header[urbHdrOffsetDir : urbHdrOffsetDir+4])
		ep := binary.BigEndian.Uint32(header[urbHdrOffsetEp : urbHdrOffsetEp+4])
		if dir != usbip.DirOut && dir != usbip.DirIn {
			return fmt.Errorf("invalid URB direction %d (seq=%d)", dir, seq)
		}
		if ep > 15 {
			return fmt.Errorf("invalid URB endpoint %d (seq=%d)", ep, seq)
		}

		if command == usbip.CmdUnlinkCode {
			unlinkSeq := binary.BigEndian.Uint32(
				header[urbHdrOffsetUnlink : urbHdrOffsetUnlink+4],
			)
			removed := schedulers.unlink(unlinkSeq)
			status := int32(0)
			if removed {
				status = errConnReset
			}
			unlinkScratch = buildRetUnlinkPacket(unlinkScratch, seq, status)
			if err := responses.write(unlinkScratch, true, time.Now()); err != nil {
				return err
			}
			continue
		}
		if command != usbip.CmdSubmitCode {
			devid := binary.BigEndian.Uint32(
				header[urbHdrOffsetDevid : urbHdrOffsetDevid+4],
			)
			return fmt.Errorf("unsupported cmd %d (seq=%d, devid=%d)", command, seq, devid)
		}

		xferLen := binary.BigEndian.Uint32(header[urbHdrOffsetLength : urbHdrOffsetLength+4])
		if xferLen > maximumTransferSize {
			return fmt.Errorf("transfer length %d exceeds limit %d", xferLen, maximumTransferSize)
		}
		packetCountWire := int32(binary.BigEndian.Uint32(
			header[urbHdrOffsetPackets : urbHdrOffsetPackets+4],
		))
		if packetCountWire < -1 || packetCountWire > maxIsoPackets {
			return fmt.Errorf("invalid ISO packet count %d", packetCountWire)
		}

		var activeBinding endpointDescriptorBinding
		endpointActive := true
		if ep != 0 {
			activeBinding, endpointActive = s.activeEndpointBinding(dev, ep, dir)
		}
		descriptorIso := endpointActive && activeBinding.descriptor != nil &&
			activeBinding.descriptor.BMAttributes&0x03 == 0x01
		// Some usbip-win clients encode ordinary control/bulk submissions with
		// NumberOfPackets == 0 instead of the documented -1 marker. A positive
		// count still asserts ISO, while the endpoint descriptor remains
		// authoritative for zero-descriptor ISO requests.
		isIso := packetCountWire > 0 || descriptorIso
		setup := header[urbHdrOffsetSetup:urbHdrSize]

		var outPayload []byte
		if dir == usbip.DirOut && xferLen > 0 {
			outPayloadScratch = resizeBytes(outPayloadScratch, int(xferLen))
			outPayload = outPayloadScratch
			if err := usbip.ReadExactly(conn, outPayload); err != nil {
				return fmt.Errorf("read OUT payload: %w", err)
			}
		}

		var isoPackets []usbip.IsoPacketDescriptor
		if isIso && packetCountWire > 0 {
			isoPackets = isoPacketScratch[:packetCountWire]
			if err := readIsoPacketDescriptors(
				conn, isoPacketWireScratch, isoPackets,
			); err != nil {
				return fmt.Errorf("read/decode %d ISO packet descriptors: %w",
					packetCountWire, err)
			}
		}

		// Consume the complete USB/IP submission before rejecting it so the
		// connection remains framed. Endpoint zero cannot be isochronous, and a
		// non-control endpoint must belong to the selected configuration/alt.
		rejectEndpoint := (ep != 0 && !endpointActive) ||
			(isIso && (ep == 0 || activeBinding.descriptor == nil ||
				activeBinding.descriptor.BMAttributes&0x03 != 0x01))
		if rejectEndpoint {
			for index := range isoPackets {
				isoPackets[index].ActualLength = 0
				isoPackets[index].Status = errPipe
			}
			responseScratch = buildRetSubmitPacket(
				responseScratch, seq, errPipe, 0, nil, isoPackets, isIso,
			)
			if err := responses.write(responseScratch, true, time.Now()); err != nil {
				return err
			}
			continue
		}

		if isIso {
			if err := validateIsoEndpointSubmission(
				activeBinding.descriptor, xferLen, isoPackets,
			); err != nil {
				return fmt.Errorf("invalid ISO submission seq %d: %w", seq, err)
			}
		}

		if dir == usbip.DirIn && ep != 0 {
			if isIso {
				// An ISO request without packet descriptors has no service slots.
				// Complete it empty without touching the microphone source.
				if len(isoPackets) == 0 {
					responseScratch = buildRetSubmitPacket(
						responseScratch, seq, 0, 0, nil, nil, true,
					)
					if err := responses.write(responseScratch, true, time.Now()); err != nil {
						return err
					}
					continue
				}
				if schedulers.enqueueIsoInBinding(
					seq, xferLen, isoPackets, activeBinding,
				) {
					continue
				}
			} else if activeBinding.descriptor.BMAttributes&0x03 == 0x03 {
				if err := validateInterruptEndpointSubmission(
					activeBinding.descriptor, xferLen,
				); err != nil {
					return fmt.Errorf("invalid interrupt submission seq %d: %w", seq, err)
				}
				if schedulers.enqueueInterruptInBinding(
					seq, xferLen, activeBinding,
				) {
					continue
				}
			} else if schedulers.enqueueGenericInBinding(
				seq, xferLen, activeBinding,
			) {
				continue
			}

			responseScratch = buildRetSubmitPacket(
				responseScratch, seq, errNoSpace, 0, nil, isoPackets, isIso,
			)
			if err := responses.write(responseScratch, true, time.Now()); err != nil {
				return err
			}
			continue
		}

		if dir == usbip.DirOut && isIso {
			if schedulers.enqueueIsoOutBinding(
				seq, xferLen, outPayload, isoPackets, activeBinding,
			) {
				continue
			}
			for index := range isoPackets {
				isoPackets[index].ActualLength = 0
				isoPackets[index].Status = errNoSpace
			}
			responseScratch = buildRetSubmitPacket(
				responseScratch, seq, errNoSpace, 0, nil, isoPackets, true,
			)
			if err := responses.write(responseScratch, true, time.Now()); err != nil {
				return err
			}
			continue
		}

		if ep == 0 {
			var handled bool
			var err error
			responseScratch, handled, err = s.processTransactionalControlSubmission(
				schedulers, responses, dev, responseScratch, seq, dir, xferLen,
				setup, outPayload,
			)
			if err != nil {
				return err
			}
			if handled {
				continue
			}
		}

		if ep != 0 && dir == usbip.DirOut && !isIso &&
			activeBinding.descriptor.BMAttributes&0x03 == 0x03 {
			var handled bool
			var err error
			responseScratch, handled, err = processTransactionalInterruptOutSubmission(
				responses, dev, responseScratch, seq, ep, outPayload,
			)
			if err != nil {
				return err
			}
			if handled {
				continue
			}
		}

		snapshotter, reportLength, reportDisposition :=
			versionedInputReportRequest(dev, ep, dir, setup, xferLen)
		switch reportDisposition {
		case versionedInputReportSnapshot:
			var err error
			responseScratch, err = writeVersionedInputReportResponse(
				responses, snapshotter, responseScratch, inputReportScratch[:],
				seq, reportLength,
			)
			if err != nil {
				return err
			}
			continue
		case versionedInputReportStall:
			responseScratch = buildRetSubmitPacket(
				responseScratch, seq, errPipe, 0, nil, nil, false,
			)
			if err := responses.write(responseScratch, true, time.Now()); err != nil {
				return err
			}
			continue
		}

		// Publish the endpoint generation boundary before the device performs its
		// reset. A due old-generation media callback can then either finish before
		// the device reset (and be drained by it) or observe the new device token;
		// it cannot be admitted after the reset under an old scheduler generation.
		lifecycle := s.parseControlLifecycleSubmission(
			dev, ep, dir, xferLen, setup,
		)
		applyControlLifecycleToSchedulers(schedulers, lifecycle)
		status := int32(0)
		if lifecycle.recognized && !lifecycle.accepted {
			status = errPipe
		}
		responseData := s.processSubmitWithLifecycle(
			deviceContext, dev, ep, dir, setup, outPayload, lifecycle,
		)
		actualLength := uint32(0)
		if status == 0 {
			actualLength = uint32(len(responseData))
			if dir == usbip.DirOut {
				actualLength = uint32(len(outPayload))
				// An OUT transfer has no device-to-host data stage. A
				// controller implementation cannot append return bytes to the
				// USB/IP stream merely by returning a non-empty slice.
				responseData = nil
			}
		} else {
			responseData = nil
		}
		flush := ep == 0 || activeBinding.descriptor != nil &&
			activeBinding.descriptor.BMAttributes&0x03 == 0x03
		responseScratch = buildRetSubmitPacket(
			responseScratch, seq, status, actualLength, responseData, nil,
			false,
		)
		if err := responses.write(responseScratch, flush, time.Now()); err != nil {
			return err
		}
	}
}

// processTransactionalInterruptOutSubmission gives an opted-in device
// complete ownership of one active interrupt OUT submission before legacy
// HandleTransfer can observe the payload. Final admission and exactly-once
// completion are serialized with RET_SUBMIT delivery, so an Accepted command
// cannot become visible unless its acknowledgement was delivered.
func processTransactionalInterruptOutSubmission(
	responses *responseWriter,
	dev usb.Device,
	responseBuffer []byte,
	seq, endpoint uint32,
	outPayload []byte,
) ([]byte, bool, error) {
	owner, ok := dev.(usb.TransactionalInterruptOutDevice)
	if !ok {
		return responseBuffer, false, nil
	}

	request := usb.InterruptOutTransactionRequest{
		Endpoint: uint8(endpoint),
		Data:     outPayload[:len(outPayload):len(outPayload)],
	}
	claim, claimErr := owner.ClaimInterruptOutTransaction(request)
	if claimErr != nil {
		completionErr := cancelReturnedInterruptOutClaim(owner, claim)
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional interrupt OUT seq %d claim: %v; cancel: %w",
				seq, claimErr, completionErr)
		}
		return responseBuffer, true, fmt.Errorf(
			"transactional interrupt OUT seq %d claim: %w", seq, claimErr)
	}
	if !claim.Valid() {
		completionErr := cancelReturnedInterruptOutClaim(owner, claim)
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional interrupt OUT seq %d invalid claim %+v; cancel: %w",
				seq, claim, completionErr)
		}
		return responseBuffer, true, fmt.Errorf(
			"transactional interrupt OUT seq %d invalid claim %+v", seq, claim)
	}
	if claim.Result == usb.InterruptOutTransactionUnhandled {
		return responseBuffer, false, nil
	}

	status := int32(0)
	actualLength := uint32(len(outPayload))
	if claim.Result == usb.InterruptOutTransactionStall {
		status = errPipe
		actualLength = 0
	}
	responseBuffer = buildRetSubmitPacket(
		responseBuffer, seq, status, actualLength, nil, nil, false,
	)

	admitted := false
	var admissionErr error
	var completionErr error
	written, writeErr := responses.writeIfThen(
		responseBuffer, true, time.Now(),
		func() bool {
			admissionErr = owner.AdmitInterruptOutTransaction(claim)
			admitted = admissionErr == nil
			return admitted
		},
		func(success bool) {
			outcome := usb.InterruptOutTransactionCancelled
			if admitted {
				outcome = usb.InterruptOutTransactionDeliveryFailed
				if success {
					outcome = usb.InterruptOutTransactionDelivered
				}
			}
			completionErr = owner.CompleteInterruptOutTransaction(claim, outcome)
		},
	)
	if admissionErr != nil {
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional interrupt OUT seq %d admission: %v; cancel: %w",
				seq, admissionErr, completionErr)
		}
		return responseBuffer, true, fmt.Errorf(
			"transactional interrupt OUT seq %d admission: %w", seq, admissionErr)
	}
	if !written {
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional interrupt OUT seq %d was not admitted; cancel: %w",
				seq, completionErr)
		}
		return responseBuffer, true, fmt.Errorf(
			"transactional interrupt OUT seq %d was not admitted", seq)
	}
	if writeErr != nil {
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional interrupt OUT seq %d delivery: %v; completion: %w",
				seq, writeErr, completionErr)
		}
		return responseBuffer, true, writeErr
	}
	if completionErr != nil {
		return responseBuffer, true, fmt.Errorf(
			"transactional interrupt OUT seq %d completion: %w", seq, completionErr)
	}
	return responseBuffer, true, nil
}

func cancelReturnedInterruptOutClaim(
	owner usb.TransactionalInterruptOutDevice,
	claim usb.InterruptOutTransactionClaim,
) error {
	if claim == (usb.InterruptOutTransactionClaim{}) {
		return nil
	}
	return owner.CompleteInterruptOutTransaction(
		claim, usb.InterruptOutTransactionCancelled)
}

// processTransactionalControlSubmission gives an opted-in device complete EP0
// ownership before any generic server policy. The response image is reserved
// before send ownership, but a Data payload remains zeroed until device final
// admission runs under the response writer lock immediately before writeFull.
// Existing devices never enter this function beyond the interface assertion.
func (s *Server) processTransactionalControlSubmission(
	schedulers *endpointSchedulers,
	responses *responseWriter,
	dev usb.Device,
	responseBuffer []byte,
	seq, dir, transferLength uint32,
	setup []byte,
	outPayload []byte,
) ([]byte, bool, error) {
	owner, ok := dev.(usb.TransactionalControlDevice)
	if !ok {
		return responseBuffer, false, nil
	}
	if len(setup) != 8 {
		return responseBuffer, true, fmt.Errorf(
			"transactional EP0 seq %d: setup length %d", seq, len(setup))
	}

	request := usb.ControlTransactionRequest{
		TransferLength: transferLength,
		Data:           outPayload[:len(outPayload):len(outPayload)],
	}
	copy(request.Setup[:], setup)
	switch dir {
	case usbip.DirOut:
		request.Direction = usb.ControlTransactionHostToDevice
	case usbip.DirIn:
		request.Direction = usb.ControlTransactionDeviceToHost
	default:
		return responseBuffer, true, fmt.Errorf(
			"transactional EP0 seq %d: invalid direction %d", seq, dir)
	}

	claim, claimErr := owner.ClaimControlTransaction(request)
	if claimErr != nil {
		completionErr := cancelReturnedControlClaim(owner, claim)
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional EP0 seq %d claim: %v; cancel: %w",
				seq, claimErr, completionErr)
		}
		return responseBuffer, true, fmt.Errorf(
			"transactional EP0 seq %d claim: %w", seq, claimErr)
	}
	if !claim.Valid() {
		completionErr := cancelReturnedControlClaim(owner, claim)
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional EP0 seq %d invalid claim %+v; cancel: %w",
				seq, claim, completionErr)
		}
		return responseBuffer, true, fmt.Errorf(
			"transactional EP0 seq %d invalid claim %+v", seq, claim)
	}
	if claim.Result == usb.ControlTransactionUnhandled {
		return responseBuffer, false, nil
	}
	lifecycle := s.parseControlLifecycleSubmission(
		dev, 0, dir, transferLength, setup)
	if lifecycle.recognized {
		validLifecycleResult := claim.Result == usb.ControlTransactionStall ||
			lifecycle.accepted &&
				claim.Result == usb.ControlTransactionNoData
		if !validLifecycleResult {
			completionErr := owner.CompleteControlTransaction(
				claim, usb.ControlTransactionCancelled)
			if completionErr != nil {
				return responseBuffer, true, fmt.Errorf(
					"transactional EP0 seq %d lifecycle/result mismatch %+v; cancel: %w",
					seq, claim, completionErr)
			}
			return responseBuffer, true, fmt.Errorf(
				"transactional EP0 seq %d lifecycle/result mismatch %+v",
				seq, claim)
		}
	}

	setupResponseLength := uint32(binary.LittleEndian.Uint16(setup[6:8]))
	invalidEnvelope := claim.ResponseLength > transferLength ||
		claim.ResponseLength > setupResponseLength ||
		claim.Result == usb.ControlTransactionData && dir != usbip.DirIn
	if invalidEnvelope {
		completionErr := owner.CompleteControlTransaction(
			claim, usb.ControlTransactionCancelled)
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional EP0 seq %d invalid result envelope %+v; cancel: %w",
				seq, claim, completionErr)
		}
		return responseBuffer, true, fmt.Errorf(
			"transactional EP0 seq %d invalid result envelope %+v",
			seq, claim)
	}

	status := int32(0)
	responseLength := uint32(0)
	actualLength := uint32(0)
	switch claim.Result {
	case usb.ControlTransactionData:
		responseLength = claim.ResponseLength
		actualLength = claim.ResponseLength
	case usb.ControlTransactionNoData:
		if dir == usbip.DirOut {
			actualLength = uint32(len(outPayload))
		}
	case usb.ControlTransactionStall:
		status = errPipe
	}
	responseBuffer = buildTransactionalControlResponse(
		responseBuffer, seq, status, actualLength, responseLength)
	destination := responseBuffer[retSubmitHeaderSize:len(responseBuffer):len(responseBuffer)]
	admitted := false
	var admissionErr error
	var completionErr error
	written, writeErr := responses.writeIfThen(
		responseBuffer, true, time.Now(),
		func() bool {
			admissionErr = owner.AdmitControlTransaction(claim, destination)
			admitted = admissionErr == nil
			return admitted
		},
		func(success bool) {
			outcome := usb.ControlTransactionCancelled
			if admitted {
				outcome = usb.ControlTransactionDeliveryFailed
				if success {
					outcome = usb.ControlTransactionDelivered
				}
			}
			completionErr = owner.CompleteControlTransaction(claim, outcome)
			if success && completionErr == nil && lifecycle.accepted &&
				claim.Result == usb.ControlTransactionNoData {
				s.applyDeliveredTransactionalControlLifecycle(
					schedulers, dev, lifecycle)
			}
		},
	)
	if admissionErr != nil {
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional EP0 seq %d admission: %v; cancel: %w",
				seq, admissionErr, completionErr)
		}
		return responseBuffer, true, fmt.Errorf(
			"transactional EP0 seq %d admission: %w", seq, admissionErr)
	}
	if !written {
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional EP0 seq %d was not admitted; cancel: %w",
				seq, completionErr)
		}
		return responseBuffer, true, fmt.Errorf(
			"transactional EP0 seq %d was not admitted", seq)
	}
	if writeErr != nil {
		if completionErr != nil {
			return responseBuffer, true, fmt.Errorf(
				"transactional EP0 seq %d delivery: %v; completion: %w",
				seq, writeErr, completionErr)
		}
		return responseBuffer, true, writeErr
	}
	if completionErr != nil {
		return responseBuffer, true, fmt.Errorf(
			"transactional EP0 seq %d completion: %w", seq, completionErr)
	}
	return responseBuffer, true, nil
}

// applyDeliveredTransactionalControlLifecycle updates only transport-owned
// binding and worker state. The transactional device already committed its
// own request effect in CompleteControlTransaction, so legacy device reset and
// alternate-setting callbacks must not be invoked a second time.
func (s *Server) applyDeliveredTransactionalControlLifecycle(
	schedulers *endpointSchedulers,
	dev usb.Device,
	lifecycle controlLifecycleSetup,
) {
	applyControlLifecycleToSchedulers(schedulers, lifecycle)
	switch lifecycle.kind {
	case controlLifecycleClearEndpointHalt:
		// The server has no separate Halt bit; worker retirement is sufficient.
	case controlLifecycleSetInterface:
		s.setInterfaceAlt(dev, lifecycle.interfaceNumber,
			lifecycle.alternateSetting)
	case controlLifecycleSetConfiguration:
		s.setDeviceConfiguration(dev, lifecycle.configurationValue)
		// The transactional owner has already applied the device effect. Clear
		// only server routing state and do not duplicate legacy notifications.
		s.clearInterfaceAlt(dev)
	}
}

// cancelReturnedControlClaim best-effort retires a claim returned alongside a
// device error or with an invalid structural shape. The all-zero unhandled
// value acquired no capability and therefore has no terminal callback.
func cancelReturnedControlClaim(
	owner usb.TransactionalControlDevice,
	claim usb.ControlTransactionClaim,
) error {
	if claim == (usb.ControlTransactionClaim{}) {
		return nil
	}
	return owner.CompleteControlTransaction(
		claim, usb.ControlTransactionCancelled)
}

func buildTransactionalControlResponse(
	destination []byte,
	seq uint32,
	status int32,
	actualLength, responseLength uint32,
) []byte {
	total := retSubmitHeaderSize + int(responseLength)
	if cap(destination) < total {
		destination = make([]byte, total)
	} else {
		destination = destination[:total]
		clear(destination)
	}
	binary.BigEndian.PutUint32(destination[0:4], usbip.RetSubmitCode)
	binary.BigEndian.PutUint32(destination[4:8], seq)
	binary.BigEndian.PutUint32(destination[20:24], uint32(status))
	binary.BigEndian.PutUint32(destination[24:28], actualLength)
	binary.BigEndian.PutUint32(destination[32:36], ^uint32(0))
	return destination
}

func readIsoPacketDescriptors(
	reader io.Reader,
	wireScratch []byte,
	packets []usbip.IsoPacketDescriptor,
) error {
	if len(packets) > len(wireScratch)/usbip.IsoPacketDescriptorSize {
		return fmt.Errorf("descriptor scratch length %d cannot hold packet count %d",
			len(wireScratch), len(packets))
	}
	wire := wireScratch[:len(packets)*usbip.IsoPacketDescriptorSize]
	if _, err := io.ReadFull(reader, wire); err != nil {
		return err
	}
	return decodeIsoPacketDescriptors(wire, packets)
}

func decodeIsoPacketDescriptors(
	wire []byte,
	packets []usbip.IsoPacketDescriptor,
) error {
	if len(packets) > len(wire)/usbip.IsoPacketDescriptorSize ||
		len(wire) != len(packets)*usbip.IsoPacketDescriptorSize {
		return fmt.Errorf("descriptor wire length %d does not match packet count %d",
			len(wire), len(packets))
	}
	for index := range packets {
		offset := index * usbip.IsoPacketDescriptorSize
		if err := packets[index].Decode(
			wire[offset : offset+usbip.IsoPacketDescriptorSize],
		); err != nil {
			return err
		}
	}
	return nil
}

type versionedInputReportDisposition uint8

const (
	versionedInputReportUnhandled versionedInputReportDisposition = iota
	versionedInputReportSnapshot
	versionedInputReportStall
)

func versionedInputReportRequest(
	dev usb.Device,
	ep, dir uint32,
	setup []byte,
	xferLen uint32,
) (inputReportSnapshotter, int, versionedInputReportDisposition) {
	if ep != 0 || dir != usbip.DirIn || len(setup) != 8 ||
		setup[0] != hidReqTypeIn || setup[1] != hidReqGetReport {
		return nil, 0, versionedInputReportUnhandled
	}
	wValue := binary.LittleEndian.Uint16(setup[2:4])
	const hidInputReportType = 0x01
	if uint8(wValue>>8) != hidInputReportType {
		return nil, 0, versionedInputReportUnhandled
	}
	snapshotter, ok := dev.(inputReportSnapshotter)
	if !ok {
		return nil, 0, versionedInputReportUnhandled
	}
	reportID := uint8(wValue)
	if byID, supportsID := dev.(inputReportIDSnapshotter); supportsID {
		if !byID.SupportsInputReportSnapshot(reportID) {
			// An ID-aware device owns the complete input-report ID namespace it
			// advertises through this contract. Falling through would turn its
			// explicit rejection into a successful zero-length control response.
			return nil, 0, versionedInputReportStall
		}
		snapshotter = inputReportIDSnapshotRequest{
			inputReportIDSnapshotter: byID,
			reportID:                 reportID,
		}
	} else if reportID != 0x01 {
		// Preserve legacy device HandleControl semantics for report IDs which
		// do not opt into the versioned snapshot contract.
		return nil, 0, versionedInputReportUnhandled
	}

	// HID class GET_REPORT is interface-recipient scoped. Once a report ID is
	// owned by the versioned contract, an invalid or non-HID target is a request
	// to the wrong control recipient and must STALL rather than bypass ownership
	// through the generic device control fallback.
	wIndex := binary.LittleEndian.Uint16(setup[4:6])
	descriptor := dev.GetDescriptor()
	if wIndex>>8 != 0 || descriptor == nil {
		return nil, 0, versionedInputReportStall
	}
	iface, exists := descriptor.Interface(uint8(wIndex))
	if !exists || iface.Descriptor.BInterfaceClass != usbInterfaceClassHID {
		return nil, 0, versionedInputReportStall
	}
	wLength := int(binary.LittleEndian.Uint16(setup[6:8]))
	reportLength := min(int(xferLen), wLength, maximumVersionedInputReportSize)
	if reportLength < 0 {
		reportLength = 0
	}
	return snapshotter, reportLength, versionedInputReportSnapshot
}

type inputReportIDSnapshotRequest struct {
	inputReportIDSnapshotter
	reportID uint8
}

func (request inputReportIDSnapshotRequest) SnapshotInputReportInto(
	destination []byte,
) (int, uint64) {
	return request.SnapshotInputReportForIDInto(request.reportID, destination)
}

// writeVersionedInputReportResponse builds the complete EP0 response before
// acquiring send ownership, then validates that no newer interrupt report was
// presented in the meantime. A stale snapshot is discarded and rebuilt. The
// input lock is held only by the two device methods; it is never held during
// response I/O.
func writeVersionedInputReportResponse(
	responses *responseWriter,
	snapshotter inputReportSnapshotter,
	responseBuffer []byte,
	reportBuffer []byte,
	seq uint32,
	reportLength int,
) ([]byte, error) {
	if reportLength < 0 {
		reportLength = 0
	}
	reportLength = min(reportLength, len(reportBuffer))
	for {
		n, version := snapshotter.SnapshotInputReportInto(
			reportBuffer[:reportLength],
		)
		if n < 0 {
			n = 0
		}
		n = min(n, reportLength)
		responseBuffer = buildRetSubmitPacket(
			responseBuffer, seq, 0, uint32(n), reportBuffer[:n], nil, false,
		)
		written, err := responses.writeIf(
			responseBuffer, true, time.Now(),
			func() bool { return snapshotter.InputReportSnapshotCurrent(version) },
		)
		if err != nil {
			return responseBuffer, err
		}
		if written {
			return responseBuffer, nil
		}
	}
}
