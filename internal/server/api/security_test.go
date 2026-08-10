package api

import (
	"log/slog"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerConfigSecureDefaults(t *testing.T) {
	var options struct {
		API ServerConfig `embed:"" prefix:"api."`
	}
	parser, err := kong.New(&options)
	require.NoError(t, err)
	_, err = parser.Parse(nil)
	require.NoError(t, err)

	assert.Equal(t, DefaultListenAddress, options.API.Addr)
	assert.True(t, options.API.RequireLocalHostAuth)
}

func TestServerConfigExplicitLocalDevelopmentOptOut(t *testing.T) {
	var options struct {
		API ServerConfig `embed:"" prefix:"api."`
	}
	parser, err := kong.New(&options)
	require.NoError(t, err)
	_, err = parser.Parse([]string{
		"--api.addr=:43242",
		"--api.require-local-host-auth=false",
	})
	require.NoError(t, err)

	assert.Equal(t, ":43242", options.API.Addr)
	assert.False(t, options.API.RequireLocalHostAuth)
}

func TestNewUsesLoopbackWhenAddressIsEmpty(t *testing.T) {
	server := New(nil, "  ", ServerConfig{}, slog.Default())

	assert.Equal(t, DefaultListenAddress, server.Addr())
	assert.Equal(t, DefaultListenAddress, server.Config().Addr)
}

func TestLoopbackListenAddress(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:3242": true,
		"127.99.1.2:0":   true,
		"localhost:3242": true,
		"[::1]:3242":     true,
		"[::1%1]:3242":   true,
		":3242":          false,
		"0.0.0.0:3242":   false,
		"[::]:3242":      false,
		"192.0.2.1:3242": false,
		"viiper.test:42": false,
		"not-an-address": false,
	}

	for addr, expected := range tests {
		t.Run(addr, func(t *testing.T) {
			assert.Equal(t, expected, isLoopbackListenAddress(addr))
		})
	}
}

func TestValidateSecurityConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		config    ServerConfig
		wantError string
	}{
		{
			name:   "explicit local development opt-out",
			addr:   "127.0.0.1:3242",
			config: ServerConfig{},
		},
		{
			name:      "authenticated localhost needs credential",
			addr:      "127.0.0.1:3242",
			config:    ServerConfig{RequireLocalHostAuth: true},
			wantError: "authentication is required for localhost",
		},
		{
			name:   "authenticated localhost",
			addr:   "127.0.0.1:3242",
			config: ServerConfig{RequireLocalHostAuth: true, Password: "secret"},
		},
		{
			name:      "wildcard needs credential",
			addr:      ":3242",
			config:    ServerConfig{},
			wantError: "may accept remote connections",
		},
		{
			name:      "specific remote interface needs credential",
			addr:      "192.0.2.10:3242",
			config:    ServerConfig{},
			wantError: "may accept remote connections",
		},
		{
			name:   "explicit authenticated remote listener",
			addr:   ":3242",
			config: ServerConfig{Password: "secret"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSecurityConfiguration(test.addr, test.config)
			if test.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantError)
		})
	}
}
