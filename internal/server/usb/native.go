package usb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type nativeLaneKey struct {
	deviceID   uint64
	generation uint32
	endpoint   uint8
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

// NativeProcessor adapts the native UdeCx broker to the same control and
// transfer engine used by USB/IP. Transport-specific clocks live here; device
// state, feedback, HID, audio, and descriptor behavior remain in usb.Device.
type NativeProcessor struct {
	server   *Server
	mu       sync.Mutex
	next     map[nativeLaneKey]time.Time
	lastIn   map[nativeLaneKey][]byte
	sessions map[nativeSessionKey]*nativeSessionState
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
	p.mu.Lock()
	for key := range p.next {
		if key.deviceID == identity.DeviceID && key.generation == identity.Generation {
			delete(p.next, key)
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
	}

	switch op.Kind {
	case udecx.OperationEndpointStart:
		p.clearLane(key)
		p.activateEndpointLocked(dev, op, session)
	case udecx.OperationEndpointPurge:
		p.clearLane(key)
		if resetter, ok := dev.(usbdevice.EndpointResetDevice); ok {
			resetter.ResetEndpoint(op.EndpointAddress)
		}
		p.deactivateEndpointLocked(dev, op, session)
	case udecx.OperationEndpointReset:
		p.clearLane(key)
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
			if signatureFromDescriptor(endpoint) != signature {
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
			if _, ok := active[signatureFromDescriptor(endpoint)]; ok {
				return true
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
	session *nativeSessionState) {
	signature := signatureFromOperation(op)
	interfaceNumber, alternateSetting, ok := descriptorInterfaceAltForEndpoint(
		dev.GetDescriptor(), signature)
	if !ok {
		return
	}
	delete(session.active, signature)
	if p.server.getInterfaceAlt(dev, interfaceNumber) == alternateSetting &&
		!descriptorInterfaceAltIsActive(dev.GetDescriptor(), interfaceNumber, alternateSetting, session.active) {
		p.server.setInterfaceAlt(dev, interfaceNumber, 0)
		p.server.notifyInterfaceAlt(dev, interfaceNumber, 0)
	}
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
	p.mu.Unlock()
}

func (p *NativeProcessor) clearLane(key nativeLaneKey) {
	p.mu.Lock()
	delete(p.next, key)
	delete(p.lastIn, key)
	p.mu.Unlock()
}

func (p *NativeProcessor) Process(ctx context.Context, dev usbdevice.Device, op udecx.Operation) (udecx.Completion, error) {
	if dev == nil {
		return udecx.Completion{}, errors.New("native UDE operation has no device")
	}
	if op.TransferLength > udecx.MaxTransferBytes || len(op.Payload) > udecx.MaxTransferBytes {
		return udecx.Completion{}, udecx.ErrLimitExceeded
	}
	ep := uint32(op.EndpointAddress & 0x0f)
	dir := uint32(usbip.DirOut)
	if op.Direction != 0 {
		dir = usbip.DirIn
	}
	key := nativeLaneKey{deviceID: op.DeviceID, generation: op.Generation, endpoint: op.EndpointAddress}
	if len(op.IsoPackets) != 0 {
		// The first ISO URB is itself authoritative proof that Windows activated
		// this endpoint. This also closes the scheduling race where a transfer is
		// dequeued before the endpoint-start notification reaches another worker.
		p.activateEndpoint(dev, op)
	}

	switch {
	case op.Kind == udecx.OperationControl:
		return p.processControl(ctx, dev, op, ep, dir)
	case len(op.IsoPackets) != 0 && dir == usbip.DirIn:
		return p.processIsoIn(ctx, dev, op, ep, dir, key)
	case len(op.IsoPackets) != 0:
		return p.processIsoOut(ctx, dev, op, ep, dir, key)
	case dir == usbip.DirIn:
		return p.processInterruptIn(ctx, dev, op, ep, dir, key)
	default:
		p.server.processSubmit(ctx, dev, ep, dir, nil, op.Payload)
		return successCompletion(op, op.TransferLength, nil, nil), ctx.Err()
	}
}

func (p *NativeProcessor) processControl(ctx context.Context, dev usbdevice.Device,
	op udecx.Operation, ep, dir uint32) (udecx.Completion, error) {
	setup := op.SetupPacket[:]
	response := p.server.processSubmit(ctx, dev, ep, dir, setup, op.Payload)
	if err := ctx.Err(); err != nil {
		return udecx.Completion{}, err
	}
	if dir == usbip.DirOut {
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
	op udecx.Operation, ep, dir uint32, key nativeLaneKey) (udecx.Completion, error) {
	duration := isoCompletionDelay(dev.GetDescriptor(), ep, len(op.IsoPackets))
	deadline := p.reserveCompletionDeadline(key, duration)
	p.server.processSubmit(ctx, dev, ep, dir, nil, op.Payload)
	if !waitUntilContext(ctx, deadline) {
		return udecx.Completion{}, ctx.Err()
	}
	packets := make([]udecx.IsoPacket, len(op.IsoPackets))
	for i, packet := range op.IsoPackets {
		packets[i] = udecx.IsoPacket{Offset: packet.Offset, Length: packet.Length}
	}
	return successCompletion(op, op.TransferLength, nil, packets), nil
}

func (p *NativeProcessor) processIsoIn(ctx context.Context, dev usbdevice.Device,
	op udecx.Operation, ep, dir uint32, key nativeLaneKey) (udecx.Completion, error) {
	interval := isoPacketInterval(dev.GetDescriptor(), ep)
	if interval <= 0 {
		interval = time.Millisecond
	}
	duration := time.Duration(len(op.IsoPackets)) * interval
	serviceStart := p.reserveServiceWindow(key, duration, interval)
	payload := make([]byte, op.TransferLength)
	packets := make([]udecx.IsoPacket, len(op.IsoPackets))
	actualTotal := uint32(0)
	serviceTime := serviceStart
	reader, direct := dev.(usbdevice.IsochronousInputDevice)
	for i, packet := range op.IsoPackets {
		if packet.Offset > op.TransferLength || packet.Length > op.TransferLength-packet.Offset {
			return udecx.Completion{}, fmt.Errorf("native ISO packet %d is outside transfer buffer", i)
		}
		if !waitUntilContext(ctx, serviceTime) {
			return udecx.Completion{}, ctx.Err()
		}
		serviceTime = serviceTime.Add(interval)
		var packetData []byte
		if direct {
			packetRegion := payload[packet.Offset : packet.Offset+packet.Length]
			written, readErr := reader.ReadIsochronousInput(ctx, ep, packetRegion)
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
			attemptCtx, cancel := context.WithTimeout(ctx, interval)
			packetData = p.server.processSubmit(attemptCtx, dev, ep, dir, nil, nil)
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
	}
	return successCompletion(op, actualTotal, payload, packets), nil
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

func (p *NativeProcessor) reserveCompletionDeadline(key nativeLaneKey, duration time.Duration) time.Time {
	if duration <= 0 {
		return time.Now()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	deadline := p.next[key]
	if deadline.IsZero() || now.Sub(deadline) >= duration {
		deadline = now.Add(duration)
	} else {
		deadline = deadline.Add(duration)
	}
	p.next[key] = deadline
	return deadline
}

func (p *NativeProcessor) reserveServiceWindow(key nativeLaneKey, duration, interval time.Duration) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	start := p.next[key]
	if start.IsZero() || (interval > 0 && now.Sub(start) >= interval) {
		start = now
	}
	p.next[key] = start.Add(duration)
	return start
}

func successCompletion(op udecx.Operation, transferLength uint32, payload []byte,
	packets []udecx.IsoPacket) udecx.Completion {
	return udecx.Completion{
		Token: op.Token, DeviceID: op.DeviceID, Generation: op.Generation,
		Status: 0, USBDStatus: 0, IsoPackets: packets, Payload: payload,
		TransferLength: transferLength,
	}
}
