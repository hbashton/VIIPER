package auth

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

type Conn struct {
	net.Conn
	aead             cipher.AEAD
	sendCtr          uint64
	sendBuf          []byte
	recvHeader       [4]byte
	recvHeaderRead   int
	recvPacket       []byte
	recvPacketRead   int
	recvRecordLength int
	recvPlain        []byte
	sendMu           sync.Mutex
	recvMu           sync.Mutex
	sendErr          error
	recvErr          error
	sendExhausted    bool
}

const maxPacketSize = 2 * 1024 * 1024 // 2 MB

var (
	errPacketTooLarge = errors.New("authenticated stream packet is too large")
	errPacketTooShort = errors.New("authenticated stream packet is too short")
	errNonceExhausted = errors.New("authenticated stream nonce space is exhausted")
	errInvalidWrite   = errors.New("authenticated stream transport returned an invalid write count")
)

func WrapConn(conn net.Conn, sessionKey []byte) (net.Conn, error) {
	aead, err := chacha20poly1305.New(sessionKey)
	if err != nil {
		return nil, err
	}
	return &Conn{Conn: conn, aead: aead}, nil
}

func (s *Conn) Close() error {
	err := s.Conn.Close()
	// Closing the transport first releases any Read or Write currently holding
	// its lane lock. Once both lanes join, no cipher or record storage can still
	// be in use, so clear it before making subsequent calls fail closed.
	s.sendMu.Lock()
	s.recvMu.Lock()
	clear(s.sendBuf)
	clear(s.recvHeader[:])
	clear(s.recvPacket)
	s.sendBuf = nil
	s.recvPacket = nil
	s.recvPlain = nil
	s.recvHeaderRead = 0
	s.recvPacketRead = 0
	s.recvRecordLength = 0
	s.sendCtr = 0
	s.sendExhausted = false
	s.aead = nil
	if s.sendErr == nil {
		s.sendErr = net.ErrClosed
	}
	if s.recvErr == nil {
		s.recvErr = net.ErrClosed
	}
	s.recvMu.Unlock()
	s.sendMu.Unlock()
	return err
}

func (s *Conn) Write(p []byte) (int, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	if s.sendErr != nil {
		return 0, s.sendErr
	}
	if s.sendExhausted {
		return 0, errNonceExhausted
	}
	nonceSize := s.aead.NonceSize()
	recordOverhead := nonceSize + s.aead.Overhead()
	if len(p) > maxPacketSize-recordOverhead {
		return 0, errPacketTooLarge
	}
	recordLength := recordOverhead + len(p)
	totalLength := 4 + recordLength
	if cap(s.sendBuf) < totalLength {
		s.sendBuf = make([]byte, totalLength)
	}
	record := s.sendBuf[:4+nonceSize]
	nonce := record[4:]
	clear(nonce)
	binary.BigEndian.PutUint64(nonce[nonceSize-8:], s.sendCtr)
	record = s.aead.Seal(record, nonce, p, nil)
	binary.BigEndian.PutUint32(record[:4], uint32(len(record)-4))

	for written := 0; written < len(record); {
		remaining := record[written:]
		n, err := s.Conn.Write(remaining)
		if n < 0 || n > len(remaining) {
			s.sendErr = errInvalidWrite
			_ = s.Conn.Close()
			return 0, errInvalidWrite
		}
		written += n
		if err != nil {
			if written == len(record) {
				s.advanceSendCounter()
				s.sendErr = err
				_ = s.Conn.Close()
				return len(p), err
			}
			if written > 0 {
				s.sendErr = err
				_ = s.Conn.Close()
			}
			return 0, err
		}
		if n == 0 {
			s.sendErr = io.ErrNoProgress
			_ = s.Conn.Close()
			return 0, io.ErrNoProgress
		}
	}
	s.advanceSendCounter()

	return len(p), nil
}

func (s *Conn) advanceSendCounter() {
	if s.sendCtr == math.MaxUint64 {
		s.sendExhausted = true
		return
	}
	s.sendCtr++
}

func (s *Conn) Read(p []byte) (int, error) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()

	if len(p) == 0 {
		return 0, nil
	}
	for len(s.recvPlain) == 0 {
		if s.recvErr != nil {
			return 0, s.recvErr
		}
		if err := s.readRecord(); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.recvPlain)
	s.recvPlain = s.recvPlain[n:]
	return n, nil
}

func (s *Conn) readRecord() error {
	if s.recvHeaderRead < len(s.recvHeader) {
		n, err := io.ReadFull(s.Conn, s.recvHeader[s.recvHeaderRead:])
		s.recvHeaderRead += n
		if err != nil {
			// The bytes belong to authenticated framing, not to the caller's
			// plaintext buffer. Keep the offset so a cleared network deadline can
			// resume this record without parsing ciphertext as a new header.
			return err
		}
	}

	if s.recvRecordLength == 0 {
		wireLength := binary.BigEndian.Uint32(s.recvHeader[:])
		minimumLength := uint32(s.aead.NonceSize() + s.aead.Overhead())
		switch {
		case wireLength < minimumLength:
			s.recvErr = errPacketTooShort
			return s.recvErr
		case wireLength > maxPacketSize:
			s.recvErr = errPacketTooLarge
			return s.recvErr
		}
		s.recvRecordLength = int(wireLength)
		if cap(s.recvPacket) < s.recvRecordLength {
			s.recvPacket = make([]byte, s.recvRecordLength)
		}
		s.recvPacket = s.recvPacket[:s.recvRecordLength]
	}

	if s.recvPacketRead < s.recvRecordLength {
		n, err := io.ReadFull(s.Conn, s.recvPacket[s.recvPacketRead:])
		s.recvPacketRead += n
		if err != nil {
			return err
		}
	}

	nonceSize := s.aead.NonceSize()
	nonce := s.recvPacket[:nonceSize]
	ct := s.recvPacket[nonceSize:]

	// AEAD permits dst and ciphertext to overlap exactly. Decrypting in place
	// keeps one reusable record slab per authenticated stream instead of
	// allocating ciphertext and plaintext for every controller/media frame.
	// recvPlain is fully consumed before the slab is reused.
	pt, err := s.aead.Open(ct[:0], nonce, ct, nil)
	if err != nil {
		s.recvErr = err
		return err
	}
	s.recvPlain = pt
	s.recvHeaderRead = 0
	s.recvPacketRead = 0
	s.recvRecordLength = 0
	return nil
}
