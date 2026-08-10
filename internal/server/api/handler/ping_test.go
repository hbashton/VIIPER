package handler_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	handlerTest "github.com/Alia5/VIIPER/internal/_testing"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/api/handler"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/viipertypes"
)

func TestPing(t *testing.T) {
	addr, _, done := handlerTest.StartAPIServer(t, func(r *api.Router, s *usb.Server, apiSrv *api.Server) {
		r.Register("ping", handler.Ping())
	})
	defer done()

	c := viiperclient.NewTransport(addr)
	line, err := c.Do("ping", nil, nil)
	assert.NoError(t, err)

	var out viipertypes.PingResponse
	err = json.Unmarshal([]byte(line), &out)
	assert.NoError(t, err)
	assert.Equal(t, "VIIPER", out.Server)
	assert.NotEmpty(t, out.Version)
	assert.Empty(t, out.Transport)
	assert.Nil(t, out.Ready)
	assert.Nil(t, out.NativeUDE)
}

func TestPingReportsNegotiatedNativeBackend(t *testing.T) {
	want := &viipertypes.NativeUDEInfo{
		ABIMajor: 1, ABIMinor: 8, Capabilities: 0x0d,
		ExpectedDriverPackageVersion: "0.1.0.0",
		MaxDevices:                   32, MaxDescriptorBytes: 262144,
		MaxTransferBytes: 1048576, MaxIsoPackets: 1024,
		MaxPendingOperations: 4096,
	}
	addr, _, done := handlerTest.StartAPIServer(t, func(r *api.Router, _ *usb.Server, _ *api.Server) {
		r.Register("ping", handler.Ping(handler.PingOptions{
			Transport: "native-ude",
			Status: func() (bool, *viipertypes.NativeUDEInfo) {
				copy := *want
				return true, &copy
			},
		}))
	})
	defer done()

	c := viiperclient.NewTransport(addr)
	line, err := c.Do("ping", nil, nil)
	assert.NoError(t, err)
	var out viipertypes.PingResponse
	assert.NoError(t, json.Unmarshal([]byte(line), &out))
	assert.Equal(t, "native-ude", out.Transport)
	if assert.NotNil(t, out.Ready) {
		assert.True(t, *out.Ready)
	}
	assert.Equal(t, want, out.NativeUDE)
}
