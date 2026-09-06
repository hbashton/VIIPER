package xboxone

const (
	// GuideButtonStatusBodySize is the exact two-byte Command 7 payload.
	GuideButtonStatusBodySize = 2
	// GuideButtonStatusMessageSize includes the four-byte GIP header.
	GuideButtonStatusMessageSize = SinglePacketHeaderSize + GuideButtonStatusBodySize
	// MessageNumberGuideButtonStatus is primary system Command 7.
	MessageNumberGuideButtonStatus uint8 = 7
	// GuideButtonVirtualKey is VK_LWIN, the source-defined Xbox/Guide key.
	GuideButtonVirtualKey byte = 0x5b
)

// GuideButtonStatusV1 is the lossless semantic body of one Guide transition.
type GuideButtonStatusV1 struct {
	Down bool
}

// EncodeGuideButtonStatusMessageInto writes one exact upstream Command 7.
// Status is canonical zero/up or one/down and the virtual key is VK_LWIN.
func EncodeGuideButtonStatusMessageInto(
	dst []byte,
	sequence uint8,
	status GuideButtonStatusV1,
) error {
	if len(dst) != GuideButtonStatusMessageSize {
		return exactLengthError(
			"guide-button status message", len(dst), GuideButtonStatusMessageSize)
	}
	var wire [GuideButtonStatusMessageSize]byte
	header := SinglePacketHeader{
		DataClass: DataClassCommand, MessageNumber: MessageNumberGuideButtonStatus,
		System: true, Sequence: sequence, PayloadLength: GuideButtonStatusBodySize,
	}
	if err := EncodeSinglePacketHeaderInto(wire[:SinglePacketHeaderSize], header); err != nil {
		return err
	}
	if status.Down {
		wire[4] = 1
	}
	wire[5] = GuideButtonVirtualKey
	copy(dst, wire[:])
	return nil
}

// DecodeGuideButtonStatusMessage decodes only canonical up/down VK_LWIN
// status. Other status values and virtual keys are not Guide events.
func DecodeGuideButtonStatusMessage(
	wire []byte,
) (sequence uint8, status GuideButtonStatusV1, err error) {
	if len(wire) != GuideButtonStatusMessageSize {
		return 0, GuideButtonStatusV1{}, exactLengthError(
			"guide-button status message", len(wire), GuideButtonStatusMessageSize)
	}
	header, err := DecodeSinglePacketHeader(wire[:SinglePacketHeaderSize])
	if err != nil {
		return 0, GuideButtonStatusV1{}, err
	}
	if header.DataClass != DataClassCommand ||
		header.MessageNumber != MessageNumberGuideButtonStatus || !header.System ||
		header.AcknowledgementRequested || header.ExpansionIndex != 0 ||
		header.PayloadLength != GuideButtonStatusBodySize || wire[4] > 1 ||
		wire[5] != GuideButtonVirtualKey {
		return 0, GuideButtonStatusV1{}, ErrInvalidGuideButtonStatusMessage
	}
	return header.Sequence, GuideButtonStatusV1{Down: wire[4] == 1}, nil
}
