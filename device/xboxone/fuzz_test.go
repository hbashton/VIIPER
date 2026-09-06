package xboxone

import (
	"bytes"
	"testing"
)

func FuzzSinglePacketHeaderRoundTrip(f *testing.F) {
	f.Add([]byte{0x09, 0x00, 0x01, 0x09})
	f.Add([]byte{0x20, 0x00, 0xff, 0x0e})
	f.Add([]byte{0x20, flagReserved, 0x01, 0x00})
	f.Fuzz(func(t *testing.T, wire []byte) {
		header, err := DecodeSinglePacketHeader(wire)
		if err != nil {
			return
		}
		var encoded [SinglePacketHeaderSize]byte
		if err := EncodeSinglePacketHeaderInto(encoded[:], header); err != nil {
			t.Fatalf("decoded header could not be encoded: %v", err)
		}
		for index := range encoded {
			if encoded[index] != wire[index] {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	})
}

func FuzzBaseInputBodyRoundTrip(f *testing.F) {
	f.Add(make([]byte, BaseInputBodySize))
	f.Add([]byte{0x01})
	f.Fuzz(func(t *testing.T, wire []byte) {
		report, err := DecodeBaseInputBody(wire)
		if err != nil {
			return
		}
		var encoded [BaseInputBodySize]byte
		if err := EncodeBaseInputBodyInto(encoded[:], report); err != nil {
			t.Fatalf("decoded body could not be encoded: %v", err)
		}
		for index := range encoded {
			if encoded[index] != wire[index] {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	})
}

func FuzzDirectMotorBodyRoundTrip(f *testing.F) {
	f.Add(make([]byte, RumbleBodySize))
	f.Add([]byte{0x00, 0x0f, 100, 100, 100, 100, 1, 0, 0})
	f.Add([]byte{0x01})
	f.Fuzz(func(t *testing.T, wire []byte) {
		body, err := DecodeRumbleBody(wire)
		if err != nil {
			return
		}
		var encoded [RumbleBodySize]byte
		if err := EncodeRumbleBodyInto(encoded[:], body); err != nil {
			t.Fatalf("decoded body could not be encoded: %v", err)
		}
		for index := range encoded {
			if encoded[index] != wire[index] {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	})
}

func FuzzGamepadInputMessageRoundTrip(f *testing.F) {
	var seed [GamepadInputMessageSize]byte
	if err := EncodeGamepadInputMessageInto(seed[:], 1, GamepadInputReportV1{}); err != nil {
		f.Fatalf("seed gamepad input: %v", err)
	}
	f.Add(seed[:])
	f.Add([]byte{0x20, 0x00, 0x00, 0x0e})
	f.Fuzz(func(t *testing.T, wire []byte) {
		sequence, report, err := DecodeGamepadInputMessage(wire)
		if err != nil {
			return
		}
		var encoded [GamepadInputMessageSize]byte
		if err := EncodeGamepadInputMessageInto(encoded[:], sequence, report); err != nil {
			t.Fatalf("decoded gamepad message could not be encoded: %v", err)
		}
		for index := range encoded {
			if encoded[index] != wire[index] {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	})
}

func FuzzDirectMotorMessageRoundTrip(f *testing.F) {
	var seed [DirectMotorMessageSize]byte
	if err := EncodeDirectMotorMessageInto(seed[:], 1, RumbleBodyV1{}); err != nil {
		f.Fatalf("seed direct motor: %v", err)
	}
	f.Add(seed[:])
	f.Add([]byte{0x09, 0x00, 0x00, 0x09})
	f.Fuzz(func(t *testing.T, wire []byte) {
		sequence, body, err := DecodeDirectMotorMessage(wire)
		if err != nil {
			return
		}
		var encoded [DirectMotorMessageSize]byte
		if err := EncodeDirectMotorMessageInto(encoded[:], sequence, body); err != nil {
			t.Fatalf("decoded direct-motor message could not be encoded: %v", err)
		}
		for index := range encoded {
			if encoded[index] != wire[index] {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	})
}

func FuzzGuideLEDCommandMessageRoundTrip(f *testing.F) {
	f.Add([]byte{0x0a, 0x20, 0x01, 0x03, 0x00, 0x01, 0x2f})
	f.Add([]byte{0x0a, 0x20, 0x00, 0x03, 0x00, 0x01, 0x00})
	f.Fuzz(func(t *testing.T, wire []byte) {
		sequence, command, err := DecodeGuideLEDCommandMessage(wire)
		if err != nil {
			return
		}
		var encoded [GuideLEDCommandMessageSize]byte
		if err := EncodeGuideLEDCommandMessageInto(
			encoded[:], sequence, command); err != nil {
			t.Fatalf("decoded Guide LED message could not be encoded: %v", err)
		}
		for index := range encoded {
			if encoded[index] != wire[index] {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	})
}

func FuzzSemanticInputWireV1RoundTrip(f *testing.F) {
	f.Add([]byte{
		0x01, 0x00, 0x18, 0x00, 0x65, 0xda, 0x00, 0x00,
		0x34, 0x02, 0xcd, 0x03, 0x02, 0x01, 0xfe, 0xff,
		0x00, 0x80, 0xff, 0x7f, 0x00, 0x00, 0x00, 0x00,
	})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, wire []byte) {
		state, err := DecodeSemanticInputWireV1(wire)
		if err != nil {
			return
		}
		if err := state.Validate(); err != nil {
			t.Fatalf("decoder admitted invalid state: %v", err)
		}
		var encoded [SemanticInputWireSize]byte
		if err := EncodeSemanticInputWireV1Into(encoded[:], state); err != nil {
			t.Fatalf("decoded state could not be encoded: %v", err)
		}
		if !bytes.Equal(encoded[:], wire) {
			t.Fatalf("round trip = % x, want % x", encoded, wire)
		}
	})
}

func FuzzUSBControlPlaneSetup(f *testing.F) {
	f.Add([]byte{0x80, 0x06, 0x00, 0x01, 0x00, 0x00, 0x08, 0x00})
	f.Add([]byte{0x00, 0x05, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, setup []byte) {
		plane, err := NewUSBControlPlane(testOnlySyntheticControllerProfileForFuzz())
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		claim, err := plane.Claim(setup)
		if err != nil {
			return
		}
		response := make([]byte, claim.ResponseSize())
		if err := plane.AdmitAndCopy(claim, response); err != nil {
			t.Fatalf("admit claimed setup: %v", err)
		}
		if err := plane.Resolve(claim, USBControlDelivered); err != nil {
			t.Fatalf("resolve claimed setup: %v", err)
		}
		snapshot := plane.Snapshot()
		if snapshot.Generation != 1 || snapshot.ClaimOutstanding ||
			snapshot.ClaimAdmitted || snapshot.State < USBControlDeviceDefault ||
			snapshot.State > USBControlDeviceConfigured {
			t.Fatalf("invalid post-request snapshot: %+v", snapshot)
		}
	})
}

func FuzzHelloMessageRoundTrip(f *testing.F) {
	profile := testOnlySyntheticControllerProfileForFuzz()
	var seed [HelloMessageSize]byte
	if err := profile.EncodeHelloMessageInto(seed[:], 1); err != nil {
		f.Fatalf("seed Hello: %v", err)
	}
	f.Add(seed[:])
	f.Add([]byte{0x02, 0x20, 0x00, 0x1c})
	f.Fuzz(func(t *testing.T, wire []byte) {
		hello, err := DecodeHelloMessage(wire)
		if err != nil {
			return
		}
		identity := ControllerIdentity{
			VendorID:         hello.VendorID,
			ProductID:        hello.ProductID,
			DeviceReleaseBCD: 0,
			DeviceID:         hello.DeviceID,
			Firmware:         hello.Firmware,
			HardwareMajor:    hello.HardwareMajor,
			HardwareMinor:    hello.HardwareMinor,
		}
		profile, err := NewUnregisteredControllerProfile(
			identity,
			ControllerUSBConfig{MaxPower2mA: 1, OUTIntervalMS: 4, INIntervalMS: 4},
		)
		if err != nil {
			t.Fatalf("decoded Hello could not construct profile: %v", err)
		}
		var encoded [HelloMessageSize]byte
		if err := profile.EncodeHelloMessageInto(encoded[:], hello.Sequence); err != nil {
			t.Fatalf("decoded Hello could not be encoded: %v", err)
		}
		for index := range encoded {
			if encoded[index] != wire[index] {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	})
}

func testOnlySyntheticControllerProfileForFuzz() UnregisteredControllerProfile {
	profile, err := NewUnregisteredControllerProfile(
		testOnlySyntheticControllerIdentity(),
		ControllerUSBConfig{MaxPower2mA: 1, OUTIntervalMS: 4, INIntervalMS: 4},
	)
	if err != nil {
		panic(err)
	}
	return profile
}

func FuzzControllerHostCommandDecode(f *testing.F) {
	f.Add([]byte{0x04, 0x20, 0x01, 0x00})
	f.Add([]byte{0x05, 0x20, 0x01, 0x01, 0x00})
	f.Add([]byte{0x05, 0x20, 0x01, 0x01, 0x06})
	f.Fuzz(func(t *testing.T, wire []byte) {
		command, err := DecodeControllerHostCommand(wire)
		if err != nil {
			return
		}
		if err := command.validate(); err != nil {
			t.Fatalf("decoded host command is invalid: %v", err)
		}
	})
}

func FuzzExtendedStatusNoEventsMessageRoundTrip(f *testing.F) {
	f.Add([]byte{0x03, 0x20, 0x01, 0x04, 0x80, 0x00, 0x00, 0x00})
	f.Add([]byte{0x03, 0x20, 0xff, 0x04, 0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0x03, 0x20, 0x01, 0x04, 0x80, 0x02, 0x00, 0x00})
	f.Fuzz(func(t *testing.T, wire []byte) {
		sequence, body, err := DecodeExtendedStatusNoEventsMessage(wire)
		if err != nil {
			return
		}
		var encoded [ExtendedStatusNoEventsMessageSize]byte
		if err := EncodeExtendedStatusNoEventsMessageInto(encoded[:], sequence, body); err != nil {
			t.Fatalf("decoded status could not be encoded: %v", err)
		}
		for index := range encoded {
			if encoded[index] != wire[index] {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	})
}

func FuzzProtocolControlACKMessageRoundTrip(f *testing.F) {
	f.Add([]byte{
		0x01, 0x20, 0x01, 0x09,
		0x00, 0x04, 0x20, 0x3a, 0x00, 0x00, 0x00, 0x80, 0x00,
	})
	f.Add([]byte{
		0x01, 0x20, 0xff, 0x09,
		0x00, 0x23, 0x27, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})
	f.Add([]byte{0x01, 0x20, 0x01, 0x09, 0x01})
	f.Fuzz(func(t *testing.T, wire []byte) {
		sequence, body, err := DecodeProtocolControlACKMessage(wire)
		if err != nil {
			return
		}
		var encoded [ProtocolControlACKMessageSize]byte
		if err := EncodeProtocolControlACKMessageInto(encoded[:], sequence, body); err != nil {
			t.Fatalf("decoded ACK could not be encoded: %v", err)
		}
		for index := range encoded {
			if encoded[index] != wire[index] {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	})
}

func FuzzControllerDownstreamMessageDecode(f *testing.F) {
	f.Add([]byte{0x04, 0x20, 0x01, 0x00})
	f.Add([]byte{0x05, 0x20, 0x01, 0x01, byte(SetDeviceStateStart)})
	f.Add([]byte{
		0x01, 0x20, 0x01, 0x09,
		0x00, 0x04, 0x20, 0x3a, 0x00, 0x00, 0x00, 0x80, 0x00,
	})
	f.Add([]byte{0x09, 0x00, 0x01, 0x09, 0x00, 0x0f, 1, 2, 3, 4, 0, 0, 0})
	f.Add([]byte{0x0a, 0x20, 0x01, 0x03, 0x00, 0x01, 0x2f})
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, wire []byte) {
		message, err := DecodeControllerDownstreamMessage(wire)
		if err != nil {
			return
		}
		if message.Sequence == 0 || len(wire) < SinglePacketHeaderSize ||
			len(wire) > ControllerPersonaMaximumWireSize {
			t.Fatalf("accepted invalid envelope: kind=%d sequence=%d wire=% x",
				message.Kind, message.Sequence, wire)
		}
		switch message.Kind {
		case ControllerDownstreamLifecycle:
			if err := message.Lifecycle.validate(); err != nil {
				t.Fatalf("accepted invalid lifecycle command: %v", err)
			}
		case ControllerDownstreamProtocolControlACK:
			if err := message.ACK.Validate(); err != nil {
				t.Fatalf("accepted invalid ACK: %v", err)
			}
		case ControllerDownstreamDirectMotor:
			if err := message.DirectMotor.Validate(); err != nil {
				t.Fatalf("accepted invalid Direct Motor: %v", err)
			}
		case ControllerDownstreamGuideLED:
			if err := message.GuideLED.Validate(); err != nil {
				t.Fatalf("accepted invalid Guide LED: %v", err)
			}
		default:
			t.Fatalf("accepted unknown kind %d", message.Kind)
		}
	})
}

func FuzzControllerDownstreamPacketDecode(f *testing.F) {
	f.Add([]byte{0x04, 0x20, 0x01, 0x00})
	f.Add([]byte{
		0x04, 0x20, 0x01, 0x00,
		0x09, 0x00, 0x01, 0x09, 0x00, 0x0f, 1, 2, 3, 4, 0, 0, 0,
		0x0a, 0x20, 0x02, 0x03, 0x00, 0x01, 0x2f,
	})
	f.Add([]byte{})
	f.Add(make([]byte, ControllerDownstreamPacketMaximumSize+1))
	f.Fuzz(func(t *testing.T, wire []byte) {
		packet, err := DecodeControllerDownstreamPacket(wire)
		if err != nil {
			if packet.Len() != 0 {
				t.Fatalf("failed decode exposed %d messages", packet.Len())
			}
			return
		}
		if len(wire) == 0 || len(wire) > ControllerDownstreamPacketMaximumSize ||
			packet.Len() == 0 || packet.Len() > controllerDownstreamPacketMaximumMessages {
			t.Fatalf("accepted invalid packet: messages=%d wire=% x", packet.Len(), wire)
		}
		for index := 0; index < packet.Len(); index++ {
			message, ok := packet.Message(index)
			if !ok || message.Sequence == 0 {
				t.Fatalf("invalid message %d = (%+v, %t)", index, message, ok)
			}
			switch message.Kind {
			case ControllerDownstreamLifecycle:
				if err := message.Lifecycle.validate(); err != nil {
					t.Fatalf("invalid lifecycle message: %v", err)
				}
			case ControllerDownstreamProtocolControlACK:
				if err := message.ACK.Validate(); err != nil {
					t.Fatalf("invalid ACK message: %v", err)
				}
			case ControllerDownstreamDirectMotor:
				if err := message.DirectMotor.Validate(); err != nil {
					t.Fatalf("invalid Direct Motor message: %v", err)
				}
			case ControllerDownstreamGuideLED:
				if err := message.GuideLED.Validate(); err != nil {
					t.Fatalf("invalid Guide LED message: %v", err)
				}
			default:
				t.Fatalf("unknown message kind %d", message.Kind)
			}
		}
	})
}
