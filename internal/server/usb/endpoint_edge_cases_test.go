package usb

import (
	"context"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

func TestInterruptEnqueueRejectsMissingEndpointBeforeWorkerAccess(t *testing.T) {
	// No worker state is installed: an absent descriptor must fail before
	// attempting to use any active transport or response writer.
	schedulers := &endpointSchedulers{desc: testCompositeDescriptor()}
	require.False(t, schedulers.enqueueInterruptIn(1, 3, 64))
	require.Nil(t, schedulers.workers)
}

func TestIsoSubmissionRejectsMissingEndpoint(t *testing.T) {
	err := validateIsoSubmission(testCompositeDescriptor(), 2, usbip.DirIn, 8,
		[]usbip.IsoPacketDescriptor{{Offset: 0, Length: 8}})
	require.ErrorContains(t, err, "endpoint 0x82")
}

type directEndpointTestDevice struct {
	*altSettingTestDevice
	endpoint  uint32
	direction uint32
	payload   []byte
}

func (device *directEndpointTestDevice) HandleTransfer(
	_ context.Context, endpoint, direction uint32, payload []byte,
) []byte {
	device.transferCalls++
	device.endpoint = endpoint
	device.direction = direction
	device.payload = payload
	return []byte{0x5a}
}

func TestProcessSubmitRoutesNonControlEndpointWithoutParsingSetup(t *testing.T) {
	device := &directEndpointTestDevice{altSettingTestDevice: &altSettingTestDevice{}}
	server := &Server{}
	payload := []byte{0x12, 0x34}
	response := server.processSubmit(context.Background(), device, 3, usbip.DirIn, nil, payload)
	require.Equal(t, []byte{0x5a}, response)
	require.Equal(t, 1, device.transferCalls)
	require.Equal(t, uint32(3), device.endpoint)
	require.Equal(t, uint32(usbip.DirIn), device.direction)
	require.Equal(t, payload, device.payload)
}
