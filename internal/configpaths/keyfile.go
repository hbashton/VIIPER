package configpaths

import (
	"fmt"
	"os"
	"path/filepath"
)

// ExplicitKeyFilePath validates a deployment-selected key file without consulting
// configuration-home fallbacks or creating files. Missing directories are allowed;
// existing ancestors must be real directories, never links or special files.
func ExplicitKeyFilePath(path string) (string, error) {
	return explicitFilePath(path, "--key-file", true)
}

// ExplicitConfigFilePath requires one existing absolute regular config file.
// It does not search other locations or accept a link to a different file.
func ExplicitConfigFilePath(path string) (string, error) {
	return explicitFilePath(path, "--config", false)
}

func explicitFilePath(path, option string, allowMissing bool) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be a nonempty absolute file path", option)
	}
	path = filepath.Clean(path)
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && current == path && !allowMissing {
			return "", fmt.Errorf("cannot inspect explicit %s file: %w", option, err)
		}
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("cannot inspect explicit %s path: %w", option, err)
		}
		if err == nil {
			if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				return "", fmt.Errorf("explicit %s path must not contain links or reparse points: %s", option, current)
			}
			if current == path && !info.Mode().IsRegular() {
				return "", fmt.Errorf("explicit %s must be a regular file: %s", option, current)
			}
			if current != path && !info.IsDir() {
				return "", fmt.Errorf("explicit %s parent must be a directory: %s", option, current)
			}
		}
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	return path, nil
}
