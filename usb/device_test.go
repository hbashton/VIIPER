package usb

import "testing"

func TestTransferDirectionWireContract(t *testing.T) {
	if DirectionOut != 0 || DirectionIn != 1 {
		t.Fatalf("USB transfer directions changed: OUT=%d IN=%d", DirectionOut, DirectionIn)
	}
}
