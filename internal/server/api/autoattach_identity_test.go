package api

import (
	"testing"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

func TestAutoAttachArgumentsPreserveExactAliasAndNumericLegacyIdentity(t *testing.T) {
	alias, err := usbip.NewProductionXboxOneBusID()
	require.NoError(t, err)
	for _, busID := range []string{alias, "17-4"} {
		meta := usbip.ExportMeta{BusID: 17, DevID: 4}
		copy(meta.USBBusID[:], busID)
		arguments, err := localhostAttachArguments(&meta, 3241)
		require.NoError(t, err)
		require.Equal(t, []string{"--tcp-port", "3241", "attach", "-r", "localhost", "-b", busID}, arguments)
	}
	_, err = localhostAttachArguments(&usbip.ExportMeta{BusID: 17, DevID: 4}, 3241)
	require.Error(t, err)
	_, err = localhostAttachArguments(nil, 3241)
	require.Error(t, err)
	_, err = localhostAttachArguments(&usbip.ExportMeta{}, 0)
	require.Error(t, err)
}
