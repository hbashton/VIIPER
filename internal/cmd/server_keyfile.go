package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Alia5/VIIPER/internal/configpaths"
	"github.com/Alia5/VIIPER/internal/server/api/auth"
)

// Validate also covers CLI/config parsing. StartServer repeats this validation
// for callers that construct Server directly rather than using Kong.
func (s *Server) Validate() error {
	if s.KeyFile == nil {
		return nil
	}
	_, err := configpaths.ExplicitKeyFilePath(*s.KeyFile)
	return err
}

func resolveServerKeyFilePath(explicit *string) (string, error) {
	if explicit != nil {
		return configpaths.ExplicitKeyFilePath(*explicit)
	}
	directory, err := configpaths.KeyFileDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve key file path: %w", err)
	}
	return filepath.Join(directory, keyFileName), nil
}

func loadServerAPIKey(path string, explicit bool) (string, bool, error) {
	if explicit {
		var err error
		path, err = configpaths.ExplicitKeyFilePath(path)
		if err != nil {
			return "", false, err
		}
	}
	if data, err := os.ReadFile(path); err == nil {
		password := strings.TrimSpace(string(data))
		if explicit && password == "" {
			return "", false, fmt.Errorf("explicit API password file is empty: %s", path)
		}
		return password, false, nil
	} else if explicit && !os.IsNotExist(err) {
		return "", false, fmt.Errorf("failed to read explicit API password file: %w", err)
	}

	password, err := auth.GenerateKey()
	if err != nil {
		return "", false, fmt.Errorf("failed to generate new API password: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, fmt.Errorf("failed to create config dir for key file: %w", err)
	}
	if explicit {
		if _, err := configpaths.ExplicitKeyFilePath(path); err != nil {
			return "", false, err
		}
		err = createExplicitAPIKey(path, password)
	} else {
		// Keep the established default-location behavior unchanged.
		err = os.WriteFile(path, []byte(password), 0o600)
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to write new API password to file: %w", err)
	}
	return password, true, nil
}

func createExplicitAPIKey(path, password string) error {
	// Exclusive creation never truncates a key that appeared after the read.
	// On any failure leave the exact path for diagnosis, never try AppData.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(password)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
