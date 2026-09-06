package handler

import (
	"testing"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

func TestXboxOneCreationReceiptCarriesExactPublicAliasAndActualCloseBudget(t *testing.T) {
	fixture := newXboxRemovalFactoryFixture(t, 63005)
	created := fixture.create(t, 401)
	require.True(t, usbip.ValidProductionXboxOneBusID(created.USBIPBusID))
	require.Equal(t, uint32(15000), created.RemovalTimeoutMilliseconds)
	metas := fixture.server.GetBus(fixture.busID).GetAllDeviceMetas()
	require.Len(t, metas, 1)
	actual, err := usbip.ExportBusID(metas[0].Meta)
	require.NoError(t, err)
	require.Equal(t, actual, created.USBIPBusID)
	require.NotEqual(t, created.RemovalToken, created.USBIPBusID)
}
