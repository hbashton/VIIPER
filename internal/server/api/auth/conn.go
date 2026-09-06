package auth

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"
)

type Conn struct {
	net.Conn
	aead            cipher.AEAD
	sendCtr         uint64
	recvCtr         uint64
	sendPrefix      uint32
	recvPrefix      uint32
	failed          atomic.Bool
	writeMu         sync.Mutex
	readMu          sync.Mutex
	readHeader      [4]byte
	writeBuf        []byte
	readCipherBuf   []byte
	readPlainBuf    []byte
	readPlainOffset int
	readPlainLength int
}

const maxPacketSize = 2 * 1024 * 1024 // 2 MB

var errInvalidEncryptedRecord = errors.New("auth: invalid encrypted record")

type Role uint8

const (
	Client Role = 1
	Server Role = 2
)

// WrapConn is v2-only. The role is explicit so two directions never share
// a (session key, nonce) pair. V1 peers must fail during handshake negotiation.
func WrapConn(conn net.Conn, sessionKey []byte, role Role) (net.Conn, error) {
	if role != Client && role != Server {
		return nil, errors.New("auth: explicit client or server role required")
	}
	if conn == nil {
		return nil, errors.New("auth: nil connection")
	}
	aead, err := chacha20poly1305.New(sessionKey)
	if err != nil {
		return nil, err
	}
	return &Conn{
		Conn: conn, aead: aead,
		sendPrefix: uint32(role - 1), recvPrefix: uint32(2 - role),
		writeBuf: make([]byte, 512), readCipherBuf: make([]byte, 512),
		readPlainBuf: make([]byte, 512),
	}, nil
}

// VIIPERAuthenticated marks a connection which completed the authenticated
// handshake and is now protected by the session AEAD. Device-specific stream
// owners can require this fact without importing the API/auth package.
func (s *Conn) VIIPERAuthenticated() bool { return s != nil }

func (s *Conn) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.failed.Load() {
		return 0, errInvalidEncryptedRecord
	}
	defer func() {
		if err != nil {
			s.failed.Store(true)
		}
	}()
	if s.sendCtr == math.MaxUint64 {
		return 0, errInvalidEncryptedRecord
	}
	if len(p) > maxPacketSize-12-s.aead.Overhead() {
		return 0, errInvalidEncryptedRecord
	}

	recordLength := 12 + len(p) + s.aead.Overhead()
	totalLength := 4 + recordLength
	s.writeBuf = ensureRecordCapacity(s.writeBuf, totalLength)
	record := s.writeBuf[:totalLength]
	binary.BigEndian.PutUint32(record[:4], uint32(recordLength))
	nonce := record[4:16]
	binary.BigEndian.PutUint32(nonce[:4], s.sendPrefix)
	binary.BigEndian.PutUint64(nonce[4:], s.sendCtr)
	s.sendCtr++

	sealed := s.aead.Seal(record[:16], nonce, p, nil)
	if len(sealed) != totalLength {
		return 0, errInvalidEncryptedRecord
	}
	if err := writeFull(s.Conn, sealed); err != nil {
		return 0, err
	}

	return len(p), nil
}

func (s *Conn) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.failed.Load() {
		return 0, errInvalidEncryptedRecord
	}
	defer func() {
		if err != nil && err != io.EOF {
			s.failed.Store(true)
		}
	}()
	for s.readPlainOffset >= s.readPlainLength {
		if _, err := io.ReadFull(s.Conn, s.readHeader[:]); err != nil {
			return 0, err
		}
		length := binary.BigEndian.Uint32(s.readHeader[:])
		if length < uint32(12+s.aead.Overhead()) ||
			length > maxPacketSize {
			return 0, errInvalidEncryptedRecord
		}

		encryptedLength := int(length)
		s.readCipherBuf = ensureRecordCapacity(
			s.readCipherBuf, encryptedLength)
		pkt := s.readCipherBuf[:encryptedLength]
		if _, err := io.ReadFull(s.Conn, pkt); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}

		nonce := pkt[:12]
		ct := pkt[12:]
		if binary.BigEndian.Uint32(nonce[:4]) != s.recvPrefix ||
			binary.BigEndian.Uint64(nonce[4:]) != s.recvCtr ||
			s.recvCtr == math.MaxUint64 {
			return 0, errInvalidEncryptedRecord
		}

		plainLength := len(ct) - s.aead.Overhead()
		s.readPlainBuf = ensureRecordCapacity(s.readPlainBuf, plainLength)
		pt, err := s.aead.Open(s.readPlainBuf[:0], nonce, ct, nil)
		if err != nil {
			return 0, err
		}
		s.recvCtr++
		s.readPlainOffset = 0
		s.readPlainLength = len(pt)
	}
	read := copy(p, s.readPlainBuf[s.readPlainOffset:s.readPlainLength])
	s.readPlainOffset += read
	return read, nil
}

func ensureRecordCapacity(buffer []byte, required int) []byte {
	if cap(buffer) >= required {
		return buffer[:required]
	}
	size := cap(buffer)
	if size == 0 {
		size = 512
	}
	for size < required {
		size *= 2
	}
	return make([]byte, required, size)
}

func writeFull(writer io.Writer, buffer []byte) error {
	for len(buffer) != 0 {
		written, err := writer.Write(buffer)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(buffer) {
			return io.ErrUnexpectedEOF
		}
		buffer = buffer[written:]
	}
	return nil
}
