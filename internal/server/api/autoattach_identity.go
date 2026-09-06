package api

import (
	"fmt"
	"strconv"

	"github.com/Alia5/VIIPER/usbip"
)

func validatedAutoAttachBusID(meta *usbip.ExportMeta, port uint16) (string, error) {
	if meta == nil || port == 0 {
		return "", fmt.Errorf("argumentValidation: invalid USB/IP attach metadata or TCP port")
	}
	busID, err := usbip.ExportBusID(*meta)
	if err != nil {
		return "", fmt.Errorf("argumentValidation: invalid USB/IP export identity: %w", err)
	}
	return busID, nil
}

// Both command implementations consume the exact serialized export identity;
// neither is allowed to silently reconstruct a numeric fallback.
func localhostAttachArguments(meta *usbip.ExportMeta, port uint16) ([]string, error) {
	busID, err := validatedAutoAttachBusID(meta, port)
	if err != nil {
		return nil, err
	}
	return []string{"--tcp-port", strconv.FormatUint(uint64(port), 10), "attach", "-r", "localhost", "-b", busID}, nil
}
