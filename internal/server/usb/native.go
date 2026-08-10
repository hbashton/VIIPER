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

// NativeProcessor adapts the native UdeCx broker to the same control and
// transfer engine used by USB/IP. Transport-specific clocks live here; device
// state, feedback, HID, audio, and descriptor behavior remain in usb.Device.
type NativeProcessor struct {
	server *Server
	mu     sync.Mutex
	next   map[nativeLaneKey]time.Time
	lastIn map[nativeLaneKey][]byte
}

func NewNativeProcessor(server *Server) (*NativeProcessor, error) {
	if server == nil {
		return nil, errors.New("native UDE processor requires a USB server engine")
	}
	return &NativeProcessor{
		server: server,
		next:   make(map[nativeLaneKey]time.Time),
		lastIn: make(map[nativeLaneKey][]byte),
	}, nil
}

func (p *NativeProcessor) Reset(dev usbdevice.Device, identity udecx.DeviceIdentity) {
	p.server.resetInterfaceAlts(dev)
	p.mu.Lock()
	for key := range p.next {
		if key.deviceID == identity.DeviceID && key.generation == identity.Generation {
			delete(p.next, key)
			delete(p.lastIn, key)
		}
	}
	p.mu.Unlock()
}

func (p *NativeProcessor) Lifecycle(_ context.Context, dev usbdevice.Device, op udecx.Operation) error {
	identity := udecx.DeviceIdentity{DeviceID: op.DeviceID, Generation: op.Generation}
	key := nativeLaneKey{
		deviceID: op.DeviceID, generation: op.Generation, endpoint: op.EndpointAddress,
	}

	switch op.Kind {
	case udecx.OperationEndpointStart:
		p.clearLane(key)
	case udecx.OperationEndpointPurge, udecx.OperationEndpointReset:
		p.clearLane(key)
		if resetter, ok := dev.(usbdevice.EndpointResetDevice); ok {
			resetter.ResetEndpoint(op.EndpointAddress)
		}
	case udecx.OperationDeviceReset, udecx.OperationDeviceD0Entry, udecx.OperationDeviceD0Exit:
		p.Reset(dev, identity)
	case udecx.OperationSetInterface:
		if !descriptorHasInterfaceAlt(dev.GetDescriptor(), op.InterfaceNumber, op.InterfaceSetting) {
			return fmt.Errorf("native UDE selected invalid alternate setting %d for interface %d",
				op.InterfaceSetting, op.InterfaceNumber)
		}
		p.clearDeviceLanes(identity)
		p.server.setInterfaceAlt(dev, op.InterfaceNumber, op.InterfaceSetting)
		p.server.notifyInterfaceAlt(dev, op.InterfaceNumber, op.InterfaceSetting)
	default:
		return fmt.Errorf("unsupported native UDE lifecycle operation %d", op.Kind)
	}
	return nil
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
	for i, packet := range op.IsoPackets {
		if packet.Offset > op.TransferLength || packet.Length > op.TransferLength-packet.Offset {
			return udecx.Completion{}, fmt.Errorf("native ISO packet %d is outside transfer buffer", i)
		}
		if !waitUntilContext(ctx, serviceTime) {
			return udecx.Completion{}, ctx.Err()
		}
		serviceTime = serviceTime.Add(interval)
		attemptCtx, cancel := context.WithTimeout(ctx, interval)
		packetData := p.server.processSubmit(attemptCtx, dev, ep, dir, nil, nil)
		cancel()
		if ctx.Err() != nil {
			return udecx.Completion{}, ctx.Err()
		}
		if len(packetData) == 0 {
			packetData = make([]byte, packet.Length)
		}
		actual := min(packet.Length, uint32(len(packetData)))
		copy(payload[packet.Offset:packet.Offset+actual], packetData[:actual])
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
