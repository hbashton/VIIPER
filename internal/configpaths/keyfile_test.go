package configpaths

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExplicitKeyFilePathRejectsRelativeOrEmptyPath(t *testing.T) {
	for _, path := range []string{"", " ", "viiper.key.txt", ".", filepath.Join("lab-data", "viiper.key.txt")} {
		t.Run(path, func(t *testing.T) {
			resolved, err := ExplicitKeyFilePath(path)
			require.Error(t, err)
			require.Empty(t, resolved)
		})
	}
}

func TestExplicitKeyFilePathAcceptsMissingAbsoluteFileWithoutCreatingIt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "lab-data", "viiper.key.txt")
	resolved, err := ExplicitKeyFilePath(path)
	require.NoError(t, err)
	require.Equal(t, path, resolved)
	_, err = os.Stat(filepath.Dir(path))
	require.True(t, os.IsNotExist(err))
}

func TestExplicitKeyFilePathRequiresRegularFileAndDirectoryParents(t *testing.T) {
	root := t.TempDir()
	_, err := ExplicitKeyFilePath(root)
	require.Error(t, err)
	file := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(file, []byte("existing"), 0o600))
	resolved, err := ExplicitKeyFilePath(file)
	require.NoError(t, err)
	require.Equal(t, file, resolved)
	_, err = ExplicitKeyFilePath(filepath.Join(file, "viiper.key.txt"))
	require.Error(t, err)
}

func TestExplicitKeyFilePathRejectsLinksIncludingParentAndDanglingLink(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	require.NoError(t, os.Mkdir(targetDir, 0o700))
	target := filepath.Join(targetDir, "key.txt")
	require.NoError(t, os.WriteFile(target, []byte("unchanged"), 0o600))
	link := filepath.Join(root, "linked-key")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	_, err := ExplicitKeyFilePath(link)
	require.Error(t, err)
	_, err = ExplicitConfigFilePath(link)
	require.Error(t, err)
	parentLink := filepath.Join(root, "linked-directory")
	require.NoError(t, os.Symlink(targetDir, parentLink))
	_, err = ExplicitKeyFilePath(filepath.Join(parentLink, "new-key.txt"))
	require.Error(t, err)
	_, err = ExplicitConfigFilePath(filepath.Join(parentLink, "key.txt"))
	require.Error(t, err)
	dangling := filepath.Join(root, "dangling-key")
	require.NoError(t, os.Symlink(filepath.Join(root, "missing"), dangling))
	_, err = ExplicitKeyFilePath(dangling)
	require.Error(t, err)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "unchanged", string(data))
}

func TestExplicitConfigFilePathRequiresExistingAbsoluteFile(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"", "relative.json", root, filepath.Join(root, "missing.json")} {
		resolved, err := ExplicitConfigFilePath(path)
		require.Error(t, err)
		require.Empty(t, resolved)
	}
	path := filepath.Join(root, "server.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	resolved, err := ExplicitConfigFilePath(path)
	require.NoError(t, err)
	require.Equal(t, path, resolved)
}
