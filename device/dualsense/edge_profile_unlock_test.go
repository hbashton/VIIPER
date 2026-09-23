package dualsense

import (
	"testing"

	"github.com/Alia5/VIIPER/usb"
	"github.com/stretchr/testify/require"
)

func TestEdgeProfileUnlockStallsWithoutMutatingCommandOrFeedbackState(t *testing.T) {
	d, err := NewEdge(nil)
	require.NoError(t, err)
	request := edgeProfileRequest(d)
	index := request.Setup[4]
	_, handled := d.HandleControl(hidClassOUT, hidSetReport, 0x380, uint16(index), 3,
		[]byte{featureIDCommand, subcmdSerial, 0})
	require.True(t, handled)
	priorCommand := d.subcommand
	priorResponse := d.featureReportCommandResponse()
	beforeOutput, beforeMedia := d.outputState, d.mediaOutputState
	callbacks := 0
	d.SetOutputCallback(func(OutputState) { callbacks++ })

	for _, size := range []int{2, 3, 64} {
		for _, operation := range []byte{0, 1, 0xFF} {
			data := make([]byte, size)
			data[0], data[1] = featureIDCommand, 0x70
			if size >= 3 {
				data[2] = operation
			}
			request.Setup = [8]byte{hidClassOUT, hidSetReport, featureIDCommand, reportTypeFeature, index, 0, byte(size), 0}
			request.Direction = usb.ControlTransactionHostToDevice
			request.TransferLength = uint32(size)
			request.Data = data
			claim, err := d.ClaimControlTransaction(request)
			require.NoError(t, err)
			require.Equal(t, usb.ControlTransactionStall, claim.Result)
			require.NoError(t, d.AdmitControlTransaction(claim, nil))
			require.NoError(t, d.CompleteControlTransaction(claim, usb.ControlTransactionDelivered))
			_, handled := d.HandleControl(hidClassOUT, hidSetReport, 0x380, uint16(index), uint16(size), data)
			require.False(t, handled, "legacy handler must not cache an unimplemented profile command")
			require.Equal(t, priorCommand, d.subcommand)
			require.Equal(t, priorResponse, d.featureReportCommandResponse())
			require.Equal(t, beforeOutput, d.outputState)
			require.Equal(t, beforeMedia, d.mediaOutputState)
			require.Zero(t, callbacks)
		}
	}
}

func TestEdgeProfileUnlockGuardPreservesCommonCommandAndBasePersonaBehavior(t *testing.T) {
	for _, edge := range []bool{false, true} {
		d, err := new(nil, edge)
		require.NoError(t, err)
		request := edgeProfileRequest(d)
		for _, command := range []byte{subcmdSerial, subcmdStatus, subcmdSensors, 0x70} {
			if edge && command == 0x70 {
				continue
			}
			request.Setup = [8]byte{hidClassOUT, hidSetReport, featureIDCommand, reportTypeFeature, request.Setup[4], 0, 3, 0}
			request.Direction, request.TransferLength = usb.ControlTransactionHostToDevice, 3
			request.Data = []byte{featureIDCommand, command, 1}
			claim, err := d.ClaimControlTransaction(request)
			require.NoError(t, err)
			require.Zero(t, claim)
			_, handled := d.HandleControl(hidClassOUT, hidSetReport, 0x380, uint16(request.Setup[4]), 3, request.Data)
			require.True(t, handled)
			require.Equal(t, [2]byte{command, 1}, d.subcommand)
		}
	}
}
