package auth_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/server/api/auth"
	"github.com/stretchr/testify/assert"
	"golang.org/x/crypto/chacha20poly1305"
)

type recordConn struct {
	bytes.Buffer
	writeCalls int
	maxWrite   int
}

type partialFailureConn struct {
	recordConn
	remaining int
	err       error
	closed    bool
}

type interruptedReadConn struct {
	recordConn
	beforeError int
	err         error
	interrupted bool
}

type fullWriteErrorConn struct {
	recordConn
	err    error
	closed bool
}

type firstWriteErrorConn struct {
	recordConn
	err   error
	first bool
}

type zeroProgressConn struct {
	recordConn
	closed bool
}

func (c *partialFailureConn) Write(p []byte) (int, error) {
	if c.remaining == 0 {
		return 0, c.err
	}
	if len(p) > c.remaining {
		p = p[:c.remaining]
	}
	n, _ := c.recordConn.Write(p)
	c.remaining -= n
	return n, nil
}

func (c *partialFailureConn) Close() error {
	c.closed = true
	return nil
}

func (c *interruptedReadConn) Read(p []byte) (int, error) {
	if c.interrupted {
		return c.recordConn.Read(p)
	}
	if c.beforeError == 0 {
		c.interrupted = true
		return 0, c.err
	}
	if len(p) > c.beforeError {
		p = p[:c.beforeError]
	}
	n, _ := c.recordConn.Read(p)
	c.beforeError -= n
	if c.beforeError == 0 {
		c.interrupted = true
		return n, c.err
	}
	return n, nil
}

func (c *fullWriteErrorConn) Write(p []byte) (int, error) {
	n, _ := c.recordConn.Write(p)
	return n, c.err
}

func (c *fullWriteErrorConn) Close() error {
	c.closed = true
	return nil
}

func (c *firstWriteErrorConn) Write(p []byte) (int, error) {
	if c.first {
		c.first = false
		return 0, c.err
	}
	return c.recordConn.Write(p)
}

func (c *zeroProgressConn) Write([]byte) (int, error) { return 0, nil }

func (c *zeroProgressConn) Close() error {
	c.closed = true
	return nil
}

type discardConn struct{}

func (discardConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (discardConn) Write(p []byte) (int, error)      { return len(p), nil }
func (discardConn) Close() error                     { return nil }
func (discardConn) LocalAddr() net.Addr              { return testAddr("local") }
func (discardConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (discardConn) SetDeadline(time.Time) error      { return nil }
func (discardConn) SetReadDeadline(time.Time) error  { return nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }

type segmentedReadConn struct {
	recordConn
	segments [][]byte
	index    int
	offset   int
}

func (c *segmentedReadConn) Read(p []byte) (int, error) {
	if c.index == len(c.segments) {
		return 0, io.EOF
	}
	segment := c.segments[c.index]
	n := copy(p, segment[c.offset:])
	c.offset += n
	if c.offset == len(segment) {
		c.index++
		c.offset = 0
	}
	return n, nil
}

func (c *recordConn) Write(p []byte) (int, error) {
	c.writeCalls++
	if c.maxWrite > 0 && len(p) > c.maxWrite {
		p = p[:c.maxWrite]
	}
	return c.Buffer.Write(p)
}

func (*recordConn) Close() error                     { return nil }
func (*recordConn) LocalAddr() net.Addr              { return testAddr("local") }
func (*recordConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (*recordConn) SetDeadline(time.Time) error      { return nil }
func (*recordConn) SetReadDeadline(time.Time) error  { return nil }
func (*recordConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

func legacyRolelessRecord(t *testing.T, key, payload []byte, counter uint64) []byte {
	t.Helper()
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	binary.BigEndian.PutUint64(nonce[4:], counter)
	record := make([]byte, 4, 4+len(nonce)+len(payload)+aead.Overhead())
	record = append(record, nonce...)
	record = aead.Seal(record, nonce, payload, nil)
	binary.BigEndian.PutUint32(record[:4], uint32(len(record)-4))
	return record
}

func TestConnNewServerAcceptsLegacyClientWireDomain(t *testing.T) {
	key, err := auth.DeriveKey("legacy-client-new-server")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("legacy client request")
	raw := &recordConn{}
	_, _ = raw.Buffer.Write(legacyRolelessRecord(t, key, payload, 0))
	server, err := auth.WrapServerConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	decoded := make([]byte, len(payload))
	if _, err = io.ReadFull(server, decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("legacy client plaintext=%q want=%q", decoded, payload)
	}
}

func TestConnNewClientRejectsLegacyRolelessServerDomain(t *testing.T) {
	key, err := auth.DeriveKey("new-client-legacy-server")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("legacy server response")
	raw := &recordConn{}
	_, _ = raw.Buffer.Write(legacyRolelessRecord(t, key, payload, 0))
	client, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	dst := bytes.Repeat([]byte{0xa5}, len(payload))
	if n, readErr := client.Read(dst); n != 0 || readErr == nil || !strings.Contains(readErr.Error(), "nonce direction=0, want 1") {
		t.Fatalf("legacy-server read=(%d, %v), want fail-closed direction error", n, readErr)
	}
	if !bytes.Equal(dst, bytes.Repeat([]byte{0xa5}, len(payload))) {
		t.Fatal("rejected legacy-server record changed the caller plaintext buffer")
	}
}

func TestConnUsesDisjointDirectionalNonceDomains(t *testing.T) {
	key, err := auth.DeriveKey("directional-nonce-domains")
	if err != nil {
		t.Fatal(err)
	}
	clientWire := &recordConn{}
	serverWire := &recordConn{}
	client, err := auth.WrapClientConn(clientWire, key)
	if err != nil {
		t.Fatal(err)
	}
	server, err := auth.WrapServerConn(serverWire, key)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("same-session duplex record")
	if _, err = client.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err = server.Write(payload); err != nil {
		t.Fatal(err)
	}

	clientNonce := clientWire.Bytes()[4:16]
	serverNonce := serverWire.Bytes()[4:16]
	if bytes.Equal(clientNonce, serverNonce) {
		t.Fatalf("client and server reused nonce %x with one session key", clientNonce)
	}
	if prefix := binary.BigEndian.Uint32(clientNonce[:4]); prefix != 0 {
		t.Fatalf("client nonce prefix=%d want=0", prefix)
	}
	if prefix := binary.BigEndian.Uint32(serverNonce[:4]); prefix != 1 {
		t.Fatalf("server nonce prefix=%d want=1", prefix)
	}
	if counter := binary.BigEndian.Uint64(clientNonce[4:]); counter != 0 {
		t.Fatalf("first client nonce counter=%d want=0", counter)
	}
	if counter := binary.BigEndian.Uint64(serverNonce[4:]); counter != 0 {
		t.Fatalf("first server nonce counter=%d want=0", counter)
	}
}

func TestConnSupportsConcurrentFullDuplexDirectionalTraffic(t *testing.T) {
	key, err := auth.DeriveKey("full-duplex-directions")
	if err != nil {
		t.Fatal(err)
	}
	clientTransport, serverTransport := net.Pipe()
	deadline := time.Now().Add(2 * time.Second)
	if err = clientTransport.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err = serverTransport.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	client, err := auth.WrapClientConn(clientTransport, key)
	if err != nil {
		t.Fatal(err)
	}
	server, err := auth.WrapServerConn(serverTransport, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	clientPayload := []byte("client controller state")
	serverPayload := []byte("server output media")
	type writeResult struct {
		name string
		n    int
		err  error
	}
	writes := make(chan writeResult, 2)
	go func() {
		n, writeErr := client.Write(clientPayload)
		writes <- writeResult{name: "client", n: n, err: writeErr}
	}()
	go func() {
		n, writeErr := server.Write(serverPayload)
		writes <- writeResult{name: "server", n: n, err: writeErr}
	}()

	gotClientPayload := make([]byte, len(clientPayload))
	if _, err = io.ReadFull(server, gotClientPayload); err != nil {
		t.Fatal(err)
	}
	gotServerPayload := make([]byte, len(serverPayload))
	if _, err = io.ReadFull(client, gotServerPayload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotClientPayload, clientPayload) || !bytes.Equal(gotServerPayload, serverPayload) {
		t.Fatalf("duplex plaintext=%q/%q want=%q/%q", gotClientPayload, gotServerPayload, clientPayload, serverPayload)
	}
	for range 2 {
		result := <-writes
		want := len(clientPayload)
		if result.name == "server" {
			want = len(serverPayload)
		}
		if result.n != want || result.err != nil {
			t.Fatalf("%s write=(%d, %v), want (%d, nil)", result.name, result.n, result.err, want)
		}
	}
}

func TestConnRejectsWrongDirectionalRole(t *testing.T) {
	key, err := auth.DeriveKey("wrong-directional-role")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		wrapOut func(net.Conn, []byte) (net.Conn, error)
		wrapIn  func(net.Conn, []byte) (net.Conn, error)
	}{
		{name: "server_wrapper_on_client", wrapOut: auth.WrapServerConn, wrapIn: auth.WrapServerConn},
		{name: "client_wrapper_on_server", wrapOut: auth.WrapClientConn, wrapIn: auth.WrapClientConn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := &recordConn{}
			sender, wrapErr := tc.wrapOut(raw, key)
			if wrapErr != nil {
				t.Fatal(wrapErr)
			}
			receiver, wrapErr := tc.wrapIn(raw, key)
			if wrapErr != nil {
				t.Fatal(wrapErr)
			}
			if _, writeErr := sender.Write([]byte("valid but wrong-direction record")); writeErr != nil {
				t.Fatal(writeErr)
			}
			if n, readErr := receiver.Read(make([]byte, 1)); n != 0 || readErr == nil || !strings.Contains(readErr.Error(), "nonce direction") {
				t.Fatalf("wrong-role read=(%d, %v), want (0, nonce direction error)", n, readErr)
			}
		})
	}
}

func TestConnRejectsAuthenticatedReplayAndOutOfOrderRecords(t *testing.T) {
	key, err := auth.DeriveKey("record-order")
	if err != nil {
		t.Fatal(err)
	}
	wireBuffer := &recordConn{}
	sender, err := auth.WrapClientConn(wireBuffer, key)
	if err != nil {
		t.Fatal(err)
	}
	firstPayload := []byte("first")
	secondPayload := []byte("second")
	if _, err = sender.Write(firstPayload); err != nil {
		t.Fatal(err)
	}
	firstLength := 4 + int(binary.BigEndian.Uint32(wireBuffer.Bytes()[:4]))
	if _, err = sender.Write(secondPayload); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), wireBuffer.Bytes()...)
	firstRecord := wire[:firstLength]
	secondRecord := wire[firstLength:]

	t.Run("replay", func(t *testing.T) {
		raw := &recordConn{}
		_, _ = raw.Buffer.Write(firstRecord)
		_, _ = raw.Buffer.Write(firstRecord)
		receiver, wrapErr := auth.WrapServerConn(raw, key)
		if wrapErr != nil {
			t.Fatal(wrapErr)
		}
		decoded := make([]byte, len(firstPayload))
		if _, readErr := io.ReadFull(receiver, decoded); readErr != nil {
			t.Fatal(readErr)
		}
		if n, readErr := receiver.Read(decoded[:1]); n != 0 || readErr == nil || !strings.Contains(readErr.Error(), "nonce counter=0, want 1") {
			t.Fatalf("replayed read=(%d, %v), want counter rejection", n, readErr)
		}
	})

	t.Run("out_of_order", func(t *testing.T) {
		raw := &recordConn{}
		_, _ = raw.Buffer.Write(secondRecord)
		_, _ = raw.Buffer.Write(firstRecord)
		receiver, wrapErr := auth.WrapServerConn(raw, key)
		if wrapErr != nil {
			t.Fatal(wrapErr)
		}
		if n, readErr := receiver.Read(make([]byte, 1)); n != 0 || readErr == nil || !strings.Contains(readErr.Error(), "nonce counter=1, want 0") {
			t.Fatalf("out-of-order read=(%d, %v), want counter rejection", n, readErr)
		}
	})
}

func TestConnCoalescesOneAuthenticatedRecordIntoOneWrite(t *testing.T) {
	key, err := auth.DeriveKey("coalesced-record")
	if err != nil {
		t.Fatal(err)
	}
	raw := &recordConn{}
	sender, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := auth.WrapServerConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("one input/media frame")
	if written, writeErr := sender.Write(payload); writeErr != nil || written != len(payload) {
		t.Fatalf("write=(%d, %v), want (%d, nil)", written, writeErr, len(payload))
	}
	if raw.writeCalls != 1 {
		t.Fatalf("authenticated frame used %d transport writes, want 1", raw.writeCalls)
	}
	decoded := make([]byte, len(payload))
	if _, err = io.ReadFull(receiver, decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded=%q want=%q", decoded, payload)
	}
}

func TestConnFinishesPartialUnderlyingWritesWithoutSplittingARecord(t *testing.T) {
	key, err := auth.DeriveKey("partial-record")
	if err != nil {
		t.Fatal(err)
	}
	raw := &recordConn{maxWrite: 3}
	sender, _ := auth.WrapClientConn(raw, key)
	receiver, _ := auth.WrapServerConn(raw, key)
	payload := []byte("partial writes are completed")
	if _, err = sender.Write(payload); err != nil {
		t.Fatal(err)
	}
	if raw.writeCalls <= 1 {
		t.Fatal("partial transport did not exercise the full-write loop")
	}
	decoded := make([]byte, len(payload))
	if _, err = io.ReadFull(receiver, decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded=%q want=%q", decoded, payload)
	}
}

func TestConnRejectsTruncatedAuthenticatedRecordWithoutPanicking(t *testing.T) {
	key, err := auth.DeriveKey("short-record")
	if err != nil {
		t.Fatal(err)
	}
	raw := &recordConn{}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 1)
	_, _ = raw.Buffer.Write(header[:])
	_ = raw.Buffer.WriteByte(0)
	receiver, _ := auth.WrapServerConn(raw, key)
	if _, err = receiver.Read(make([]byte, 1)); err == nil {
		t.Fatal("truncated authenticated record was accepted")
	}
}

func TestConnRejectsInvalidRecordLengthTerminallyBeforeAllocation(t *testing.T) {
	key, err := auth.DeriveKey("invalid-record-length")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		length uint32
	}{
		{name: "below_nonce_and_tag", length: 12 + 16 - 1},
		{name: "above_bound", length: 2*1024*1024 + 1},
		{name: "uint32_max", length: ^uint32(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := &recordConn{}
			var header [4]byte
			binary.BigEndian.PutUint32(header[:], tc.length)
			_, _ = raw.Buffer.Write(header[:])
			receiver, wrapErr := auth.WrapServerConn(raw, key)
			if wrapErr != nil {
				t.Fatal(wrapErr)
			}
			if n, readErr := receiver.Read(make([]byte, 1)); n != 0 || readErr == nil {
				t.Fatalf("invalid length read=(%d, %v), want (0, error)", n, readErr)
			}
			remaining := raw.Len()
			if n, readErr := receiver.Read(make([]byte, 1)); n != 0 || readErr == nil {
				t.Fatalf("repeated invalid length read=(%d, %v), want sticky error", n, readErr)
			}
			if raw.Len() != remaining {
				t.Fatal("terminal framing error consumed bytes on retry")
			}
		})
	}
}

func TestConnSerializesConcurrentRecordsWithMonotonicNonces(t *testing.T) {
	key, err := auth.DeriveKey("concurrent-records")
	if err != nil {
		t.Fatal(err)
	}
	raw := &recordConn{}
	sender, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	const records = 64
	start := make(chan struct{})
	errs := make(chan error, records)
	var writers sync.WaitGroup
	for id := 0; id < records; id++ {
		writers.Add(1)
		go func(id int) {
			defer writers.Done()
			<-start
			var payload [4]byte
			binary.BigEndian.PutUint32(payload[:], uint32(id))
			_, writeErr := sender.Write(payload[:])
			errs <- writeErr
		}(id)
	}
	close(start)
	writers.Wait()
	close(errs)
	for writeErr := range errs {
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}

	wire := append([]byte(nil), raw.Bytes()...)
	for counter := uint64(0); counter < records; counter++ {
		if len(wire) < 4 {
			t.Fatalf("record %d has no length prefix", counter)
		}
		length := int(binary.BigEndian.Uint32(wire[:4]))
		if length < 12 || len(wire) < 4+length {
			t.Fatalf("record %d length=%d remaining=%d", counter, length, len(wire))
		}
		nonceCounter := binary.BigEndian.Uint64(wire[8:16])
		if nonceCounter != counter {
			t.Fatalf("record %d nonce counter=%d", counter, nonceCounter)
		}
		wire = wire[4+length:]
	}
	if len(wire) != 0 {
		t.Fatalf("%d trailing authenticated bytes", len(wire))
	}

	receiver, err := auth.WrapServerConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[uint32]bool, records)
	for range records {
		var payload [4]byte
		if _, err = io.ReadFull(receiver, payload[:]); err != nil {
			t.Fatal(err)
		}
		seen[binary.BigEndian.Uint32(payload[:])] = true
	}
	if len(seen) != records {
		t.Fatalf("decoded %d unique records, want %d", len(seen), records)
	}
}

func TestConnClosesAfterPartialRecordFailure(t *testing.T) {
	key, err := auth.DeriveKey("terminal-partial-record")
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected transport failure")
	raw := &partialFailureConn{remaining: 7, err: wantErr}
	sender, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	if written, writeErr := sender.Write([]byte("frame")); written != 0 || !errors.Is(writeErr, wantErr) {
		t.Fatalf("partial write=(%d, %v), want (0, %v)", written, writeErr, wantErr)
	}
	if !raw.closed {
		t.Fatal("partially emitted authenticated record did not close the stream")
	}
	wireLength := raw.Len()
	if written, writeErr := sender.Write([]byte("retry")); written != 0 || !errors.Is(writeErr, wantErr) {
		t.Fatalf("retry=(%d, %v), want terminal (0, %v)", written, writeErr, wantErr)
	}
	if raw.Len() != wireLength {
		t.Fatal("terminal authenticated stream emitted bytes after partial failure")
	}
}

func TestConnReturnsCompletePlaintextCountWhenTransportReportsFullWriteAndError(t *testing.T) {
	key, err := auth.DeriveKey("full-record-error")
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("transport failed after accepting the record")
	raw := &fullWriteErrorConn{err: wantErr}
	sender, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("complete authenticated frame")
	if written, writeErr := sender.Write(payload); written != len(payload) || !errors.Is(writeErr, wantErr) {
		t.Fatalf("full write=(%d, %v), want (%d, %v)", written, writeErr, len(payload), wantErr)
	}
	if !raw.closed {
		t.Fatal("transport error after a complete record did not make the write side terminal")
	}
	wireLength := raw.Len()
	if written, writeErr := sender.Write([]byte("retry")); written != 0 || !errors.Is(writeErr, wantErr) {
		t.Fatalf("retry=(%d, %v), want terminal (0, %v)", written, writeErr, wantErr)
	}
	if raw.Len() != wireLength {
		t.Fatal("terminal stream emitted bytes after a full-record transport error")
	}

	receiver, err := auth.WrapServerConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	decoded := make([]byte, len(payload))
	if _, err = io.ReadFull(receiver, decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded=%q want=%q", decoded, payload)
	}
}

func TestConnRetriesRecordAfterZeroByteTransportErrorWithoutNonceReuseOnWire(t *testing.T) {
	key, err := auth.DeriveKey("zero-byte-retry")
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("temporary transport error")
	raw := &firstWriteErrorConn{err: wantErr, first: true}
	sender, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("retry safely")
	if written, writeErr := sender.Write(payload); written != 0 || !errors.Is(writeErr, wantErr) {
		t.Fatalf("first write=(%d, %v), want (0, %v)", written, writeErr, wantErr)
	}
	if raw.Len() != 0 {
		t.Fatal("zero-byte transport error emitted authenticated wire data")
	}
	if written, writeErr := sender.Write(payload); written != len(payload) || writeErr != nil {
		t.Fatalf("retry=(%d, %v), want (%d, nil)", written, writeErr, len(payload))
	}
	if counter := binary.BigEndian.Uint64(raw.Bytes()[8:16]); counter != 0 {
		t.Fatalf("retried record nonce counter=%d want=0", counter)
	}
}

func TestConnClosesAfterZeroProgressWrite(t *testing.T) {
	key, err := auth.DeriveKey("zero-progress")
	if err != nil {
		t.Fatal(err)
	}
	raw := &zeroProgressConn{}
	sender, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	if written, writeErr := sender.Write([]byte("frame")); written != 0 || !errors.Is(writeErr, io.ErrNoProgress) {
		t.Fatalf("zero-progress write=(%d, %v), want (0, %v)", written, writeErr, io.ErrNoProgress)
	}
	if !raw.closed {
		t.Fatal("zero-progress transport did not close the unrecoverable stream")
	}
	if written, writeErr := sender.Write([]byte("retry")); written != 0 || !errors.Is(writeErr, io.ErrNoProgress) {
		t.Fatalf("retry=(%d, %v), want terminal (0, %v)", written, writeErr, io.ErrNoProgress)
	}
}

func TestConnRejectsOversizedRecordBeforeTransportWrite(t *testing.T) {
	key, err := auth.DeriveKey("oversized-record")
	if err != nil {
		t.Fatal(err)
	}
	raw := &recordConn{}
	sender, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	if written, writeErr := sender.Write(make([]byte, 2*1024*1024)); written != 0 || writeErr == nil {
		t.Fatalf("oversized write=(%d, %v), want rejection", written, writeErr)
	}
	if raw.writeCalls != 0 {
		t.Fatalf("oversized record reached transport in %d write(s)", raw.writeCalls)
	}
}

func TestConnAcceptsExactMaximumRecordBound(t *testing.T) {
	key, err := auth.DeriveKey("maximum-record")
	if err != nil {
		t.Fatal(err)
	}
	raw := &recordConn{}
	sender, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	// The 2 MiB bound includes the 12-byte nonce and 16-byte Poly1305 tag.
	payload := make([]byte, 2*1024*1024-12-16)
	payload[0], payload[len(payload)-1] = 0x5a, 0xa5
	if written, writeErr := sender.Write(payload); written != len(payload) || writeErr != nil {
		t.Fatalf("maximum write=(%d, %v), want (%d, nil)", written, writeErr, len(payload))
	}
	if got := binary.BigEndian.Uint32(raw.Bytes()[:4]); got != 2*1024*1024 {
		t.Fatalf("maximum wire record length=%d", got)
	}

	receiver, err := auth.WrapServerConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	decoded := make([]byte, len(payload))
	if _, err = io.ReadFull(receiver, decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatal("maximum authenticated record changed during round trip")
	}
}

func TestConnResumesInterruptedAuthenticatedFramingWithoutExposingWireBytes(t *testing.T) {
	key, err := auth.DeriveKey("interrupted-framing")
	if err != nil {
		t.Fatal(err)
	}
	wireBuffer := &recordConn{}
	sender, err := auth.WrapClientConn(wireBuffer, key)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("only authenticated plaintext may reach the caller")
	if _, err = sender.Write(payload); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), wireBuffer.Bytes()...)
	wantErr := errors.New("injected read deadline")

	for _, tc := range []struct {
		name        string
		beforeError int
	}{
		{name: "partial_header", beforeError: 2},
		{name: "partial_record", beforeError: 4 + 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := &interruptedReadConn{beforeError: tc.beforeError, err: wantErr}
			_, _ = raw.Buffer.Write(wire)
			receiver, wrapErr := auth.WrapServerConn(raw, key)
			if wrapErr != nil {
				t.Fatal(wrapErr)
			}
			dst := bytes.Repeat([]byte{0xa5}, len(payload))
			if n, readErr := receiver.Read(dst[:1]); n != 0 || !errors.Is(readErr, wantErr) {
				t.Fatalf("interrupted read=(%d, %v), want (0, %v)", n, readErr, wantErr)
			}
			if !bytes.Equal(dst, bytes.Repeat([]byte{0xa5}, len(payload))) {
				t.Fatal("unauthenticated framing bytes changed the caller buffer")
			}
			if _, readErr := io.ReadFull(receiver, dst); readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(dst, payload) {
				t.Fatalf("resumed plaintext=%q want=%q", dst, payload)
			}
		})
	}
}

func TestConnSkipsAuthenticatedEmptyRecordAndZeroLengthReadDoesNotConsumeWire(t *testing.T) {
	key, err := auth.DeriveKey("empty-record")
	if err != nil {
		t.Fatal(err)
	}
	raw := &recordConn{}
	sender, err := auth.WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	if written, writeErr := sender.Write(nil); written != 0 || writeErr != nil {
		t.Fatalf("empty write=(%d, %v), want (0, nil)", written, writeErr)
	}
	payload := []byte("after empty")
	if _, err = sender.Write(payload); err != nil {
		t.Fatal(err)
	}
	wireLength := raw.Len()
	receiver, err := auth.WrapServerConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	if n, readErr := receiver.Read(nil); n != 0 || readErr != nil {
		t.Fatalf("zero-length read=(%d, %v), want (0, nil)", n, readErr)
	}
	if raw.Len() != wireLength {
		t.Fatal("zero-length destination consumed authenticated wire data")
	}
	dst := make([]byte, len(payload))
	if n, readErr := receiver.Read(dst); n != len(payload) || readErr != nil {
		t.Fatalf("read after empty record=(%d, %v), want (%d, nil)", n, readErr, len(payload))
	}
	if !bytes.Equal(dst, payload) {
		t.Fatalf("read after empty record=%q want=%q", dst, payload)
	}
}

func TestConnReadCopiesPlaintextOutOfReusableRecordSlab(t *testing.T) {
	key, err := auth.DeriveKey("retained-read-buffer")
	if err != nil {
		t.Fatal(err)
	}
	raw := &recordConn{}
	sender, _ := auth.WrapClientConn(raw, key)
	receiver, _ := auth.WrapServerConn(raw, key)
	firstWant := []byte("first-frame")
	secondWant := []byte("second-frame")
	_, _ = sender.Write(firstWant)
	first := make([]byte, len(firstWant))
	if _, err = io.ReadFull(receiver, first); err != nil {
		t.Fatal(err)
	}
	_, _ = sender.Write(secondWant)
	second := make([]byte, len(secondWant))
	if _, err = io.ReadFull(receiver, second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, firstWant) || !bytes.Equal(second, secondWant) {
		t.Fatalf("retained=%q/%q want=%q/%q", first, second, firstWant, secondWant)
	}
}

func TestConnReadPreservesFourSegmentClientWireCompatibility(t *testing.T) {
	key, err := auth.DeriveKey("segmented-client-record")
	if err != nil {
		t.Fatal(err)
	}
	wireBuffer := &recordConn{}
	sender, _ := auth.WrapClientConn(wireBuffer, key)
	payload := []byte("header nonce ciphertext tag remain one protocol record")
	if _, err = sender.Write(payload); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), wireBuffer.Bytes()...)
	tagStart := len(wire) - 16
	raw := &segmentedReadConn{segments: [][]byte{
		wire[:4], wire[4:16], wire[16:tagStart], wire[tagStart:],
	}}
	receiver, _ := auth.WrapServerConn(raw, key)
	decoded := make([]byte, len(payload))
	if _, err = io.ReadFull(receiver, decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded=%q want=%q", decoded, payload)
	}
}

func BenchmarkConnWriteAuthenticatedRecord(b *testing.B) {
	key, err := auth.DeriveKey("authenticated-write-benchmark")
	if err != nil {
		b.Fatal(err)
	}
	wrapped, err := auth.WrapClientConn(discardConn{}, key)
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 512)
	if _, err = wrapped.Write(payload); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err = wrapped.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func TestConn(t *testing.T) {

	type testCase struct {
		name        string
		setupFn     func(clientConn net.Conn, serverConn net.Conn) (clientKey []byte, serverKey []byte)
		input       []byte
		expected    []byte
		expectedErr error
	}

	testCases := []testCase{
		{
			name: "valid read",
			setupFn: func(clientConn, serverConn net.Conn) (clientKey []byte, serverKey []byte) {
				password := "test123"
				key, err := auth.DeriveKey(password)
				if err != nil {
					t.Fatalf("failed to derive key: %v", err)
				}
				return key, key
			},
			input:    []byte("Hello, World!"),
			expected: []byte("Hello, World!"),
		},
		{
			name: "Differing Keys",
			setupFn: func(clientConn, serverConn net.Conn) (clientKey []byte, serverKey []byte) {
				key, err := auth.DeriveKey("test123")
				if err != nil {
					t.Fatalf("failed to derive key: %v", err)
				}
				key2, err := auth.DeriveKey("123test")
				if err != nil {
					t.Fatalf("failed to derive key: %v", err)
				}
				return key, key2
			},
			input:       []byte("x"),
			expected:    nil,
			expectedErr: errors.New("chacha20poly1305: message authentication failed"),
		},
		{
			name: "bad key length (client)",
			setupFn: func(clientConn, serverConn net.Conn) (clientKey []byte, serverKey []byte) {
				key, err := auth.DeriveKey("test123")
				if err != nil {
					t.Fatalf("failed to derive key: %v", err)
				}
				return []byte{1, 2, 3}, key // invalid key length on client
			},
			input:       []byte("x"),
			expected:    nil,
			expectedErr: errors.New("chacha20poly1305: bad key length"),
		},
		{
			name: "bad key length (server)",
			setupFn: func(clientConn, serverConn net.Conn) (clientKey []byte, serverKey []byte) {
				key, err := auth.DeriveKey("test123")
				if err != nil {
					t.Fatalf("failed to derive key: %v", err)
				}
				return key, []byte{1, 2, 3} // invalid key length on server
			},
			input:       []byte("x"),
			expected:    nil,
			expectedErr: errors.New("chacha20poly1305: bad key length"),
		},
		{
			name: "client closed before write",
			setupFn: func(clientConn, serverConn net.Conn) (clientKey []byte, serverKey []byte) {
				key, err := auth.DeriveKey("test123")
				if err != nil {
					t.Fatalf("failed to derive key: %v", err)
				}
				_ = clientConn.Close()
				return key, key
			},
			input:       []byte("x"),
			expected:    nil,
			expectedErr: errors.New("use of closed network connection"),
		},
		{
			name: "server closed before read",
			setupFn: func(clientConn, serverConn net.Conn) (clientKey []byte, serverKey []byte) {
				key, err := auth.DeriveKey("test123")
				if err != nil {
					t.Fatalf("failed to derive key: %v", err)
				}
				_ = serverConn.Close()
				return key, key
			},
			input:    []byte("x"),
			expected: nil,
			// just check for error, linux/win differ
			expectedErr: errors.New(""),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("failed to start test server: %v", err)
			}
			clientConn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatalf("failed to connect to test server: %v", err)
			}
			serverConn, err := ln.Accept()
			if err != nil {
				t.Fatalf("failed to accept connection: %v", err)
			}
			defer ln.Close()         //nolint:errcheck
			defer clientConn.Close() //nolint:errcheck
			defer serverConn.Close() //nolint:errcheck

			var clientKey, serverKey []byte
			if tc.setupFn != nil {
				clientKey, serverKey = tc.setupFn(clientConn, serverConn)
			}

			wrappedServerConn, err := auth.WrapServerConn(serverConn, serverKey)
			if err != nil {
				if tc.expectedErr != nil {
					assert.ErrorContains(t, err, tc.expectedErr.Error())
				} else {
					t.Fatalf("failed to wrap server conn: %v", err)
				}
				return
			}
			wrappedClientConn, err := auth.WrapClientConn(clientConn, clientKey)
			if err != nil {
				if tc.expectedErr != nil {
					assert.ErrorContains(t, err, tc.expectedErr.Error())
				} else {
					t.Fatalf("failed to wrap client conn: %v", err)
				}
				return
			}

			_, err = wrappedClientConn.Write(tc.input)
			if err != nil {
				if tc.expectedErr != nil {
					assert.ErrorContains(t, err, tc.expectedErr.Error())
				} else {
					t.Fatalf("failed to wrap client conn: %v", err)
				}
				return
			}
			readSize := len(tc.expected)
			if tc.expectedErr != nil && readSize == 0 {
				readSize = 1
			}
			buf := make([]byte, readSize)
			_, err = wrappedServerConn.Read(buf)
			if err != nil {
				if tc.expectedErr != nil {
					assert.ErrorContains(t, err, tc.expectedErr.Error())
				} else {
					t.Errorf("server read error: %v", err)
				}
				return
			}
			if tc.expectedErr != nil {
				t.Fatalf("server read succeeded, want error containing %q", tc.expectedErr)
			}
			assert.Equal(t, tc.expected, buf)

		})
	}

}
