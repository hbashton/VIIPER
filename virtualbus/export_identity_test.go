package virtualbus

import (
	"fmt"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

func TestProductionAliasIsImmutableBeforePublicationAndHasNoNumericImportFallback(t *testing.T) {
	bus := New(0xf301)
	defer bus.Close()
	device := &closeFenceTestDevice{id: 1}
	alias, err := usbip.NewProductionXboxOneBusID()
	require.NoError(t, err)
	first, err := bus.AddProvisionalRegistrationWithXboxOneBusIDThrough(device, 0xffff, alias)
	require.NoError(t, err)
	actual, err := usbip.ExportBusID(first.Meta)
	require.NoError(t, err)
	require.Equal(t, alias, actual)
	_, found := bus.GetDeviceImportSnapshotByBusID(alias)
	require.False(t, found)
	require.True(t, bus.HasUSBIPBusID(alias), "provisional alias is reserved, not discoverable")
	published, err := bus.PublishRegistration(first)
	require.NoError(t, err)
	require.True(t, published)
	snapshot, found := bus.GetDeviceImportSnapshotByBusID(alias)
	require.True(t, found)
	require.Equal(t, first.RegistrationToken, snapshot.RegistrationToken)
	_, found = bus.GetDeviceImportSnapshotByBusID(fmt.Sprintf("%d-1", bus.BusID()))
	require.False(t, found)
	removed, err := bus.RemoveRegistrationIfPresent(first)
	require.NoError(t, err)
	require.True(t, removed)
	secondAlias, err := usbip.NewProductionXboxOneBusID()
	require.NoError(t, err)
	require.NotEqual(t, alias, secondAlias)
	second, err := bus.AddProvisionalRegistrationWithXboxOneBusIDThrough(device, 0xffff, secondAlias)
	require.NoError(t, err)
	published, err = bus.PublishRegistration(second)
	require.NoError(t, err)
	require.True(t, published)
	require.Equal(t, first.Meta.DevID, second.Meta.DevID)
	_, found = bus.GetDeviceImportSnapshotByBusID(alias)
	require.False(t, found)
	snapshot, found = bus.GetDeviceImportSnapshotByBusID(secondAlias)
	require.True(t, found)
	require.Equal(t, second.RegistrationToken, snapshot.RegistrationToken)
	removed, err = bus.RemoveRegistrationIfPresent(first)
	require.NoError(t, err)
	require.False(t, removed)
	generic, err := bus.AddRegistration(&closeFenceTestDevice{id: 2})
	require.NoError(t, err)
	numeric := fmt.Sprintf("%d-%d", generic.Meta.BusID, generic.Meta.DevID)
	_, found = bus.GetDeviceImportSnapshotByBusID(numeric)
	require.True(t, found)
}
