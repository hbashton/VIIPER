package auth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

func TestConnReadAllocatesZeroAfterWarmup(t *testing.T) {
	key := make([]byte, chacha20poly1305.KeySize)
	source := &memoryAuthConn{}
	writer, err := WrapConn(source, key, Client)
	if err != nil {
		t.Fatal(err)
	}
	var payload [72]byte
	for i := 0; i < 1010; i++ {
		if _, err := writer.Write(payload[:]); err != nil {
			t.Fatal(err)
		}
	}
	receiver, err := WrapConn(&memoryAuthConn{read: bytes.NewReader(source.written.Bytes())}, key, Server)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Read(payload[:]); err != nil {
		t.Fatal(err)
	}
	var readErr error
	allocations := testing.AllocsPerRun(1000, func() { _, readErr = receiver.Read(payload[:]) })
	if readErr != nil {
		t.Fatal(readErr)
	}
	if allocations != 0 {
		t.Fatalf("authenticated 72-byte reads allocate %v times, want 0", allocations)
	}
}

func TestConnWriteAllocatesZeroAfterWarmup(t *testing.T) {
	key := make([]byte, chacha20poly1305.KeySize)
	wrapped, err := WrapConn(discardAuthConn{}, key, Client)
	if err != nil {
		t.Fatal(err)
	}
	var payload [72]byte
	if _, err := wrapped.Write(payload[:]); err != nil {
		t.Fatal(err)
	}
	var writeErr error
	allocations := testing.AllocsPerRun(1_000, func() {
		_, writeErr = wrapped.Write(payload[:])
	})
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if allocations != 0 {
		t.Fatalf("authenticated 72-byte writes allocate %v times, want 0",
			allocations)
	}
}

func TestConnRejectsReplayedRecordNonce(t *testing.T) {
	key := make([]byte, chacha20poly1305.KeySize)
	for index := range key {
		key[index] = byte(index + 1)
	}
	record := testEncryptedRecord(t, key, 0, []byte{0x5a})
	serverRaw, clientRaw := net.Pipe()
	defer serverRaw.Close()
	defer clientRaw.Close()
	server, err := WrapConn(serverRaw, key, Server)
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() {
		if err := writeFull(clientRaw, record); err != nil {
			writeDone <- err
			return
		}
		writeDone <- writeFull(clientRaw, record)
	}()

	var value [1]byte
	if _, err := server.Read(value[:]); err != nil || value[0] != 0x5a {
		t.Fatalf("first Read = (% x, %v)", value, err)
	}
	if _, err := server.Read(value[:]); !errors.Is(
		err, errInvalidEncryptedRecord) {
		t.Fatalf("replayed Read error = %v, want %v",
			err, errInvalidEncryptedRecord)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestConnRejectsUndersizedRecordWithoutPanic(t *testing.T) {
	key := make([]byte, chacha20poly1305.KeySize)
	serverRaw, clientRaw := net.Pipe()
	defer serverRaw.Close()
	defer clientRaw.Close()
	server, err := WrapConn(serverRaw, key, Server)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], 1)
		_ = writeFull(clientRaw, header[:])
	}()
	var value [1]byte
	if _, err := server.Read(value[:]); !errors.Is(
		err, errInvalidEncryptedRecord) {
		t.Fatalf("undersized Read error = %v, want %v",
			err, errInvalidEncryptedRecord)
	}
}

func testEncryptedRecord(t *testing.T, key []byte, counter uint64,
	plaintext []byte,
) []byte {
	t.Helper()
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(nonce[4:], counter)
	ciphertext := aead.Seal(nil, nonce[:], plaintext, nil)
	record := make([]byte, 4+len(nonce)+len(ciphertext))
	binary.BigEndian.PutUint32(record[:4],
		uint32(len(nonce)+len(ciphertext)))
	copy(record[4:], nonce[:])
	copy(record[4+len(nonce):], ciphertext)
	return record
}

type discardAuthConn struct{}

func (discardAuthConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (discardAuthConn) Write(value []byte) (int, error)  { return len(value), nil }
func (discardAuthConn) Close() error                     { return nil }
func (discardAuthConn) LocalAddr() net.Addr              { return authTestAddr("local") }
func (discardAuthConn) RemoteAddr() net.Addr             { return authTestAddr("remote") }
func (discardAuthConn) SetDeadline(time.Time) error      { return nil }
func (discardAuthConn) SetReadDeadline(time.Time) error  { return nil }
func (discardAuthConn) SetWriteDeadline(time.Time) error { return nil }

type authTestAddr string

func (address authTestAddr) Network() string { return "test" }
func (address authTestAddr) String() string  { return string(address) }
