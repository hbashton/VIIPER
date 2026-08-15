package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedFileAppendsAcrossRecoveryRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.log")
	if err := os.WriteFile(path, []byte("first failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openBoundedFile(path, 0o600, 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err = file.Write([]byte("recovered\n")); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(contents); got != "first failure\nrecovered\n" {
		t.Fatalf("appended log=%q", got)
	}
}

func TestBoundedFileWrapsBeforeDiskLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.log")
	file, err := openBoundedFile(path, 0o600, 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err = file.Write([]byte("old-record\n")); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("new-record\n")); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(contents); got != "new-record\n" || strings.Contains(got, "old") {
		t.Fatalf("wrapped log=%q", got)
	}
}
