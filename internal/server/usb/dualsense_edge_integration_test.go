package usb_test

import (
	"testing"

	"github.com/Alia5/VIIPER/device/dualsense"
	usbserver "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/stretchr/testify/require"
)

func TestDualSenseEdgeUnsupportedProfilesStallOnWireWithoutBreakingEnumeration(t *testing.T) {
	device, err := dualsense.NewEdge(nil)
	require.NoError(t, err)
	usbserver.AssertEdgeProfileStallTransport(t, device)
}
