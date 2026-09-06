package xboxone

import (
	"errors"
	"net"
	"testing"
)

func TestProductionStreamHandlerRejectsUnauthenticatedConnection(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	err := ProductionStreamHandler(server, nil, nil)
	if !errors.Is(err, ErrProductionBrokerAuthenticationRequired) {
		t.Fatalf("ProductionStreamHandler error = %v, want %v",
			err, ErrProductionBrokerAuthenticationRequired)
	}
}
