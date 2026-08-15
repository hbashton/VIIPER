package auth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"testing"
	"time"
)

type internalRecordConn struct {
	bytes.Buffer
}

type loopingInternalConn struct {
	record []byte
	offset int
}

func (*internalRecordConn) Close() error                     { return nil }
func (*internalRecordConn) LocalAddr() net.Addr              { return internalTestAddr("local") }
func (*internalRecordConn) RemoteAddr() net.Addr             { return internalTestAddr("remote") }
func (*internalRecordConn) SetDeadline(time.Time) error      { return nil }
func (*internalRecordConn) SetReadDeadline(time.Time) error  { return nil }
func (*internalRecordConn) SetWriteDeadline(time.Time) error { return nil }

func (c *loopingInternalConn) Read(p []byte) (int, error) {
	if c.offset == len(c.record) {
		c.offset = 0
	}
	n := copy(p, c.record[c.offset:])
	c.offset += n
	return n, nil
}
func (*loopingInternalConn) Write(p []byte) (int, error) { return len(p), nil }
func (*loopingInternalConn) Close() error                { return nil }
func (*loopingInternalConn) LocalAddr() net.Addr         { return internalTestAddr("local") }
func (*loopingInternalConn) RemoteAddr() net.Addr        { return internalTestAddr("remote") }
func (*loopingInternalConn) SetDeadline(time.Time) error { return nil }
func (*loopingInternalConn) SetReadDeadline(time.Time) error {
	return nil
}
func (*loopingInternalConn) SetWriteDeadline(time.Time) error {
	return nil
}

type internalTestAddr string

func (a internalTestAddr) Network() string { return string(a) }
func (a internalTestAddr) String() string  { return string(a) }

type blockingInternalConn struct {
	readStarted  chan struct{}
	writeStarted chan struct{}
	closed       chan struct{}
	readOnce     sync.Once
	writeOnce    sync.Once
	closeOnce    sync.Once
}

func (c *blockingInternalConn) Read([]byte) (int, error) {
	c.readOnce.Do(func() { close(c.readStarted) })
	<-c.closed
	return 0, net.ErrClosed
}

func (c *blockingInternalConn) Write([]byte) (int, error) {
	c.writeOnce.Do(func() { close(c.writeStarted) })
	<-c.closed
	return 0, net.ErrClosed
}

func (c *blockingInternalConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (*blockingInternalConn) LocalAddr() net.Addr              { return internalTestAddr("local") }
func (*blockingInternalConn) RemoteAddr() net.Addr             { return internalTestAddr("remote") }
func (*blockingInternalConn) SetDeadline(time.Time) error      { return nil }
func (*blockingInternalConn) SetReadDeadline(time.Time) error  { return nil }
func (*blockingInternalConn) SetWriteDeadline(time.Time) error { return nil }

func TestConnUsesFinalCounterNonceExactlyOnceBeforeExhaustion(t *testing.T) {
	key, err := DeriveKey("nonce-exhaustion")
	if err != nil {
		t.Fatal(err)
	}
	raw := &internalRecordConn{}
	wrapper, err := WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	conn := wrapper.(*Conn)
	conn.sendCtr = math.MaxUint64
	payload := []byte("final nonce")
	if written, writeErr := conn.Write(payload); written != len(payload) || writeErr != nil {
		t.Fatalf("final nonce write=(%d, %v), want (%d, nil)", written, writeErr, len(payload))
	}
	wire := append([]byte(nil), raw.Bytes()...)
	if got := binary.BigEndian.Uint64(wire[8:16]); got != math.MaxUint64 {
		t.Fatalf("final nonce counter=%d want=%d", got, uint64(math.MaxUint64))
	}
	if written, writeErr := conn.Write([]byte("must not wrap")); written != 0 || !errors.Is(writeErr, errNonceExhausted) {
		t.Fatalf("exhausted write=(%d, %v), want (0, %v)", written, writeErr, errNonceExhausted)
	}
	if !bytes.Equal(raw.Bytes(), wire) {
		t.Fatal("nonce-exhausted write emitted wire data")
	}

	receiverWrapper, err := WrapServerConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	receiver := receiverWrapper.(*Conn)
	receiver.recvCtr = math.MaxUint64
	decoded := make([]byte, len(payload))
	if _, err = io.ReadFull(receiverWrapper, decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("final nonce plaintext=%q want=%q", decoded, payload)
	}
	_, _ = raw.Buffer.Write(wire)
	if n, readErr := receiver.Read(decoded[:1]); n != 0 || !errors.Is(readErr, errRecvExhausted) {
		t.Fatalf("receive after final nonce=(%d, %v), want (0, %v)", n, readErr, errRecvExhausted)
	}
}

func BenchmarkConnReadAuthenticatedRecord(b *testing.B) {
	key, err := DeriveKey("authenticated-read-benchmark")
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 512)
	wire := &internalRecordConn{}
	sender, err := WrapClientConn(wire, key)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = sender.Write(payload); err != nil {
		b.Fatal(err)
	}
	raw := &loopingInternalConn{record: append([]byte(nil), wire.Bytes()...)}
	wrapper, err := WrapServerConn(raw, key)
	if err != nil {
		b.Fatal(err)
	}
	receiver := wrapper.(*Conn)
	dst := make([]byte, len(payload))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		// The transport loops one authenticated counter-zero fixture to isolate
		// decrypt/copy cost. Reset only the expected test counter; production
		// streams reject this replay.
		receiver.recvCtr = 0
		receiver.recvExhausted = false
		if _, err = io.ReadFull(receiver, dst); err != nil {
			b.Fatal(err)
		}
	}
}

func TestConnCloseJoinsLanesAndClearsRecordAndCipherState(t *testing.T) {
	key, err := DeriveKey("clear-connection-state")
	if err != nil {
		t.Fatal(err)
	}
	raw := &internalRecordConn{}
	senderWrapper, err := WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	sender := senderWrapper.(*Conn)
	largePayload := bytes.Repeat([]byte{0x5a}, 4096)
	smallPayload := []byte("small sensitive controller state")
	if _, err = sender.Write(largePayload); err != nil {
		t.Fatal(err)
	}
	if _, err = sender.Write(smallPayload); err != nil {
		t.Fatal(err)
	}
	sendBacking := sender.sendBuf[:cap(sender.sendBuf)]
	if err = sender.Close(); err != nil {
		t.Fatal(err)
	}
	if sender.aead != nil || sender.sendBuf != nil || sender.sendNoncePrefix != 0 || sender.sendCtr != 0 {
		t.Fatal("close retained send cipher or record state")
	}
	for i, value := range sendBacking {
		if value != 0 {
			t.Fatalf("close retained send byte %d=%02x", i, value)
		}
	}
	if _, writeErr := sender.Write([]byte("closed")); !errors.Is(writeErr, net.ErrClosed) {
		t.Fatalf("write after close=%v want %v", writeErr, net.ErrClosed)
	}

	receiveRaw := &internalRecordConn{}
	_, _ = receiveRaw.Buffer.Write(raw.Bytes())
	receiverWrapper, err := WrapServerConn(receiveRaw, key)
	if err != nil {
		t.Fatal(err)
	}
	receiver := receiverWrapper.(*Conn)
	largeDecoded := make([]byte, len(largePayload))
	if _, err = io.ReadFull(receiver, largeDecoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(largeDecoded, largePayload) {
		t.Fatal("large receive record changed before close")
	}
	firstSmallByte := make([]byte, 1)
	if _, err = receiver.Read(firstSmallByte); err != nil {
		t.Fatal(err)
	}
	if firstSmallByte[0] != smallPayload[0] {
		t.Fatalf("small receive prefix=%02x want=%02x", firstSmallByte[0], smallPayload[0])
	}
	if len(receiver.recvPlain) == 0 {
		t.Fatal("test did not leave plaintext buffered before close")
	}
	if len(receiver.recvPacket) >= cap(receiver.recvPacket) {
		t.Fatalf("test did not shrink receive slab: len=%d cap=%d", len(receiver.recvPacket), cap(receiver.recvPacket))
	}
	receiveBacking := receiver.recvPacket[:cap(receiver.recvPacket)]
	tailRetainedData := false
	for _, value := range receiveBacking[len(receiver.recvPacket):] {
		if value != 0 {
			tailRetainedData = true
			break
		}
	}
	if !tailRetainedData {
		t.Fatal("test setup did not retain a large-record tail outside the shrunken receive slice")
	}
	if err = receiver.Close(); err != nil {
		t.Fatal(err)
	}
	if receiver.aead != nil || receiver.recvPacket != nil || receiver.recvPlain != nil ||
		receiver.recvNoncePrefix != 0 || receiver.recvCtr != 0 || receiver.recvExhausted {
		t.Fatal("close retained receive cipher or record state")
	}
	for i, value := range receiveBacking {
		if value != 0 {
			t.Fatalf("close retained receive byte %d=%02x", i, value)
		}
	}
	if _, readErr := receiver.Read(make([]byte, 1)); !errors.Is(readErr, net.ErrClosed) {
		t.Fatalf("read after close=%v want %v", readErr, net.ErrClosed)
	}
}

func TestConnCloseUnblocksAndJoinsConcurrentReadAndWrite(t *testing.T) {
	key, err := DeriveKey("close-concurrent-lanes")
	if err != nil {
		t.Fatal(err)
	}
	raw := &blockingInternalConn{
		readStarted: make(chan struct{}), writeStarted: make(chan struct{}), closed: make(chan struct{}),
	}
	wrapper, err := WrapClientConn(raw, key)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	writeDone := make(chan error, 1)
	go func() {
		_, readErr := wrapper.Read(make([]byte, 1))
		readDone <- readErr
	}()
	go func() {
		_, writeErr := wrapper.Write([]byte("blocked"))
		writeDone <- writeErr
	}()
	select {
	case <-raw.readStarted:
	case <-time.After(time.Second):
		t.Fatal("read lane did not block in transport")
	}
	select {
	case <-raw.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write lane did not block in transport")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- wrapper.Close() }()
	for name, done := range map[string]<-chan error{"read": readDone, "write": writeDone, "close": closeDone} {
		select {
		case laneErr := <-done:
			if name != "close" && !errors.Is(laneErr, net.ErrClosed) {
				t.Fatalf("%s lane error=%v want %v", name, laneErr, net.ErrClosed)
			}
			if name == "close" && laneErr != nil {
				t.Fatalf("close error=%v", laneErr)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s lane did not join", name)
		}
	}
}
