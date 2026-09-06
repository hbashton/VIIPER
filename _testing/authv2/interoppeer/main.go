// Local integration peer only: no controller, API server, USB/IP or live keys.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/Alia5/VIIPER/internal/server/api/auth"
)

const password = "synthetic-auth-v2-interop-not-a-deployment-key"
const records = 4096

func payload(index int, direction byte) []byte {
	n := 24 + index%521
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(index+i) ^ direction
	}
	return b
}

func exchange(conn net.Conn, key []byte) error {
	r := bufio.NewReader(conn)
	clientNonce, serverNonce, err := auth.HandleAuthHandshake(r, conn, key, false)
	if err != nil {
		return err
	}
	stream, err := auth.WrapConn(conn, auth.DeriveSessionKey(key, serverNonce, clientNonce), auth.Server)
	if err != nil {
		return err
	}
	defer stream.Close()
	// Handshake consumes exactly one message. The client waits for its response
	// before sending; assert that no encrypted input was silently discarded.
	if r.Buffered() != 0 {
		return fmt.Errorf("unexpected prefetched client bytes: %d", r.Buffered())
	}
	written := make(chan error, 1)
	go func() {
		for i := 0; i < records; i++ {
			if _, err := stream.Write(payload(i, 0xa5)); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	for i := 0; i < records; i++ {
		want := payload(i, 0x5a)
		actual := make([]byte, len(want))
		if _, err := io.ReadFull(stream, actual); err != nil {
			return err
		}
		if !bytes.Equal(want, actual) {
			return fmt.Errorf("client plaintext mismatch at %d", i)
		}
	}
	return <-written
}

func run() error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	key, err := auth.DeriveKey(password)
	if err != nil {
		return err
	}
	defer clear(key)
	fmt.Println(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	if err := exchange(conn, key); err != nil {
		return err
	}
	fmt.Println("PASS: Go production auth verified 4096 records in each direction")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
