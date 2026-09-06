package xboxone

import (
	"bytes"
	"errors"
	"testing"
)

func TestControllerDownstreamMessageExactClassification(t *testing.T) {
	direct := testDirectMotorBody()
	directWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(directWire, 0x31, direct); err != nil {
		t.Fatal(err)
	}
	led := GuideLEDCommandV1{Pattern: GuideLEDPatternRampToLevel, Intensity: 47}
	ledWire := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(ledWire, 0x32, led); err != nil {
		t.Fatal(err)
	}
	ackBody := ProtocolControlACKBodyV1{
		ReferencedDataClass: DataClassCommand, ReferencedMessageNumber: messageNumberMetadataRequest,
		ReferencedSystem: true, FragmentOffset: 58, RemainingBuffer: 0x1234,
	}
	ackWire := make([]byte, ProtocolControlACKMessageSize)
	if err := EncodeProtocolControlACKMessageInto(ackWire, 0x33, ackBody); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		wire     []byte
		kind     ControllerDownstreamMessageKind
		sequence uint8
	}{
		{name: "metadata lifecycle", wire: []byte{0x04, 0x20, 0x01, 0x00},
			kind: ControllerDownstreamLifecycle, sequence: 1},
		{name: "state lifecycle", wire: []byte{0x05, 0x20, 0x22, 0x01, 0x00},
			kind: ControllerDownstreamLifecycle, sequence: 0x22},
		{name: "security data complete lifecycle",
			wire: []byte{0x06, 0x20, 0x34, 0x02, 0x01, 0x00},
			kind: ControllerDownstreamLifecycle, sequence: 0x34},
		{name: "protocol ack", wire: ackWire,
			kind: ControllerDownstreamProtocolControlACK, sequence: 0x33},
		{name: "direct motor", wire: directWire,
			kind: ControllerDownstreamDirectMotor, sequence: 0x31},
		{name: "guide led", wire: ledWire,
			kind: ControllerDownstreamGuideLED, sequence: 0x32},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecodeControllerDownstreamMessage(test.wire)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != test.kind || got.Sequence != test.sequence {
				t.Fatalf("classification = kind %d sequence %d", got.Kind, got.Sequence)
			}
		})
	}

	unsupported := []byte{0x06, 0x20, 0x01, 0x02, 0x02, 0x00}
	if _, err := DecodeControllerDownstreamMessage(unsupported); !errors.Is(
		err, ErrUnsupportedControllerPersonaHostMessage) {
		t.Fatalf("unsupported error = %v", err)
	}
	validWires := [][]byte{
		{0x04, 0x20, 0x01, 0x00},
		{0x05, 0x20, 0x22, 0x01, 0x00},
		{0x06, 0x20, 0x34, 0x02, 0x01, 0x00},
		ackWire, directWire, ledWire,
	}
	for _, valid := range validWires {
		for size := 0; size < len(valid); size++ {
			wire := append([]byte(nil), valid[:size]...)
			if _, err := DecodeControllerDownstreamMessage(wire); !errors.Is(
				err, ErrInvalidLength) {
				t.Fatalf("short %d/%d wire % x error = %v, want invalid length",
					size, len(valid), wire, err)
			}
		}
		wire := append(append([]byte(nil), valid...), 0)
		if _, err := DecodeControllerDownstreamMessage(wire); !errors.Is(
			err, ErrInvalidLength) {
			t.Fatalf("oversized wire % x error = %v, want invalid length", wire, err)
		}
	}
	coalesced := append(append([]byte(nil), validWires[0]...), validWires[3]...)
	if _, err := DecodeControllerDownstreamMessage(coalesced); !errors.Is(
		err, ErrInvalidLength) {
		t.Fatalf("coalesced wire error = %v, want invalid length", err)
	}
}

func testControllerDownstreamWires(
	t *testing.T,
) ([][]byte, RumbleBodyV1, GuideLEDCommandV1, ProtocolControlACKBodyV1) {
	t.Helper()
	direct := testDirectMotorBody()
	directWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(directWire, 0x31, direct); err != nil {
		t.Fatal(err)
	}
	led := GuideLEDCommandV1{Pattern: GuideLEDPatternRampToLevel, Intensity: 47}
	ledWire := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(ledWire, 0x32, led); err != nil {
		t.Fatal(err)
	}
	ack := ProtocolControlACKBodyV1{
		ReferencedDataClass: DataClassCommand, ReferencedMessageNumber: messageNumberMetadataRequest,
		ReferencedSystem: true, FragmentOffset: 58, RemainingBuffer: 0x1234,
	}
	ackWire := make([]byte, ProtocolControlACKMessageSize)
	if err := EncodeProtocolControlACKMessageInto(ackWire, 0x33, ack); err != nil {
		t.Fatal(err)
	}
	return [][]byte{
		{0x04, 0x20, 0x01, 0x00},
		{0x05, 0x20, 0x22, 0x01, byte(SetDeviceStateQuiesce)},
		ackWire,
		directWire,
		ledWire,
	}, direct, led, ack
}

func TestControllerDownstreamPacketCoalescedOrderAndSnapshot(t *testing.T) {
	wires, direct, led, ack := testControllerDownstreamWires(t)
	wire := bytes.Join(wires, nil)
	packet, err := DecodeControllerDownstreamPacket(wire)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Len() != len(wires) {
		t.Fatalf("packet length = %d, want %d", packet.Len(), len(wires))
	}
	want := [...]ControllerDownstreamMessage{
		{Kind: ControllerDownstreamLifecycle, Sequence: 1,
			Lifecycle: ControllerHostCommand{Kind: ControllerHostCommandMetadataRequest, Sequence: 1}},
		{Kind: ControllerDownstreamLifecycle, Sequence: 0x22,
			Lifecycle: ControllerHostCommand{Kind: ControllerHostCommandSetDeviceState,
				Sequence: 0x22, State: SetDeviceStateQuiesce}},
		{Kind: ControllerDownstreamProtocolControlACK, Sequence: 0x33, ACK: ack},
		{Kind: ControllerDownstreamDirectMotor, Sequence: 0x31, DirectMotor: direct},
		{Kind: ControllerDownstreamGuideLED, Sequence: 0x32, GuideLED: led},
	}
	for index := range want {
		got, ok := packet.Message(index)
		if !ok || got != want[index] {
			t.Fatalf("message %d = (%+v, %t), want %+v", index, got, ok, want[index])
		}
	}
	if got, ok := packet.Message(-1); ok || got != (ControllerDownstreamMessage{}) {
		t.Fatalf("negative lookup = (%+v, %t)", got, ok)
	}
	if got, ok := packet.Message(packet.Len()); ok || got != (ControllerDownstreamMessage{}) {
		t.Fatalf("past-end lookup = (%+v, %t)", got, ok)
	}
	var nilPacket *ControllerDownstreamPacket
	if nilPacket.Len() != 0 {
		t.Fatalf("nil packet length = %d", nilPacket.Len())
	}
	if got, ok := nilPacket.Message(0); ok || got != (ControllerDownstreamMessage{}) {
		t.Fatalf("nil packet lookup = (%+v, %t)", got, ok)
	}

	clear(wire)
	got, ok := packet.Message(3)
	if !ok || got.DirectMotor != direct {
		t.Fatalf("caller mutation changed packet snapshot: (%+v, %t)", got, ok)
	}
}

func TestControllerDownstreamPacketMaximumCompleteMessages(t *testing.T) {
	message := []byte{0x04, 0x20, 0x01, 0x00}
	wire := bytes.Repeat(message, controllerDownstreamPacketMaximumMessages)
	if len(wire) != ControllerDownstreamPacketMaximumSize {
		t.Fatalf("fixture size = %d", len(wire))
	}
	packet, err := DecodeControllerDownstreamPacket(wire)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Len() != controllerDownstreamPacketMaximumMessages {
		t.Fatalf("packet length = %d, want %d",
			packet.Len(), controllerDownstreamPacketMaximumMessages)
	}
	for index := 0; index < packet.Len(); index++ {
		message, ok := packet.Message(index)
		if !ok || message.Kind != ControllerDownstreamLifecycle ||
			message.Lifecycle.Kind != ControllerHostCommandMetadataRequest {
			t.Fatalf("message %d = (%+v, %t)", index, message, ok)
		}
	}
}

func TestControllerDownstreamPacketFailsClosedAcrossWholePacket(t *testing.T) {
	wires, _, _, _ := testControllerDownstreamWires(t)
	metadata := wires[0]
	unsupported := []byte{0x06, 0x20, 0x01, 0x00}
	fragmented := []byte{0x05, flagFragment | flagSystem, 0x01, 0x01, 0x00}
	extended := []byte{0x05, flagSystem, 0x01, lengthExtended | 0x01, 0x00}
	malformedDirect := append([]byte(nil), wires[3]...)
	malformedDirect[SinglePacketHeaderSize+2] = 101

	tests := []struct {
		name string
		wire []byte
		want error
	}{
		{name: "empty", wire: nil, want: ErrInvalidLength},
		{name: "oversized", wire: make([]byte, ControllerDownstreamPacketMaximumSize+1),
			want: ErrInvalidLength},
		{name: "trailing partial header", wire: append(append([]byte(nil), metadata...), 0x05),
			want: ErrInvalidLength},
		{name: "truncated later body", wire: append(append([]byte(nil), metadata...), wires[4][:len(wires[4])-1]...),
			want: ErrInvalidLength},
		{name: "unsupported later family", wire: append(append([]byte(nil), metadata...), unsupported...),
			want: ErrUnsupportedControllerPersonaHostMessage},
		{name: "fragmented later message", wire: append(append([]byte(nil), metadata...), fragmented...),
			want: ErrFragmentedHeader},
		{name: "extended later header", wire: append(append([]byte(nil), metadata...), extended...),
			want: ErrExtendedPayloadLength},
		{name: "malformed later body", wire: append(append([]byte(nil), metadata...), malformedDirect...),
			want: ErrMotorLevelOutOfRange},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet, err := DecodeControllerDownstreamPacket(test.wire)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if packet.Len() != 0 {
				t.Fatalf("failed decode exposed partial packet: %+v", packet)
			}
		})
	}
}

func TestControllerDownstreamPacketDecodeAllocations(t *testing.T) {
	wires, _, _, _ := testControllerDownstreamWires(t)
	wire := bytes.Join(wires, nil)
	if _, err := DecodeControllerDownstreamPacket(wire); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(1000, func() {
		packet, err := DecodeControllerDownstreamPacket(wire)
		if err != nil || packet.Len() != len(wires) {
			panic("valid packet rejected")
		}
	})
	if allocs != 0 {
		t.Fatalf("downstream packet allocations = %v, want 0", allocs)
	}
}
