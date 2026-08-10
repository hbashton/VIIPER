package cmd

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyTransportAPISecurityPolicy(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		initial   bool
		expected  bool
	}{
		{name: "native forces local authentication", transport: "native-ude", expected: true},
		{name: "usbip preserves explicit local opt-out", transport: "usbip", expected: false},
		{name: "usbip preserves local authentication", transport: "usbip", initial: true, expected: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := api.ServerConfig{RequireLocalHostAuth: test.initial}
			applyTransportAPISecurityPolicy(test.transport, &config)
			assert.Equal(t, test.expected, config.RequireLocalHostAuth)
		})
	}
}

func TestGeneratedAPICredentialLogDoesNotExposeSecret(t *testing.T) {
	const secret = "do-not-print-this-api-secret"
	keyPath := filepath.Join(t.TempDir(), "viiper.key.txt")
	require.NoError(t, os.WriteFile(keyPath, []byte(secret), 0o600))

	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	logGeneratedAPICredential(logger, keyPath)

	assert.Contains(t, output.String(), keyPath)
	assert.NotContains(t, output.String(), secret)
}
