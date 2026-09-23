package dualsense

import (
	"testing"

	"github.com/Alia5/VIIPER/usb"
	"github.com/stretchr/testify/require"
)

// daidr/dualsense-tester f6e6247, DualSenseEdge/_Profile/pages:
// JoystickSensitivity.vue and TriggerDeadZone.vue send payload byte 38 = 0x80,
// byte 40 = 0/1/2/3 and preview parameters starting at byte 49. USB adds ID 0x02.
// These are controller configuration commands, not ordinary adaptive effects.
func edgeConfigurationOutput(selector byte, size int, withID bool) []byte {
	report := make([]byte, size)
	report[0], report[1], report[3], report[4] = ReportIDOutput, 0x0F, 71, 93
	report[11], report[22] = 0x21, 0x26
	report[39] = 0x80
	if size > 41 {
		report[41] = selector
	}
	for i := 50; i < len(report); i++ {
		report[i] = byte(i - 41)
	}
	if !withID {
		return report[1:]
	}
	return report
}

func TestEdgeConfigurationOutputIsRejectedAtomically(t *testing.T) {
	d, err := NewEdge(nil)
	require.NoError(t, err)
	callbacks := 0
	d.SetOutputCallback(func(OutputState) { callbacks++ })
	baseline := []byte{ReportIDOutput, 0x03, 0, 17, 29}
	require.True(t, d.handleOutputReport(baseline))
	beforeOutput, beforeMedia := d.outputState, d.mediaOutputState
	for _, withID := range []bool{true, false} {
		for _, size := range []int{40, 48, 64} {
			for _, selector := range []byte{0, 1, 2, 3, 0xFF} {
				report := edgeConfigurationOutput(selector, size, withID)
				require.False(t, d.handleOutputReport(report), "size=%d selector=%d withID=%t", size, selector, withID)
				require.Equal(t, beforeOutput, d.outputState)
				require.Equal(t, beforeMedia, d.mediaOutputState)
				require.Equal(t, 1, callbacks)
			}
		}
	}
	// A rejected preview neither disables later native feedback nor mutates V5.
	require.True(t, d.handleOutputReport(baseline))
	require.Equal(t, 2, callbacks)
	require.Equal(t, 48, len(d.outputState.RawOutputReport))
	require.Equal(t, 76, OutputStateCombinedBluetoothOffset)
}

func TestEdgeConfigurationOutputRejectionIsPersonaAndBitSpecific(t *testing.T) {
	for _, edge := range []bool{false, true} {
		d, err := new(nil, edge)
		require.NoError(t, err)
		callbacks := 0
		var feedback OutputState
		d.SetOutputCallback(func(state OutputState) { callbacks++; feedback = state })
		for flags := 0; flags < 256; flags++ {
			report := edgeConfigurationOutput(3, 64, true)
			report[39] = byte(flags)
			want := !edge || flags&0x80 == 0
			require.Equal(t, want, d.handleOutputReport(report), "edge=%t flags=%02X", edge, flags)
			if want {
				require.Equal(t, byte(flags), feedback.RawOutputReport[39])
			}
		}
		if edge {
			require.Equal(t, 128, callbacks)
		} else {
			require.Equal(t, 256, callbacks)
		}
	}
}

func TestEdgeExtensionAuthorizationIsRejectedWithoutTailOrPreviewBit(t *testing.T) {
	for _, edge := range []bool{false, true} {
		d, err := new(nil, edge)
		require.NoError(t, err)
		for _, size := range []int{42, 48, 64} {
			for flags := 0; flags < 256; flags++ {
				report := edgeConfigurationOutput(0, size, true)
				report[39], report[41] = 0, byte(flags)
				require.Equal(t, !edge || flags&0x80 == 0, d.handleOutputReport(report),
					"edge=%t size=%d flags=%02X", edge, size, flags)
			}
		}
	}
}

func TestEdgeConfigurationControlClaimsStallAndInterruptAdmissionRejects(t *testing.T) {
	d, err := NewEdge(nil)
	require.NoError(t, err)
	for _, withID := range []bool{true, false} {
		report := edgeConfigurationOutput(1, 64, withID)
		setup := [8]byte{hidClassOUT, hidSetReport, ReportIDOutput, reportTypeOutput, 3, 0, byte(len(report)), 0}
		handled, accepted := d.TryHandleOutputCommand(0, setup, report)
		require.False(t, handled, "EP0 must reach the transactional STALL owner")
		require.False(t, accepted)
		request := usb.ControlTransactionRequest{Setup: setup, Direction: usb.ControlTransactionHostToDevice,
			TransferLength: uint32(len(report)), Data: report}
		claim, err := d.ClaimControlTransaction(request)
		require.NoError(t, err)
		require.Equal(t, usb.ControlTransactionStall, claim.Result)
		require.NoError(t, d.AdmitControlTransaction(claim, nil))
		require.NoError(t, d.CompleteControlTransaction(claim, usb.ControlTransactionDelivered))
		_, legacyHandled := d.HandleControl(hidClassOUT, hidSetReport, 0x202, 3, uint16(len(report)), report)
		require.False(t, legacyHandled, "legacy handler must not falsely acknowledge discarded configuration")
		handled, accepted = d.TryHandleOutputCommand(EndpointOut&0x0F, [8]byte{}, report)
		require.True(t, handled)
		require.False(t, accepted)
		request.Setup[4] = 0xFF
		claim, err = d.ClaimControlTransaction(request)
		require.NoError(t, err)
		require.Zero(t, claim, "configuration guard must not own an unrelated interface")
	}
}
