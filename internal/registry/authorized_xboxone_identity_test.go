package registry

import (
	"log/slog"
	"testing"
	"time"

	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

func TestProductionFactoryIssuesAliasWhileDormantAuthorizedFactoryStaysNumeric(t *testing.T) {
	server, _ := registryTestUSBServer(t, 62010, registryTestAuthorityID)
	production, err := RegisterProductionXboxOneRetainedUSB(server, registryTestProductionXboxRequest(62010, registryTestAuthorityID))
	require.NoError(t, err)
	productionMeta, ok := production.DeviceMeta()
	require.True(t, ok)
	alias, err := usbip.ExportBusID(productionMeta.Meta)
	require.NoError(t, err)
	require.True(t, usbip.ValidProductionXboxOneBusID(alias))
	require.NoError(t, production.Close())
	dormant, err := RegisterAuthorizedXboxOneRetainedUSB(server, registryTestXboxRequest(t, 62010, registryTestAuthorityID))
	require.NoError(t, err)
	dormantMeta, ok := dormant.DeviceMeta()
	require.True(t, ok)
	numeric, err := usbip.ExportBusID(dormantMeta.Meta)
	require.NoError(t, err)
	require.Equal(t, "62010-1", numeric)
	require.NoError(t, dormant.Close())
}

func TestProductionFactoryRejectsUnsupportedBudgetBeforeIdentityPreparation(t *testing.T) {
	server := serverusb.New(serverusb.ServerConfig{ConnectionTimeout: 101 * time.Second}, slog.Default(), nil)
	defer server.Close()
	// Neither a bus nor valid persona options exist. Budget validation must
	// precede consuming any identity or publishing a partial registration.
	registration, err := RegisterProductionXboxOneRetainedUSB(server, ProductionXboxOneRetainedUSBRequest{})
	require.ErrorContains(t, err, "lifecycle budget")
	require.Nil(t, registration)
}
