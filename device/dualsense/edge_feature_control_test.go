package dualsense

import (
	"sync"
	"testing"

	"github.com/Alia5/VIIPER/usb"
	"github.com/stretchr/testify/require"
)

func edgeProfileRequest(d *DualSense) usb.ControlTransactionRequest {
	var index byte
	for _, iface := range d.descriptor.Interfaces {
		if iface.HID != nil {
			index = iface.Descriptor.BInterfaceNumber
			break
		}
	}
	return usb.ControlTransactionRequest{Setup: [8]byte{0xA1, 1, 0x70, 3, index, 0, 64, 0},
		Direction: usb.ControlTransactionDeviceToHost, TransferLength: 64}
}

func TestEdgeFeatureClaimsAreExactAndRetiredOnce(t *testing.T) {
	d, err := NewEdge(nil)
	require.NoError(t, err)
	request := edgeProfileRequest(d)
	claim, err := d.ClaimControlTransaction(request)
	require.NoError(t, err)
	require.Equal(t, usb.ControlTransactionStall, claim.Result)
	forged := claim
	forged.Generation++
	require.Error(t, d.AdmitControlTransaction(forged, nil))
	require.Error(t, d.CompleteControlTransaction(forged, usb.ControlTransactionCancelled))
	require.Error(t, d.AdmitControlTransaction(claim, []byte{0}))
	require.Error(t, d.CompleteControlTransaction(claim, usb.ControlTransactionDelivered))
	require.NoError(t, d.AdmitControlTransaction(claim, nil))
	require.Error(t, d.AdmitControlTransaction(claim, nil))
	require.Error(t, d.CompleteControlTransaction(claim, usb.ControlTransactionCancelled))
	require.NoError(t, d.CompleteControlTransaction(claim, usb.ControlTransactionDelivered))
	require.Error(t, d.CompleteControlTransaction(claim, usb.ControlTransactionDelivered))
	require.Error(t, d.AdmitControlTransaction(claim, nil))
	for _, outcome := range []usb.ControlTransactionOutcome{usb.ControlTransactionCancelled, usb.ControlTransactionDeliveryFailed} {
		next, err := d.ClaimControlTransaction(request)
		require.NoError(t, err)
		require.NotEqual(t, claim.Token, next.Token)
		if outcome != usb.ControlTransactionCancelled {
			require.NoError(t, d.AdmitControlTransaction(next, nil))
		}
		require.NoError(t, d.CompleteControlTransaction(next, outcome))
	}
}

func TestEdgeFeatureClaimsAreBoundedAndConcurrent(t *testing.T) {
	d, err := NewEdge(nil)
	require.NoError(t, err)
	request := edgeProfileRequest(d)
	var claims []usb.ControlTransactionClaim
	for i := 0; i < len(d.edgeFeatureControl.slots); i++ {
		claim, err := d.ClaimControlTransaction(request)
		require.NoError(t, err)
		claims = append(claims, claim)
	}
	claim, err := d.ClaimControlTransaction(request)
	require.Error(t, err)
	require.Zero(t, claim)
	for _, claim := range claims {
		require.NoError(t, d.CompleteControlTransaction(claim, usb.ControlTransactionCancelled))
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				claim, err := d.ClaimControlTransaction(request)
				if err != nil {
					t.Error(err)
					return
				}
				if err = d.AdmitControlTransaction(claim, nil); err != nil {
					t.Error(err)
					return
				}
				if err = d.CompleteControlTransaction(claim, usb.ControlTransactionDelivered); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, slot := range d.edgeFeatureControl.slots {
		require.Zero(t, slot)
	}
}

func TestEdgeFeatureClaimsLeaveOtherTrafficUnchanged(t *testing.T) {
	d, err := NewEdge(nil)
	require.NoError(t, err)
	request := edgeProfileRequest(d)
	base, err := New(nil)
	require.NoError(t, err)
	claim, err := base.ClaimControlTransaction(request)
	require.NoError(t, err)
	require.Zero(t, claim)
	for _, setup := range [][8]byte{
		{0x80, 6, 0, 1, 0, 0, 18, 0},                   // descriptor
		{0xA1, 1, 0x20, 3, request.Setup[4], 0, 64, 0}, // firmware
		{0x21, 9, 2, 2, request.Setup[4], 0, 64, 0},    // native output
		{0xA1, 1, 0x70, 3, 0xFF, 0, 64, 0},             // wrong interface
		{0xA1, 1, 0x70, 3, request.Setup[4], 1, 64, 0}, // truncated interface
	} {
		request.Setup = setup
		claim, err := d.ClaimControlTransaction(request)
		require.NoError(t, err)
		require.Zero(t, claim)
	}
}
