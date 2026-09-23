package usb_test

import (
	"sync/atomic"
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

func TestDualSenseEdgeConfigurationOutputRejectedOnWireWithoutPartialEffects(t *testing.T) {
	device, err := dualsense.NewEdge(nil)
	require.NoError(t, err)
	var callbacks atomic.Int32
	device.SetOutputCallback(func(state dualsense.OutputState) {
		callbacks.Add(1)
		if state.RumbleSmall != 17 || state.RumbleLarge != 29 {
			t.Errorf("rejected configuration leaked partial feedback: %d/%d", state.RumbleSmall, state.RumbleLarge)
		}
	})
	usbserver.AssertEdgeConfigurationStallTransport(t, device)
	require.Equal(t, int32(2), callbacks.Load(), "only the two ordinary output reports may reach feedback")
}
