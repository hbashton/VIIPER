package cmd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/Alia5/VIIPER/internal/configpaths"
	"github.com/Alia5/VIIPER/internal/server/api/auth"
	"github.com/stretchr/testify/require"
)

func TestServerKeyFileOmissionPreservesPlatformDefault(t *testing.T) {
	directory, err := configpaths.KeyFileDir()
	require.NoError(t, err)
	path, err := resolveServerKeyFilePath(nil)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(directory, keyFileName), path)
	require.NoError(t, (&Server{}).Validate())
}

func TestServerExplicitKeyFileNeverResolvesDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AppData", "")
	t.Setenv("XDG_CONFIG_HOME", root)
	path := filepath.Join(root, "lab-data", keyFileName)
	resolved, err := resolveServerKeyFilePath(&path)
	require.NoError(t, err)
	require.Equal(t, path, resolved)
	for _, invalid := range []string{"", " ", "relative-key.txt"} {
		_, err := resolveServerKeyFilePath(&invalid)
		require.Error(t, err)
		require.Error(t, (&Server{KeyFile: &invalid}).Validate())
	}
}

func TestServerExplicitInvalidKeyFileRejectsBeforeRuntimeOrTrayStartup(t *testing.T) {
	path := t.TempDir() // A directory, not a regular key file.
	server := Server{KeyFile: &path}
	// Nil loggers are deliberate: validation must return before the runtime
	// prerequisite, tray, listeners, or any authentication-file mutation.
	err := server.StartServer(context.Background(), nil, nil)
	require.ErrorContains(t, err, "regular file")
}

func TestServerExplicitKeyFileCreatesOnceAndReusesSameKey(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "lab-data", keyFileName)
	password, generated, err := loadServerAPIKey(path, true)
	require.NoError(t, err)
	require.True(t, generated)
	require.Len(t, password, auth.AutoGenKeyLength)
	require.Regexp(t, "^[0-9A-Za-z]{16}$", password)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, password, string(data))
	again, generated, err := loadServerAPIKey(path, true)
	require.NoError(t, err)
	require.False(t, generated)
	require.Equal(t, password, again)
	if runtime.GOOS != "windows" {
		fileInfo, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
		dirInfo, err := os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
	}
}

func TestServerExplicitKeyFileReadsExistingPasswordWithoutChangingBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyFileName)
	original := " \tdeployment-password\r\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	password, generated, err := loadServerAPIKey(path, true)
	require.NoError(t, err)
	require.False(t, generated)
	require.Equal(t, "deployment-password", password)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, string(data))
	expected, err := auth.DeriveKey("deployment-password")
	require.NoError(t, err)
	actual, err := auth.DeriveKey(password)
	require.NoError(t, err)
	require.Equal(t, expected, actual)
}

func TestServerExplicitEmptyKeyFailsWithoutReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyFileName)
	require.NoError(t, os.WriteFile(path, []byte(" \r\n"), 0o600))
	password, generated, err := loadServerAPIKey(path, true)
	require.ErrorContains(t, err, "empty")
	require.Empty(t, password)
	require.False(t, generated)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, " \r\n", string(data))
}

func TestServerExplicitKeyCreationNeverOverwritesRaceWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyFileName)
	require.NoError(t, os.WriteFile(path, []byte("race-winner"), 0o600))
	require.ErrorIs(t, createExplicitAPIKey(path, "replacement"), os.ErrExist)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "race-winner", string(data))
}

func TestServerDefaultKeyHandlingRetainsExistingBehavior(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyFileName)
	password, generated, err := loadServerAPIKey(path, false)
	require.NoError(t, err)
	require.True(t, generated)
	require.Len(t, password, auth.AutoGenKeyLength)
	require.NoError(t, os.WriteFile(path, []byte(" \r\n"), 0o600))
	password, generated, err = loadServerAPIKey(path, false)
	require.NoError(t, err)
	require.False(t, generated)
	require.Empty(t, password)
}

func TestServerConfigTemplateOmitsUnsetOptionalKeyFile(t *testing.T) {
	defaults := buildMapFromStruct(reflect.TypeOf(Server{}))
	require.NotContains(t, defaults, "keyFile")
	require.Equal(t, "30s", defaults["connectionTimeout"])
}
