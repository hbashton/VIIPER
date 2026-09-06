package xboxone

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionRemovalCapabilityBindsExactlyOneAdd(t *testing.T) {
	device, _ := testProductionRetainedUSBDevice(t)
	first, cancel := context.WithCancel(context.Background())
	defer cancel()
	second, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	token, err := device.BindProductionRemovalRegistration(first.Done())
	require.NoError(t, err)
	require.True(t, ValidProductionRemovalToken(token))
	require.True(t, device.AuthorizesProductionRemoval(first.Done(), token))
	require.False(t, device.AuthorizesProductionRemoval(second.Done(), token))
	require.False(t, device.AuthorizesProductionRemoval(nil, token))
	_, err = device.BindProductionRemovalRegistration(first.Done())
	require.Error(t, err)
	cancel()
	// The old snapshot may still join its exact removal, but cannot authorize
	// a successor Add, even for the same device pointer.
	require.True(t, device.AuthorizesProductionRemoval(first.Done(), token))
	_, err = device.BindProductionRemovalRegistration(second.Done())
	require.Error(t, err)
	require.False(t, device.AuthorizesProductionRemoval(second.Done(), token))
	metadata, err := json.Marshal(device.GetDeviceSpecificArgs())
	require.NoError(t, err)
	require.NotContains(t, string(metadata), token)
}

func TestProductionRemovalCapabilityHasNoPredictableOrCrossDeviceFallback(t *testing.T) {
	tokens := make(map[string]bool)
	for range 32 {
		device, _ := testProductionRetainedUSBDevice(t)
		registration, cancel := context.WithCancel(context.Background())
		token, err := device.BindProductionRemovalRegistration(registration.Done())
		require.NoError(t, err)
		require.False(t, tokens[token])
		tokens[token] = true
		for other := range tokens {
			require.Equal(t, token == other, device.AuthorizesProductionRemoval(registration.Done(), other))
		}
		require.False(t, device.AuthorizesProductionRemoval(registration.Done(), strings.Repeat("0", 64)))
		cancel()
	}
	var nilDevice *AuthorizedDormantRetainedUSBDevice
	require.False(t, nilDevice.AuthorizesProductionRemoval(make(chan struct{}), strings.Repeat("a", 64)))
	_, err := nilDevice.BindProductionRemovalRegistration(make(chan struct{}))
	require.Error(t, err)
	unissued := &AuthorizedDormantRetainedUSBDevice{}
	_, err = unissued.BindProductionRemovalRegistration(make(chan struct{}))
	require.Error(t, err)
	device, _ := testProductionRetainedUSBDevice(t)
	closed := make(chan struct{})
	close(closed)
	_, err = device.BindProductionRemovalRegistration(closed)
	require.Error(t, err)
}
