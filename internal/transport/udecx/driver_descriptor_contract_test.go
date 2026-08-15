package udecx

import (
	"strings"
	"testing"
)

func TestNativeDriverValidatesUdeCxEndpointSchedulesBeforePublication(t *testing.T) {
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	validator := normalizedContract(nativeCFunction(t, device, "ViiperValidateEndpointSchedules"))
	for _, required := range []string{
		"transferType = item[3] & USB_ENDPOINT_TYPE_MASK;",
		"if (transferType == USB_ENDPOINT_TYPE_ISOCHRONOUS)",
		"if (Speed == 1) { return FALSE; }",
		"if (Speed == 2)",
		"if (item[6] != 4) { return FALSE; }",
		"else if (item[6] == 0 || item[6] > 4)",
		"USB_ENDPOINT_TYPE_INTERRUPT && (item[6] == 0 || item[6] > 16)",
	} {
		if !strings.Contains(validator, required) {
			t.Fatalf("native descriptor schedule gate lost %q in:\n%s", required, validator)
		}
	}

	create := normalizedContract(nativeCFunction(t, device, "ViiperValidateCreateDevice"))
	requireContractOrder(t, create,
		"!ViiperValidateDescriptorChain( descriptor, record->Length, USB_CONFIGURATION_DESCRIPTOR_TYPE) ||",
		"!ViiperValidateEndpointSchedules( descriptor, record->Length, Input->Speed)")
}
