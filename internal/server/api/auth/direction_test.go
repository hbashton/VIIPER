package auth

import (
	"bytes"
	"io"
	"testing"
)

func TestBidirectionalRecordsUseDistinctNonceDomains(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	clientWire, serverWire := &memoryAuthConn{}, &memoryAuthConn{}
	client, err := WrapConn(clientWire, key, Client)
	if err != nil {
		t.Fatal(err)
	}
	server, err := WrapConn(serverWire, key, Server)
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range []io.Writer{client, server} {
		if _, err := peer.Write([]byte("identical synthetic payload")); err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Equal(clientWire.written.Bytes()[4:16], serverWire.written.Bytes()[4:16]) {
		t.Fatal("client and server reused the same nonce under one session key")
	}
}

func TestReflectedRecordIsRejected(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	wire := &memoryAuthConn{}
	server, err := WrapConn(wire, key, Server)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Write([]byte{0x5a}); err != nil {
		t.Fatal(err)
	}
	wire.read = bytes.NewReader(wire.written.Bytes())
	var value [1]byte
	if _, err := server.Read(value[:]); err == nil {
		t.Fatal("server accepted its own reflected output as client input")
	}
}

type memoryAuthConn struct {
	discardAuthConn
	read    *bytes.Reader
	written bytes.Buffer
}

func (c *memoryAuthConn) Read(p []byte) (int, error) {
	if c.read == nil {
		return 0, io.EOF
	}
	return c.read.Read(p)
}
func (c *memoryAuthConn) Write(p []byte) (int, error) { return c.written.Write(p) }
