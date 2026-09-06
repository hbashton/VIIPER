package xboxone

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/Alia5/VIIPER/controllerfeedback"
)

const (
	productionBrokerWireVersion    byte = 1
	productionBrokerHeaderSize          = 16
	productionBrokerMaximumPayload      = controllerPersonaCanonicalFeedbackWireSize

	productionBrokerConsumerReady     byte = 0x01
	productionBrokerSemanticInput     byte = 0x02
	productionBrokerCanonicalAck      byte = 0x03
	productionBrokerConsumerReadyAck  byte = 0x81
	productionBrokerSemanticInputAck  byte = 0x82
	productionBrokerCanonicalFeedback byte = 0x83

	productionBrokerRejected byte = 0
	productionBrokerAccepted byte = 1
)

const controllerPersonaCanonicalFeedbackWireSize = controllerfeedback.FrameSize

var (
	errProductionBrokerWire = errors.New(
		"xboxone: invalid production broker wire frame")
	productionBrokerMagic = [4]byte{'X', '1', 'B', 'R'}
)

type productionBrokerFrame struct {
	typeID      byte
	correlation uint64
	payload     []byte
}

// A single broker stream owns this reader. Payload storage remains caller-owned.
type productionBrokerFrameReader struct {
	reader io.Reader
	header [productionBrokerHeaderSize]byte
}

func newProductionBrokerFrameReader(reader io.Reader) *productionBrokerFrameReader {
	return &productionBrokerFrameReader{reader: reader}
}

type productionBrokerFrameWriter struct {
	mu      sync.Mutex
	writer  io.Writer
	scratch [productionBrokerHeaderSize + productionBrokerMaximumPayload]byte
}

func newProductionBrokerFrameWriter(
	writer io.Writer,
) *productionBrokerFrameWriter {
	return &productionBrokerFrameWriter{writer: writer}
}

func (writer *productionBrokerFrameWriter) write(
	typeID byte,
	correlation uint64,
	payload []byte,
) error {
	if writer == nil || writer.writer == nil ||
		!productionBrokerPayloadLengthValid(typeID, len(payload)) {
		return errProductionBrokerWire
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	header := writer.scratch[:productionBrokerHeaderSize]
	copy(header[:4], productionBrokerMagic[:])
	header[4] = productionBrokerWireVersion
	header[5] = typeID
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(payload)))
	binary.LittleEndian.PutUint64(header[8:16], correlation)
	copy(writer.scratch[productionBrokerHeaderSize:], payload)
	return writeProductionBrokerFull(writer.writer,
		writer.scratch[:productionBrokerHeaderSize+len(payload)])
}

func readProductionBrokerFrame(
	reader io.Reader,
	payloadScratch []byte,
) (productionBrokerFrame, error) {
	// One-shot convenience for cold callers. A production stream reuses its
	// reader so io.ReadFull cannot force a fresh header allocation per frame.
	return newProductionBrokerFrameReader(reader).read(payloadScratch)
}

func (reader *productionBrokerFrameReader) read(
	payloadScratch []byte,
) (productionBrokerFrame, error) {
	if reader == nil || reader.reader == nil {
		return productionBrokerFrame{}, errProductionBrokerWire
	}
	header := reader.header[:]
	if _, err := io.ReadFull(reader.reader, header); err != nil {
		return productionBrokerFrame{}, err
	}
	if header[0] != productionBrokerMagic[0] ||
		header[1] != productionBrokerMagic[1] ||
		header[2] != productionBrokerMagic[2] ||
		header[3] != productionBrokerMagic[3] ||
		header[4] != productionBrokerWireVersion {
		return productionBrokerFrame{}, errProductionBrokerWire
	}
	payloadLength := int(binary.LittleEndian.Uint16(header[6:8]))
	if payloadLength > len(payloadScratch) ||
		!productionBrokerPayloadLengthValid(header[5], payloadLength) {
		return productionBrokerFrame{}, fmt.Errorf(
			"%w: type=0x%02x payload=%d",
			errProductionBrokerWire, header[5], payloadLength)
	}
	payload := payloadScratch[:payloadLength]
	if _, err := io.ReadFull(reader.reader, payload); err != nil {
		return productionBrokerFrame{}, err
	}
	return productionBrokerFrame{
		typeID:      header[5],
		correlation: binary.LittleEndian.Uint64(header[8:16]),
		payload:     payload,
	}, nil
}

func productionBrokerPayloadLengthValid(typeID byte, length int) bool {
	switch typeID {
	case productionBrokerConsumerReady,
		productionBrokerConsumerReadyAck:
		return length == 0
	case productionBrokerSemanticInput:
		return length == SemanticInputWireSize
	case productionBrokerCanonicalAck,
		productionBrokerSemanticInputAck:
		return length == 1
	case productionBrokerCanonicalFeedback:
		return length == controllerPersonaCanonicalFeedbackWireSize
	default:
		return false
	}
}

func writeProductionBrokerFull(writer io.Writer, frame []byte) error {
	for len(frame) > 0 {
		written, err := writer.Write(frame)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrUnexpectedEOF
		}
		frame = frame[written:]
	}
	return nil
}
