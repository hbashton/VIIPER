package udecx

import (
	"strings"
	"testing"
)

func TestNativeDriverUsesEndpointDirectionForNonControlTransfers(t *testing.T) {
	source := nativeDriverBrokerSource(t)
	start := strings.Index(source, "ViiperSerializeOperation(")
	if start < 0 {
		t.Fatal("native transfer serializer is missing")
	}
	end := strings.Index(source[start:], "ViiperDispatchAvailable(")
	if end < 0 {
		t.Fatal("native transfer serializer boundary is missing")
	}
	serializer := source[start : start+end]

	for _, required := range []string{
		"urb->UrbHeader.Function != URB_FUNCTION_CONTROL_TRANSFER",
		"urb->UrbHeader.Function != URB_FUNCTION_CONTROL_TRANSFER_EX",
		"endpointContext->Descriptor.bEndpointAddress &",
		"USB_ENDPOINT_DIRECTION_MASK",
		"transferFlags |= USBD_TRANSFER_DIRECTION_IN;",
		"transferFlags &= ~USBD_TRANSFER_DIRECTION_IN;",
		"operation->Direction = directionIn ? 1 : 0;",
		"operation->TransferFlags = transferFlags;",
	} {
		if !strings.Contains(serializer, required) {
			t.Fatalf("native transfer direction normalization is missing %q", required)
		}
	}

	metadataStart := strings.Index(source, "ViiperGetTransferMetadata(")
	if metadataStart < 0 {
		t.Fatal("native transfer metadata helper is missing")
	}
	metadataEnd := strings.Index(source[metadataStart:], "ViiperSerializeOperation(")
	if metadataEnd < 0 {
		t.Fatal("native transfer metadata helper boundary is missing")
	}
	metadata := source[metadataStart : metadataStart+metadataEnd]
	if !strings.Contains(metadata, "*DirectionIn = ((SetupPacket[0] & USB_ENDPOINT_DIRECTION_MASK) != 0);") {
		t.Fatal("control transfer direction no longer comes from its setup packet")
	}
}
