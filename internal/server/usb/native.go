package usb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
)

type nativeLaneKey struct {
	deviceID   uint64
	generation uint32
	endpoint   uint8
	attributes uint8
	interval   uint8
	maxPacket  uint16
}

type nativeSessionKey struct {
	deviceID   uint64
	generation uint32
}

type nativeEndpointSignature struct {
	address    uint8
	attributes uint8
	interval   uint8
	maxPacket  uint16
}

type nativeSessionState struct {
	mu     sync.Mutex
	active map[nativeEndpointSignature]struct{}
}

type nativeClockSample struct {
	now   time.Time
	frame uint32
}

type nativeIsoEndpoint struct {
	number    uint32
	direction uint32
	interval  time.Duration
	key       nativeLaneKey
}

const (
	// USBD_ISO_START_FRAME_RANGE from usb.h.
	usbdIsoStartFrameRange = int64(1024)
)

// NativeProcessor adapts the native UdeCx broker to the same control and
// transfer engine used by USB/IP. Transport-specific clocks live here; device
// state, feedback, HID, audio, and descriptor behavior remain in usb.Device.
type NativeProcessor struct {
	server   *Server
	mu       sync.Mutex
	next     map[nativeLaneKey]time.Time
	lastIn   map[nativeLaneKey][]byte
	sessions map[nativeSessionKey]*nativeSessionState
	clock    func() nativeClockSample
	wait     func(context.Context, time.Time) bool
}

func NewNativeProcessor(server *Server) (*NativeProcessor, error) {
	if server == nil {
		return nil, errors.New("native UDE processor requires a USB server engine")
	}
	return &NativeProcessor{
		server:   server,
		next:     make(map[nativeLaneKey]time.Time),
		lastIn:   make(map[nativeLaneKey][]byte),
		sessions: make(map[nativeSessionKey]*nativeSessionState),
		clock:    nativeClockSnapshot,
		wait:     waitUntilContext,
	}, nil
}

func (p *NativeProcessor) Reset(dev usbdevice.Device, identity udecx.DeviceIdentity) {
	key := nativeSessionKey{deviceID: identity.DeviceID, generation: identity.Generation}
	session := p.lockSession(key)
	p.resetDeviceLocked(dev, identity, session)
	p.mu.Lock()
	if p.sessions[key] == session {
		delete(p.sessions, key)
	}
	p.mu.Unlock()
	session.mu.Unlock()
}

func (p *NativeProcessor) lockSession(key nativeSessionKey) *nativeSessionState {
	for {
		p.mu.Lock()
		session := p.sessions[key]
		if session == nil {
			session = &nativeSessionState{active: make(map[nativeEndpointSignature]struct{})}
			p.sessions[key] = session
		}
		p.mu.Unlock()

		session.mu.Lock()
		p.mu.Lock()
		current := p.sessions[key]
		p.mu.Unlock()
		if current == session {
			return session
		}
		// Reset retired this state while this goroutine waited. Retry against
		// the current generation-owned state rather than mutating an orphan.
		session.mu.Unlock()
	}
}

func (p *NativeProcessor) resetDeviceLocked(dev usbdevice.Device, identity udecx.DeviceIdentity,
	session *nativeSessionState) {
	p.server.resetInterfaceAlts(dev)
	p.clearDeviceTransportLocked(identity, session)
}

func (p *NativeProcessor) clearDeviceTransportLocked(identity udecx.DeviceIdentity,
	session *nativeSessionState) {
	p.mu.Lock()
	for key := range p.next {
		if key.deviceID == identity.DeviceID && key.generation == identity.Generation {
			delete(p.next, key)
			delete(p.lastIn, key)
		}
	}
	for key := range p.lastIn {
		if key.deviceID == identity.DeviceID && key.generation == identity.Generation {
			delete(p.lastIn, key)
		}
	}
	p.mu.Unlock()
	clear(session.active)
}

func (p *NativeProcessor) Lifecycle(_ context.Context, dev usbdevice.Device, op udecx.Operation) error {
	identity := udecx.DeviceIdentity{DeviceID: op.DeviceID, Generation: op.Generation}
	sessionKey := nativeSessionKey{deviceID: op.DeviceID, generation: op.Generation}
	session := p.lockSession(sessionKey)
	defer session.mu.Unlock()
	key := nativeLaneKey{
		deviceID: op.DeviceID, generation: op.Generation, endpoint: op.EndpointAddress,
		attributes: op.EndpointAttributes, interval: op.EndpointInterval,
		maxPacket: op.EndpointMaxPacketSize,
	}

	switch op.Kind {
	case udecx.OperationEndpointStart:
		p.clearEndpointLanes(key)
		p.activateEndpointLocked(dev, op, session)
	case udecx.OperationEndpointPurge:
		p.clearEndpointLanes(key)
		// Closing the last endpoint of an alternate setting already establishes
		// the controller's media-generation boundary through
		// SetInterfaceAltSetting(0). Reset the individual pipe only when the
		// interface stays active (or the endpoint cannot be mapped). Otherwise a
		// native purge would flush the PlayStation stream twice while USB/IP
		// closes it once.
		resetByInterfaceClose := p.deactivateEndpointLocked(dev, op, session)
		if resetter, ok := dev.(usbdevice.EndpointResetDevice); ok && !resetByInterfaceClose {
			resetter.ResetEndpoint(op.EndpointAddress)
		}
	case udecx.OperationEndpointReset:
		p.clearEndpointLanes(key)
		if resetter, ok := dev.(usbdevice.EndpointResetDevice); ok {
			resetter.ResetEndpoint(op.EndpointAddress)
		}
	case udecx.OperationDeviceReset:
		p.resetDeviceLocked(dev, identity, session)
	case udecx.OperationDeviceD0Entry, udecx.OperationDeviceD0Exit:
		// A link-power transition is not a USB reset. Preserve the selected
		// audio interfaces and controller state, but discard stale service-clock
		// anchors so the first resumed transfer starts from the current time.
		p.clearDeviceLanes(identity)
	case udecx.OperationSetInterface:
		// UdeCx is documented by usbip-win2 0.9.7.8 to return incorrect
		// interface/alternate values for some composite devices. Treat this as
		// a hint only. Interfaces with endpoint-bearing alternate settings are
		// driven by the exact endpoint descriptors carried by start/purge and
		// transfer operations instead.
		p.applyInterfaceHintLocked(dev, op)
	default:
		return fmt.Errorf("unsupported native UDE lifecycle operation %d", op.Kind)
	}
	return nil
}

func signatureFromOperation(op udecx.Operation) nativeEndpointSignature {
	return nativeEndpointSignature{
		address: op.EndpointAddress, attributes: op.EndpointAttributes,
		interval: op.EndpointInterval, maxPacket: op.EndpointMaxPacketSize,
	}
}

func signatureFromDescriptor(endpoint usbdevice.EndpointDescriptor) nativeEndpointSignature {
	return nativeEndpointSignature{
		address: endpoint.BEndpointAddress, attributes: endpoint.BMAttributes,
		interval: endpoint.BInterval, maxPacket: endpoint.WMaxPacketSize,
	}
}

func nativeSignatureFromDescriptor(speed uint32,
	endpoint usbdevice.EndpointDescriptor) (nativeEndpointSignature, bool) {
	projected, err := udecx.EndpointDescriptorForNativeUdeCx(
		udecx.DeviceSpeed(speed), endpoint)
	if err != nil {
		return nativeEndpointSignature{}, false
	}
	return signatureFromDescriptor(projected), true
}

func descriptorInterfaceAltForEndpoint(desc *usbdevice.Descriptor,
	signature nativeEndpointSignature) (uint8, uint8, bool) {
	if desc == nil || signature.address == 0 {
		return 0, 0, false
	}
	var interfaceNumber, alternateSetting uint8
	found := false
	for _, iface := range desc.Interfaces {
		if iface.Descriptor.BAlternateSetting == 0 {
			continue
		}
		for _, endpoint := range iface.Endpoints {
			projected, valid := nativeSignatureFromDescriptor(desc.Device.Speed, endpoint)
			if !valid || projected != signature {
				continue
			}
			candidateInterface := iface.Descriptor.BInterfaceNumber
			candidateAlt := iface.Descriptor.BAlternateSetting
			if found && (candidateInterface != interfaceNumber || candidateAlt != alternateSetting) {
				return 0, 0, false
			}
			interfaceNumber, alternateSetting, found = candidateInterface, candidateAlt, true
		}
	}
	return interfaceNumber, alternateSetting, found
}

func descriptorInterfaceUsesEndpointLifecycle(desc *usbdevice.Descriptor, interfaceNumber uint8) bool {
	if desc == nil {
		return false
	}
	for _, iface := range desc.Interfaces {
		if iface.Descriptor.BInterfaceNumber == interfaceNumber &&
			iface.Descriptor.BAlternateSetting != 0 && len(iface.Endpoints) != 0 {
			return true
		}
	}
	return false
}

func descriptorInterfaceAltIsActive(desc *usbdevice.Descriptor, interfaceNumber, alternateSetting uint8,
	active map[nativeEndpointSignature]struct{}) bool {
	if desc == nil {
		return false
	}
	for _, iface := range desc.Interfaces {
		if iface.Descriptor.BInterfaceNumber != interfaceNumber ||
			iface.Descriptor.BAlternateSetting != alternateSetting {
			continue
		}
		for _, endpoint := range iface.Endpoints {
			projected, valid := nativeSignatureFromDescriptor(desc.Device.Speed, endpoint)
			if valid {
				if _, ok := active[projected]; ok {
					return true
				}
			}
		}
	}
	return false
}

func (p *NativeProcessor) activateEndpoint(dev usbdevice.Device, op udecx.Operation) {
	key := nativeSessionKey{deviceID: op.DeviceID, generation: op.Generation}
	session := p.lockSession(key)
	defer session.mu.Unlock()
	p.activateEndpointLocked(dev, op, session)
}

func (p *NativeProcessor) activateEndpointLocked(dev usbdevice.Device, op udecx.Operation,
	session *nativeSessionState) {
	signature := signatureFromOperation(op)
	interfaceNumber, alternateSetting, ok := descriptorInterfaceAltForEndpoint(
		dev.GetDescriptor(), signature)
	if !ok {
		return
	}
	session.active[signature] = struct{}{}
	if p.server.getInterfaceAlt(dev, interfaceNumber) != alternateSetting {
		p.server.setInterfaceAlt(dev, interfaceNumber, alternateSetting)
		p.server.notifyInterfaceAlt(dev, interfaceNumber, alternateSetting)
	}
}

func (p *NativeProcessor) deactivateEndpointLocked(dev usbdevice.Device, op udecx.Operation,
	session *nativeSessionState) bool {
	signature := signatureFromOperation(op)
	interfaceNumber, alternateSetting, ok := descriptorInterfaceAltForEndpoint(
		dev.GetDescriptor(), signature)
	if !ok {
		return false
	}
	delete(session.active, signature)
	if p.server.getInterfaceAlt(dev, interfaceNumber) == alternateSetting &&
		!descriptorInterfaceAltIsActive(dev.GetDescriptor(), interfaceNumber, alternateSetting, session.active) {
		p.server.setInterfaceAlt(dev, interfaceNumber, 0)
		p.server.notifyInterfaceAlt(dev, interfaceNumber, 0)
		return true
	}
	return false
}

func (p *NativeProcessor) applyInterfaceHintLocked(dev usbdevice.Device, op udecx.Operation) {
	desc := dev.GetDescriptor()
	if !descriptorHasInterfaceAlt(desc, op.InterfaceNumber, op.InterfaceSetting) ||
		descriptorInterfaceUsesEndpointLifecycle(desc, op.InterfaceNumber) {
		return
	}
	if p.server.getInterfaceAlt(dev, op.InterfaceNumber) == op.InterfaceSetting {
		return
	}
	p.server.setInterfaceAlt(dev, op.InterfaceNumber, op.InterfaceSetting)
	p.server.notifyInterfaceAlt(dev, op.InterfaceNumber, op.InterfaceSetting)
}

func (p *NativeProcessor) clearDeviceLanes(identity udecx.DeviceIdentity) {
	p.mu.Lock()
	for key := range p.next {
		if key.deviceID == identity.DeviceID && key.generation == identity.Generation {
			delete(p.next, key)
			delete(p.lastIn, key)
		}
	}
	for key := range p.lastIn {
		if key.deviceID == identity.DeviceID && key.generation == identity.Generation {
			delete(p.lastIn, key)
		}
	}
	p.mu.Unlock()
}

func (p *NativeProcessor) clearEndpointLanes(endpoint nativeLaneKey) {
	p.mu.Lock()
	for key := range p.next {
		if key.deviceID == endpoint.deviceID && key.generation == endpoint.generation &&
			key.endpoint == endpoint.endpoint {
			delete(p.next, key)
			delete(p.lastIn, key)
		}
	}
	for key := range p.lastIn {
		if key.deviceID == endpoint.deviceID && key.generation == endpoint.generation &&
			key.endpoint == endpoint.endpoint {
			delete(p.lastIn, key)
		}
	}
	p.mu.Unlock()
}

func nativeLaneKeyFromOperation(op udecx.Operation) nativeLaneKey {
	return nativeLaneKey{
		deviceID: op.DeviceID, generation: op.Generation, endpoint: op.EndpointAddress,
		attributes: op.EndpointAttributes, interval: op.EndpointInterval,
		maxPacket: op.EndpointMaxPacketSize,
	}
}

func logicalEndpointForNativeSignature(desc *usbdevice.Descriptor,
	signature nativeEndpointSignature) (usbdevice.EndpointDescriptor, bool) {
	if desc == nil {
		return usbdevice.EndpointDescriptor{}, false
	}
	for _, iface := range desc.Interfaces {
		for _, endpoint := range iface.Endpoints {
			projected, valid := nativeSignatureFromDescriptor(desc.Device.Speed, endpoint)
			if valid && projected == signature {
				return endpoint, true
			}
		}
	}
	return usbdevice.EndpointDescriptor{}, false
}

func nativeIsoServiceInterval(speed uint32, bInterval uint8) (time.Duration, error) {
	if bInterval == 0 {
		return 0, errors.New("native ISO endpoint has zero bInterval")
	}
	if speed == uint32(udecx.DeviceSpeedLow) {
		return 0, errors.New("low-speed USB does not support isochronous endpoints")
	}
	if speed >= uint32(udecx.DeviceSpeedHigh) {
		if bInterval > 16 {
			return 0, fmt.Errorf("native high-speed ISO bInterval %d exceeds 16", bInterval)
		}
		return time.Duration(1<<(bInterval-1)) * 125 * time.Microsecond, nil
	}
	return time.Duration(bInterval) * time.Millisecond, nil
}

func resolveNativeIsoEndpoint(dev usbdevice.Device, op udecx.Operation) (nativeIsoEndpoint, error) {
	desc := dev.GetDescriptor()
	if desc == nil {
		return nativeIsoEndpoint{}, errors.New("native ISO operation has no device descriptor")
	}
	signature := signatureFromOperation(op)
	if signature.address == 0 || signature.attributes&0x03 != 0x01 {
		return nativeIsoEndpoint{}, fmt.Errorf(
			"native ISO operation has invalid endpoint signature %+v", signature)
	}
	direction := uint8(0)
	usbDirection := usbdevice.DirectionOut
	if signature.address&0x80 != 0 {
		direction = 1
		usbDirection = usbdevice.DirectionIn
	}
	flagDirection := uint8(0)
	if op.TransferFlags&udecx.TransferFlagDirectionIn != 0 {
		flagDirection = 1
	}
	if op.Direction != direction || flagDirection != direction {
		return nativeIsoEndpoint{}, fmt.Errorf(
			"native ISO endpoint 0x%02x direction %d disagrees with operation %d/flags %d",
			signature.address, direction, op.Direction, flagDirection)
	}
	logicalEndpoint, ok := logicalEndpointForNativeSignature(desc, signature)
	if !ok {
		return nativeIsoEndpoint{}, fmt.Errorf(
			"native ISO endpoint signature %+v is not present in the device descriptor", signature)
	}
	interval, err := nativeIsoServiceInterval(desc.Device.Speed, logicalEndpoint.BInterval)
	if err != nil {
		return nativeIsoEndpoint{}, err
	}
	return nativeIsoEndpoint{
		number: uint32(signature.address & 0x0f), direction: usbDirection, interval: interval,
		key: nativeLaneKeyFromOperation(op),
	}, nil
}

func (p *NativeProcessor) Process(ctx context.Context, dev usbdevice.Device, op udecx.Operation) (udecx.Completion, error) {
	if dev == nil {
		return udecx.Completion{}, errors.New("native UDE operation has no device")
	}
	if op.TransferLength > udecx.MaxTransferBytes || len(op.Payload) > udecx.MaxTransferBytes {
		return udecx.Completion{}, udecx.ErrLimitExceeded
	}
	if len(op.IsoPackets) != 0 {
		if op.Kind != udecx.OperationTransfer {
			return udecx.Completion{}, fmt.Errorf(
				"native operation kind %d carries ISO packets", op.Kind)
		}
		for index, packet := range op.IsoPackets {
			if packet.Offset > op.TransferLength ||
				packet.Length > op.TransferLength-packet.Offset {
				return udecx.Completion{}, fmt.Errorf(
					"native ISO packet %d is outside transfer buffer", index)
			}
		}
		endpoint, err := resolveNativeIsoEndpoint(dev, op)
		if err != nil {
			return udecx.Completion{}, err
		}
		if endpoint.direction == usbdevice.DirectionIn {
			return p.processIsoIn(ctx, dev, op, endpoint)
		}
		return p.processIsoOut(ctx, dev, op, endpoint)
	}
	ep := uint32(op.EndpointAddress & 0x0f)
	dir := usbdevice.DirectionOut
	if op.Direction != 0 {
		dir = usbdevice.DirectionIn
	}
	key := nativeLaneKeyFromOperation(op)

	switch {
	case op.Kind == udecx.OperationControl:
		return p.processControl(ctx, dev, op, ep, dir)
	case dir == usbdevice.DirectionIn:
		return p.processInterruptIn(ctx, dev, op, ep, dir, key)
	default:
		p.server.processSubmit(ctx, dev, ep, dir, nil, op.Payload)
		return successCompletion(op, op.TransferLength, nil, nil), ctx.Err()
	}
}

func (p *NativeProcessor) processControl(ctx context.Context, dev usbdevice.Device,
	op udecx.Operation, ep, dir uint32) (udecx.Completion, error) {
	if op.SetupPacket[0] == usbReqTypeStandardToDevice && op.SetupPacket[1] == usbReqSetConfiguration {
		identity := udecx.DeviceIdentity{DeviceID: op.DeviceID, Generation: op.Generation}
		session := p.lockSession(nativeSessionKey{
			deviceID: op.DeviceID, generation: op.Generation,
		})
		// Server.processSubmit applies the USB request's interface reset and
		// publishes that notification exactly once. Retire the native endpoint
		// activity and media-clock state here after the host's device barrier has
		// joined every pre-configuration callback.
		p.clearDeviceTransportLocked(identity, session)
		session.mu.Unlock()
	}
	setup := op.SetupPacket[:]
	response := p.server.processSubmit(ctx, dev, ep, dir, setup, op.Payload)
	if err := ctx.Err(); err != nil {
		return udecx.Completion{}, err
	}
	if dir == usbdevice.DirectionOut {
		return successCompletion(op, op.TransferLength, nil, nil), nil
	}
	if uint32(len(response)) > op.TransferLength {
		response = response[:op.TransferLength]
	}
	return successCompletion(op, uint32(len(response)), response, nil), nil
}

func (p *NativeProcessor) processInterruptIn(ctx context.Context, dev usbdevice.Device,
	op udecx.Operation, ep, dir uint32, key nativeLaneKey) (udecx.Completion, error) {
	interval := endpointInterval(dev.GetDescriptor(), ep)
	if interval <= 0 {
		interval = time.Millisecond
	}

	for {
		serviceTime := p.reserveServiceTime(key, interval)
		if !waitUntilContext(ctx, serviceTime) {
			return udecx.Completion{}, ctx.Err()
		}
		attemptCtx, cancel := context.WithTimeout(ctx, interval)
		response := p.server.processSubmit(attemptCtx, dev, ep, dir, nil, nil)
		expired := len(response) == 0 && errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil {
			return udecx.Completion{}, ctx.Err()
		}
		if len(response) != 0 {
			if uint32(len(response)) > op.TransferLength {
				response = response[:op.TransferLength]
			}
			p.mu.Lock()
			p.lastIn[key] = append(p.lastIn[key][:0], response...)
			p.mu.Unlock()
			return successCompletion(op, uint32(len(response)), response, nil), nil
		}
		if expired {
			p.mu.Lock()
			cached := append([]byte(nil), p.lastIn[key]...)
			p.mu.Unlock()
			if len(cached) != 0 {
				if uint32(len(cached)) > op.TransferLength {
					cached = cached[:op.TransferLength]
				}
				return successCompletion(op, uint32(len(cached)), cached, nil), nil
			}
			continue
		}
		return successCompletion(op, 0, nil, nil), nil
	}
}

func (p *NativeProcessor) processIsoOut(ctx context.Context, dev usbdevice.Device,
	op udecx.Operation, endpoint nativeIsoEndpoint) (udecx.Completion, error) {
	duration := time.Duration(len(op.IsoPackets)) * endpoint.interval
	serviceStart, serviceEnd, err := p.reserveIsoServiceWindow(
		endpoint.key, op.StartFrame, op.TransferFlags, duration)
	if err != nil {
		return udecx.Completion{}, err
	}
	// The operation's full endpoint signature is the authoritative active
	// UdeCx identity. Applying it only after frame validation avoids mutating
	// alternate-setting state for a rejected explicit schedule.
	p.activateEndpoint(dev, op)
	if !p.wait(ctx, serviceStart) {
		return udecx.Completion{}, ctx.Err()
	}
	p.server.processSubmit(ctx, dev, endpoint.number, endpoint.direction, nil, op.Payload)
	if !p.wait(ctx, serviceEnd) {
		return udecx.Completion{}, ctx.Err()
	}
	packets := make([]udecx.IsoPacket, len(op.IsoPackets))
	for i, packet := range op.IsoPackets {
		packets[i] = udecx.IsoPacket{Offset: packet.Offset, Length: packet.Length}
	}
	return successCompletion(op, op.TransferLength, nil, packets), nil
}

func (p *NativeProcessor) processIsoIn(ctx context.Context, dev usbdevice.Device,
	op udecx.Operation, endpoint nativeIsoEndpoint) (udecx.Completion, error) {
	duration := time.Duration(len(op.IsoPackets)) * endpoint.interval
	serviceStart, _, err := p.reserveIsoServiceWindow(
		endpoint.key, op.StartFrame, op.TransferFlags, duration)
	if err != nil {
		return udecx.Completion{}, err
	}
	p.activateEndpoint(dev, op)
	payload := make([]byte, op.TransferLength)
	packets := make([]udecx.IsoPacket, len(op.IsoPackets))
	actualTotal := uint32(0)
	serviceTime := serviceStart
	reader, direct := dev.(usbdevice.IsochronousInputDevice)
	for i, packet := range op.IsoPackets {
		if !p.wait(ctx, serviceTime) {
			return udecx.Completion{}, ctx.Err()
		}
		serviceTime = serviceTime.Add(endpoint.interval)
		var packetData []byte
		if direct {
			packetRegion := payload[packet.Offset : packet.Offset+packet.Length]
			written, readErr := reader.ReadIsochronousInput(ctx, endpoint.number, packetRegion)
			if readErr != nil {
				return udecx.Completion{}, readErr
			}
			if written < 0 || uint32(written) > packet.Length {
				return udecx.Completion{}, fmt.Errorf(
					"native ISO packet %d encoded %d bytes into a %d-byte region",
					i, written, packet.Length)
			}
			packetData = packetRegion[:written]
		} else {
			attemptCtx, cancel := context.WithTimeout(ctx, endpoint.interval)
			packetData = p.server.processSubmit(
				attemptCtx, dev, endpoint.number, endpoint.direction, nil, nil)
			cancel()
		}
		if ctx.Err() != nil {
			return udecx.Completion{}, ctx.Err()
		}
		if len(packetData) == 0 {
			if direct {
				packetData = payload[packet.Offset : packet.Offset+packet.Length]
			} else {
				packetData = make([]byte, packet.Length)
			}
		}
		actual := min(packet.Length, uint32(len(packetData)))
		if !direct {
			copy(payload[packet.Offset:packet.Offset+actual], packetData[:actual])
		}
		packets[i] = udecx.IsoPacket{Offset: packet.Offset, Length: actual}
		actualTotal += actual
		serviceTime = reanchorMissedIsoPacketSlot(
			serviceTime, endpoint.interval, p.clock().now)
	}
	p.extendIsoServiceWindow(endpoint.key, serviceTime)
	return successCompletion(op, actualTotal, payload, packets), nil
}

func (p *NativeProcessor) extendIsoServiceWindow(key nativeLaneKey, serviceEnd time.Time) {
	p.mu.Lock()
	if serviceEnd.After(p.next[key]) {
		p.next[key] = serviceEnd
	}
	p.mu.Unlock()
}

func (p *NativeProcessor) reserveServiceTime(key nativeLaneKey, interval time.Duration) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	serviceTime := p.next[key]
	if serviceTime.IsZero() || now.Sub(serviceTime) >= interval {
		serviceTime = now
	}
	p.next[key] = serviceTime.Add(interval)
	return serviceTime
}

func (p *NativeProcessor) reserveIsoServiceWindow(
	key nativeLaneKey, startFrame, transferFlags uint32, duration time.Duration,
) (time.Time, time.Time, error) {
	sample := p.clock()
	delta := int64(int32(startFrame - sample.frame))
	explicit := transferFlags&udecx.TransferFlagStartIsoASAP == 0
	if explicit && (delta <= 0 || delta >= usbdIsoStartFrameRange) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"native explicit ISO start frame %d is outside the future frame range from %d",
			startFrame, sample.frame)
	}
	plannedStart := sample.now.Add(time.Duration(delta) * time.Millisecond)
	if plannedStart.Before(sample.now) {
		// A delayed ASAP dequeue must not replay elapsed USB frames in a burst.
		// Explicit frames take the range-error path above instead.
		plannedStart = sample.now
	}
	// For ASAP URBs the kernel has already discarded the caller's input value
	// and replaced StartFrame with its ordered output reservation. This mapping
	// gates host-side service only; controller media-clock correction remains in
	// the device engine.

	p.mu.Lock()
	defer p.mu.Unlock()
	if previousEnd := p.next[key]; previousEnd.After(plannedStart) {
		if explicit {
			return time.Time{}, time.Time{}, fmt.Errorf(
				"native explicit ISO start frame %d overlaps the previous endpoint window",
				startFrame)
		}
		plannedStart = previousEnd
	}
	serviceEnd := plannedStart.Add(duration)
	p.next[key] = serviceEnd
	return plannedStart, serviceEnd, nil
}

func successCompletion(op udecx.Operation, transferLength uint32, payload []byte,
	packets []udecx.IsoPacket) udecx.Completion {
	return udecx.Completion{
		Token: op.Token, DeviceID: op.DeviceID, Generation: op.Generation,
		Status: 0, USBDStatus: 0, IsoPackets: packets, Payload: payload,
		TransferLength: transferLength,
	}
}
