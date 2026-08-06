// Package xboxseries implements an Xbox Series X|S-compatible GIP USB gamepad.
package xboxseries

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

const (
	messageProtocolControl = 0x01
	messageHello           = 0x02
	messageStatus          = 0x03
	messageMetadata        = 0x04
	messageSetState        = 0x05
	messageAuthenticate    = 0x06
	messageGuide           = 0x07
	messageMotor           = 0x09
	messageInput           = 0x20

	stateStart = 0x00
	stateStop  = 0x01
	stateOff   = 0x04
	stateReset = 0x07
)

type XboxSeries struct {
	descriptor usb.Descriptor

	mu             sync.Mutex
	inputState     InputState
	lastPayload    []byte
	active         bool
	forceInput     bool
	guideDown      bool
	lastHello      time.Time
	lastStatus     time.Time
	startedAt      time.Time
	globalSequence byte
	inputSequence  byte
	metadata       []byte
	metadataSeq    byte
	metadataSent   int
	queue          [][]byte
	signal         chan struct{}

	feedbackDispatchMu sync.Mutex
	feedbackMu         sync.Mutex
	feedbackState      MotorState
	feedbackSeen       bool
	feedbackFunc       func(MotorState)
	feedbackScheduleMu sync.Mutex
	feedbackGeneration uint64
	feedbackCancel     context.CancelFunc
}

func New(o *device.CreateOptions) (*XboxSeries, error) {
	x := &XboxSeries{
		descriptor: MakeDescriptor(),
		metadata:   seriesMetadata(),
		signal:     make(chan struct{}, 1),
	}
	if o != nil {
		if o.IDVendor != nil {
			x.descriptor.Device.IDVendor = *o.IDVendor
		}
		if o.IDProduct != nil {
			x.descriptor.Device.IDProduct = *o.IDProduct
		}
	}
	x.notify()
	return x, nil
}

func MakeDescriptor() usb.Descriptor {
	return usb.Descriptor{
		Device: usb.DeviceDescriptor{
			BcdUSB: 0x0200, BDeviceClass: 0xFF, BDeviceSubClass: 0x47,
			BDeviceProtocol: 0xD0, BMaxPacketSize0: 0x40,
			IDVendor: 0x045E, IDProduct: 0x0B12, BcdDevice: 0x0500,
			IManufacturer: 1, IProduct: 2, ISerialNumber: 3,
			BNumConfigurations: 1, Speed: 2,
		},
		MicrosoftOS10: &usb.MicrosoftOS10Descriptor{
			VendorCode: 0x90, InterfaceNumber: 0, CompatibleID: "XGIP10",
		},
		Interfaces: []usb.InterfaceConfig{{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber: 0, BAlternateSetting: 0, BNumEndpoints: 2,
				BInterfaceClass: 0xFF, BInterfaceSubClass: 0x47,
				BInterfaceProtocol: 0xD0,
			},
			Endpoints: []usb.EndpointDescriptor{
				{BEndpointAddress: 0x01, BMAttributes: 0x03, WMaxPacketSize: 64, BInterval: 4},
				{BEndpointAddress: 0x81, BMAttributes: 0x03, WMaxPacketSize: 64, BInterval: 4},
			},
		}},
		Strings: map[uint8]string{0: "\u0409", 1: "Microsoft", 2: "Xbox Series X|S Controller", 3: "VIIPERXS0001"},
	}
}

func (x *XboxSeries) GetDescriptor() *usb.Descriptor        { return &x.descriptor }
func (x *XboxSeries) GetDeviceSpecificArgs() map[string]any { return map[string]any{} }

func (x *XboxSeries) SetMotorCallback(f func(MotorState)) {
	x.feedbackDispatchMu.Lock()
	defer x.feedbackDispatchMu.Unlock()
	x.feedbackMu.Lock()
	x.feedbackFunc = f
	latest, replay := x.feedbackState, f != nil && x.feedbackSeen
	x.feedbackMu.Unlock()
	if replay {
		f(latest)
	}
}

func (x *XboxSeries) UpdateInputState(state InputState) {
	x.mu.Lock()
	x.inputState = state
	x.mu.Unlock()
	x.notify()
}

func (x *XboxSeries) HandleTransfer(ctx context.Context, ep uint32, dir uint32, out []byte) []byte {
	if ep != 1 {
		return nil
	}
	if dir == usbip.DirOut {
		x.handleHostPacket(out)
		return nil
	}
	if dir != usbip.DirIn {
		return nil
	}

	for {
		if packet := x.nextPacket(); packet != nil {
			return packet
		}
		delay := x.nextWakeDelay()
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return nil
		case <-x.signal:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (x *XboxSeries) nextPacket() []byte {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.queue) != 0 {
		packet := x.queue[0]
		x.queue = x.queue[1:]
		return packet
	}
	now := time.Now()
	if !x.active {
		if x.lastHello.IsZero() || now.Sub(x.lastHello) >= 500*time.Millisecond {
			x.lastHello = now
			return x.helloPacketLocked()
		}
		return nil
	}

	guide := x.inputState.Buttons&ButtonGuide != 0
	if guide != x.guideDown {
		x.guideDown = guide
		return x.guidePacketLocked(guide)
	}
	payload := x.inputState.buildGamepadPayload()
	if x.forceInput || !bytes.Equal(payload, x.lastPayload) {
		x.forceInput = false
		x.lastPayload = append(x.lastPayload[:0], payload...)
		return x.packetLocked(messageInput, 0, x.nextInputSequenceLocked(), payload)
	}
	statusInterval := 20 * time.Second
	if now.Sub(x.startedAt) < 10*time.Second {
		statusInterval = time.Second
	}
	if x.lastStatus.IsZero() || now.Sub(x.lastStatus) >= statusInterval {
		x.lastStatus = now
		return x.statusPacketLocked(false)
	}
	return nil
}

func (x *XboxSeries) nextWakeDelay() time.Duration {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.queue) != 0 || x.forceInput {
		return 0
	}
	now := time.Now()
	if !x.active {
		if x.lastHello.IsZero() {
			return 0
		}
		return remaining(500*time.Millisecond, now.Sub(x.lastHello))
	}
	interval := 20 * time.Second
	if now.Sub(x.startedAt) < 10*time.Second {
		interval = time.Second
	}
	if x.lastStatus.IsZero() {
		return 0
	}
	return remaining(interval, now.Sub(x.lastStatus))
}

func remaining(period, elapsed time.Duration) time.Duration {
	if elapsed >= period {
		return 0
	}
	return period - elapsed
}

func (x *XboxSeries) handleHostPacket(packet []byte) {
	for len(packet) >= 4 {
		payloadLength, lengthBytes, ok := decodeGIPVarint(packet[3:])
		if !ok {
			return
		}
		headerLength := 3 + lengthBytes
		fragmentEnd := payloadLength
		if packet[1]&0x80 != 0 {
			totalOrOffset, offsetBytes, offsetOK := decodeGIPVarint(packet[headerLength:])
			if !offsetOK {
				return
			}
			headerLength += offsetBytes
			if packet[1]&0x40 == 0 {
				fragmentEnd += totalOrOffset
			}
		}
		if headerLength+payloadLength > len(packet) {
			return
		}
		payload := packet[headerLength : headerLength+payloadLength]
		x.handleMessage(packet[0], packet[1], packet[2], payload, fragmentEnd)
		packet = packet[headerLength+payloadLength:]
	}
}

func decodeGIPVarint(data []byte) (int, int, bool) {
	value := 0
	for index := 0; index < len(data) && index < 4; index++ {
		value |= int(data[index]&0x7F) << (7 * index)
		if data[index]&0x80 == 0 {
			return value, index + 1, true
		}
	}
	return 0, 0, false
}

func (x *XboxSeries) handleMessage(kind, flags byte, sequence byte,
	payload []byte, fragmentEnd int) {
	switch kind {
	case messageMetadata:
		if len(payload) == 0 {
			x.mu.Lock()
			x.active = false
			x.metadataSeq = x.nextGlobalSequenceLocked()
			x.metadataSent = 0
			x.queue = x.queue[:0]
			x.queueMetadataFragmentLocked(0)
			x.mu.Unlock()
			x.notify()
		}
	case messageProtocolControl:
		x.handleAck(payload)
	case messageAuthenticate:
		// Security is a reliable message class. In addition to explicit ACME
		// requests, acknowledge its zero-length Data Complete packet so the
		// sender can finish the required three-way transfer handshake.
		if flags&0x10 != 0 || flags&0x80 != 0 && len(payload) == 0 {
			x.queueAcknowledge(kind, flags, sequence, fragmentEnd, 0)
		}
	case messageSetState:
		if len(payload) != 0 {
			x.setState(payload[0])
		}
	case messageMotor:
		x.handleMotor(payload)
	}
}

// queueAcknowledge completes the outer GIP delivery contract. Authentication
// packets are always acknowledgment-gated; Windows will not advance to the
// next handshake command until the exact command, sequence and delivered-byte
// count have been acknowledged.
func (x *XboxSeries) queueAcknowledge(kind, flags, sequence byte,
	delivered, remaining int) {
	payload := make([]byte, 9)
	payload[1] = kind
	payload[2] = flags & 0x2F // client id + internal; strip chunk/ACK flags
	binary.LittleEndian.PutUint16(payload[3:5], uint16(delivered))
	binary.LittleEndian.PutUint16(payload[7:9], uint16(remaining))

	x.mu.Lock()
	x.queue = append(x.queue,
		x.packetLocked(messageProtocolControl, flags&0x2F, sequence, payload))
	x.mu.Unlock()
	x.notify()
}

func (x *XboxSeries) setState(state byte) {
	x.mu.Lock()
	switch state {
	case stateStart:
		x.active, x.forceInput = true, true
		x.startedAt, x.lastStatus = time.Now(), time.Time{}
		x.queue = append(x.queue, x.statusPacketLocked(false))
	case stateStop:
		x.active = false
	case stateOff, stateReset:
		x.active = false
		x.queue = append(x.queue, x.statusPacketLocked(true))
		x.lastHello = time.Time{}
	}
	x.mu.Unlock()
	x.notify()
}

func (x *XboxSeries) handleAck(payload []byte) {
	if len(payload) < 9 || payload[1] != messageMetadata {
		return
	}
	acknowledged64 := uint64(binary.LittleEndian.Uint32(payload[3:7]))
	if acknowledged64 > uint64(len(x.metadata)) {
		return
	}
	acknowledged := int(acknowledged64)
	x.mu.Lock()
	defer x.mu.Unlock()
	if acknowledged >= len(x.metadata) {
		x.queue = append(x.queue, metadataComplete(x.metadataSeq, len(x.metadata)))
		x.metadataSent = len(x.metadata)
	} else if acknowledged >= x.metadataSent {
		for offset := acknowledged; offset < len(x.metadata); offset += 58 {
			x.queueMetadataFragmentLocked(offset)
		}
	}
	x.notify()
}

func (x *XboxSeries) queueMetadataFragmentLocked(offset int) {
	if offset >= len(x.metadata) {
		return
	}
	remaining := len(x.metadata) - offset
	length := 58
	if remaining < length {
		length = remaining
	}
	flags := byte(0xA0)
	if offset == 0 {
		flags = 0xF0
	}
	if offset+length == len(x.metadata) {
		flags = 0xB0
	}
	p := make([]byte, 6+length)
	p[0], p[1], p[2] = messageMetadata, flags, x.metadataSeq
	switch flags {
	case 0xF0: // initial: 1-byte payload length + 2-byte total size
		p[3] = byte(length)
		p[4], p[5] = byte(len(x.metadata)&0x7F)|0x80,
			byte(len(x.metadata)>>7)
	case 0xA0: // middle: 2-byte payload length + 1-byte offset
		p[3], p[4] = byte(length&0x7F)|0x80, byte(length>>7)
		p[5] = byte(offset)
	default: // final: 1-byte payload length + 2-byte offset
		p[3] = byte(length)
		p[4], p[5] = byte(offset&0x7F)|0x80, byte(offset>>7)
	}
	copy(p[6:], x.metadata[offset:offset+length])
	x.queue = append(x.queue, p)
	if offset+length > x.metadataSent {
		x.metadataSent = offset + length
	}
}

func metadataComplete(sequence byte, length int) []byte {
	return []byte{messageMetadata, 0xA0, sequence, 0, byte(length&0x7F) | 0x80, byte(length >> 7)}
}

func (x *XboxSeries) handleMotor(payload []byte) {
	if len(payload) < 9 || payload[0] != 0 {
		return
	}
	bitmap := payload[1]
	state := MotorState{Duration: payload[6], Delay: payload[7], Repeat: payload[8]}
	if state.Duration != 0 {
		if bitmap&0x02 != 0 {
			state.LeftMotor = normalizeMotor(payload[4])
		}
		if bitmap&0x01 != 0 {
			state.RightMotor = normalizeMotor(payload[5])
		}
		if bitmap&0x08 != 0 {
			state.LeftImpulse = normalizeMotor(payload[2])
		}
		if bitmap&0x04 != 0 {
			state.RightImpulse = normalizeMotor(payload[3])
		}
	}
	x.scheduleMotor(state)
}

func normalizeMotor(v byte) byte {
	if v > 100 {
		v = 100
	}
	return byte((uint16(v)*255 + 50) / 100)
}

func (x *XboxSeries) emitMotor(state MotorState) {
	x.feedbackDispatchMu.Lock()
	defer x.feedbackDispatchMu.Unlock()
	x.feedbackMu.Lock()
	x.feedbackState, x.feedbackSeen = state, true
	f := x.feedbackFunc
	x.feedbackMu.Unlock()
	if f != nil {
		f(state)
	}
}

// scheduleMotor mirrors the controller-side timing contract for direct motor
// commands. A new command atomically supersedes the previous command; delay is
// honored before each play, duration is the on-time, and repeat is the number
// of additional plays. Delay-free repeats are coalesced into one continuous
// interval so they cannot introduce artificial gaps in impulse or body rumble.
func (x *XboxSeries) scheduleMotor(state MotorState) {
	x.feedbackScheduleMu.Lock()
	x.feedbackGeneration++
	generation := x.feedbackGeneration
	if x.feedbackCancel != nil {
		x.feedbackCancel()
		x.feedbackCancel = nil
	}
	if state.Duration == 0 {
		x.emitMotor(state)
		x.feedbackScheduleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	x.feedbackCancel = cancel
	x.feedbackScheduleMu.Unlock()

	if state.Delay == 0 {
		x.publishScheduledMotor(generation, state)
		total := time.Duration(state.Duration) * 10 * time.Millisecond *
			time.Duration(uint16(state.Repeat)+1)
		go x.finishMotorAfter(ctx, generation, total)
		return
	}

	go x.runMotorPattern(ctx, generation, state)
}

func (x *XboxSeries) runMotorPattern(ctx context.Context,
	generation uint64, state MotorState) {
	delay := time.Duration(state.Delay) * 10 * time.Millisecond
	duration := time.Duration(state.Duration) * 10 * time.Millisecond
	plays := int(state.Repeat) + 1
	for play := 0; play < plays; play++ {
		if !waitMotor(ctx, delay) {
			return
		}
		if !x.publishScheduledMotor(generation, state) {
			return
		}
		if !waitMotor(ctx, duration) {
			return
		}
		if !x.publishScheduledMotor(generation, MotorState{}) {
			return
		}
	}
}

func (x *XboxSeries) finishMotorAfter(ctx context.Context,
	generation uint64, duration time.Duration) {
	if waitMotor(ctx, duration) {
		x.publishScheduledMotor(generation, MotorState{})
	}
}

func waitMotor(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (x *XboxSeries) publishScheduledMotor(generation uint64,
	state MotorState) bool {
	x.feedbackScheduleMu.Lock()
	defer x.feedbackScheduleMu.Unlock()
	if generation != x.feedbackGeneration {
		return false
	}
	x.emitMotor(state)
	return true
}

func (x *XboxSeries) helloPacketLocked() []byte {
	p := make([]byte, 28)
	copy(p[0:6], []byte{1, 0, 0, 0, 0, 2})
	binary.LittleEndian.PutUint16(p[8:10], x.descriptor.Device.IDVendor)
	binary.LittleEndian.PutUint16(p[10:12], x.descriptor.Device.IDProduct)
	binary.LittleEndian.PutUint16(p[12:14], 1)
	binary.LittleEndian.PutUint16(p[14:16], 1)
	p[20], p[21], p[22], p[23], p[24], p[25], p[26], p[27] = 1, 0, 1, 0, 1, 0, 1, 0
	return x.packetLocked(messageHello, 0x20, x.nextGlobalSequenceLocked(), p)
}

func (x *XboxSeries) statusPacketLocked(poweringOff bool) []byte {
	status := byte(0x80) // wired, full power, no battery
	if poweringOff {
		status = 0
	}
	return x.packetLocked(messageStatus, 0x20, x.nextGlobalSequenceLocked(), []byte{status, 0, 0, 0})
}

func (x *XboxSeries) guidePacketLocked(down bool) []byte {
	v := byte(0)
	if down {
		v = 1
	}
	return x.packetLocked(messageGuide, 0x20, x.nextGlobalSequenceLocked(), []byte{v, 0x5B})
}

func (x *XboxSeries) packetLocked(kind, flags, sequence byte, payload []byte) []byte {
	p := make([]byte, 4+len(payload))
	p[0], p[1], p[2], p[3] = kind, flags, sequence, byte(len(payload))
	copy(p[4:], payload)
	return p
}

func (x *XboxSeries) nextGlobalSequenceLocked() byte {
	x.globalSequence++
	if x.globalSequence == 0 {
		x.globalSequence = 1
	}
	return x.globalSequence
}

func (x *XboxSeries) nextInputSequenceLocked() byte {
	x.inputSequence++
	if x.inputSequence == 0 {
		x.inputSequence = 1
	}
	return x.inputSequence
}

func (x *XboxSeries) notify() {
	select {
	case x.signal <- struct{}{}:
	default:
	}
}
