package auth

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"testing"
)

// Independent Node/OpenSSL ChaCha20-Poly1305 vectors, not this wrapper's output.
var v2Records = []string{
	"0000002400000000000000000000000018b94032d266582e05ebcfe4ba88b8a24dd1043e6dcd23fb",
	"00000024000000000000000000000001695d7eda4e8a46850d10d0c85e47680d9f125be025e25461",
	"00000024000000010000000000000000ab479fea760618c3be9f8fd13269fd4b4fc440460493639f",
	"00000024000000010000000000000001e10be70c805489fbcb0d12b623a633fd8e2ad6a2e57a58c0",
}

func v2Hex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func v2Key() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func TestV2IndependentBidirectionalVectors(t *testing.T) {
	payload := v2Hex(t, "000102037f80feff")
	for _, role := range []Role{Client, Server} {
		wire := &memoryAuthConn{}
		wrapped, err := WrapConn(wire, v2Key(), role)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			wire.written.Reset()
			if _, err := wrapped.Write(payload); err != nil {
				t.Fatal(err)
			}
			expected := v2Hex(t, v2Records[int(role-1)*2+i])
			if !bytes.Equal(wire.written.Bytes(), expected) {
				t.Fatalf("role %d sequence %d mismatches independent vector", role, i)
			}
		}
		received := append(v2Hex(t, v2Records[int(2-role)*2]), v2Hex(t, v2Records[int(2-role)*2+1])...)
		wire.read = bytes.NewReader(received)
		// One-byte consumption must preserve both record tails.
		actual := make([]byte, 2*len(payload))
		for i := range actual {
			if n, err := wrapped.Read(actual[i : i+1]); n != 1 || err != nil {
				t.Fatalf("read %d: n=%d err=%v", i, n, err)
			}
		}
		if !bytes.Equal(actual, append(payload, payload...)) {
			t.Fatal("stream lost plaintext")
		}
	}
}

func TestV2InvalidRecordPoisonsConnectionWithoutDeliveringBytes(t *testing.T) {
	first := v2Hex(t, v2Records[0])
	badTag := bytes.Clone(first)
	badTag[len(badTag)-1] ^= 1
	cases := map[string][]byte{
		"reflection":       v2Hex(t, v2Records[2]),
		"counter_gap":      v2Hex(t, v2Records[1]),
		"tampered_tag":     badTag,
		"truncated_header": first[:2],
		"truncated_body":   first[:len(first)-1],
		"missing_body":     first[:4],
		"short_record":     {0, 0, 0, 27},
		"oversized_record": {0, 32, 0, 1},
	}
	for name, packet := range cases {
		t.Run(name, func(t *testing.T) {
			wire := &memoryAuthConn{read: bytes.NewReader(packet)}
			wrapped, err := WrapConn(wire, v2Key(), Server)
			if err != nil {
				t.Fatal(err)
			}
			output := bytes.Repeat([]byte{0x55}, 8)
			if n, err := wrapped.Read(output); n != 0 || err == nil {
				t.Fatalf("malformed read: n=%d err=%v", n, err)
			}
			if !bytes.Equal(output, bytes.Repeat([]byte{0x55}, 8)) {
				t.Fatal("delivered unauthenticated bytes")
			}
			if wrapped.(*Conn).recvCtr != 0 {
				t.Fatal("advanced rejected receive counter")
			}
			wire.read = bytes.NewReader(first)
			if _, err := wrapped.Read(output); !errors.Is(err, errInvalidEncryptedRecord) {
				t.Fatalf("faulted read resumed: %v", err)
			}
			if _, err := wrapped.Write([]byte{1}); !errors.Is(err, errInvalidEncryptedRecord) {
				t.Fatalf("faulted write resumed: %v", err)
			}
		})
	}
}

func TestV2CounterExhaustionAndExplicitRole(t *testing.T) {
	for _, role := range []Role{0, 3, 255} {
		if _, err := WrapConn(&memoryAuthConn{}, v2Key(), role); err == nil {
			t.Fatalf("accepted role %d", role)
		}
	}
	wire := &memoryAuthConn{}
	wrapped, _ := WrapConn(wire, v2Key(), Client)
	wrapped.(*Conn).sendCtr = math.MaxUint64
	if n, err := wrapped.Write([]byte{1}); n != 0 || err == nil || wire.written.Len() != 0 {
		t.Fatal("exhausted counter wrote a record")
	}
	receiver, _ := WrapConn(&memoryAuthConn{read: bytes.NewReader(v2Hex(t, v2Records[0]))}, v2Key(), Server)
	receiver.(*Conn).recvCtr = math.MaxUint64
	if _, err := receiver.Read(make([]byte, 8)); err == nil {
		t.Fatal("exhausted receive counter accepted a record")
	}
}

func TestV2EmptyOperationsAndEmptyRecordAreNotEOF(t *testing.T) {
	wire := &memoryAuthConn{}
	wrapped, _ := WrapConn(wire, v2Key(), Server)
	if n, err := wrapped.Read(nil); n != 0 || err != nil {
		t.Fatal("empty read touched transport")
	}
	if n, err := wrapped.Write(nil); n != 0 || err != nil || wire.written.Len() != 0 {
		t.Fatal("empty write emitted record")
	}
	empty := testEncryptedRecord(t, v2Key(), 0, nil)
	data := testEncryptedRecord(t, v2Key(), 1, []byte{0x55})
	wire.read = bytes.NewReader(append(empty, data...))
	var output [1]byte
	if n, err := wrapped.Read(output[:]); n != 1 || err != nil || output[0] != 0x55 {
		t.Fatalf("empty record became EOF: %d %v", n, err)
	}
	if n, err := wrapped.Read(output[:]); n != 0 || err != io.EOF {
		t.Fatalf("expected clean EOF: %d %v", n, err)
	}
}

func TestV2NegotiationNeverReinterpretsLegacyOrUnknownMagic(t *testing.T) {
	for _, magic := range []string{"eVI1\x00", "eVI3\x00", "eVI2!"} {
		if accepted, err := IsAuthHandshake(bufio.NewReader(bytes.NewBufferString(magic))); accepted || !errors.Is(err, ErrUnsupportedAuthVersion) {
			t.Fatalf("magic %q: %v %v", magic, accepted, err)
		}
		var response bytes.Buffer
		_, _, err := HandleAuthHandshake(bufio.NewReader(bytes.NewBufferString(magic)), &response, v2Key(), false)
		if !errors.Is(err, ErrUnsupportedAuthVersion) || response.Len() != 0 {
			t.Fatalf("direct helper accepted %q: %v", magic, err)
		}
	}
}

func TestV2IndependentHandshakeAndSessionVectors(t *testing.T) {
	client := bytes.Repeat([]byte{0x5a}, NonceSize)
	server := bytes.Repeat([]byte{0xa5}, NonceSize)
	expected := v2Hex(t, "71424901662650fb5c29ce71795ba055f114d4701da55490fadd27f82398f00c")
	if !bytes.Equal(DeriveSessionKey(v2Key(), server, client), expected) {
		t.Fatal("session context or nonce order differs from independent vector")
	}
	request := append([]byte("eVI2\x00"), client...)
	request = append(request, v2Hex(t, "2ac9c36cb65d0fac8fdab74f2dd52169f721cfc3ac605006be40c43fffbfdbdf")...)
	var response bytes.Buffer
	actualClient, _, err := HandleAuthHandshake(bufio.NewReader(bytes.NewReader(request)), &response, v2Key(), false)
	if err != nil || !bytes.Equal(actualClient, client) || response.Len() != 35 || !bytes.Equal(response.Bytes()[:3], []byte("OK\x00")) {
		t.Fatalf("independently authenticated handshake failed: %v", err)
	}
}
