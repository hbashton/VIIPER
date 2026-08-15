//go:build windows

package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestNativePackageUninstallSerializesConcurrentPackageTransactions(t *testing.T) {
	requireNativeMutexAdministrator(t)
	name := `VIIPER_NATIVE_UNINSTALL_TEST_` + filepath.Base(t.TempDir())
	releaseFirst, err := acquireNamedNativePackageMutex(name, time.Second)
	if err != nil {
		t.Fatalf("acquire first package owner: %v", err)
	}
	secondResult := make(chan error, 1)
	go func() {
		releaseSecond, secondErr := acquireNamedNativePackageMutex(name, 40*time.Millisecond)
		if releaseSecond != nil {
			releaseSecond()
		}
		secondResult <- secondErr
	}()
	select {
	case secondErr := <-secondResult:
		if secondErr == nil ||
			(!strings.Contains(secondErr.Error(), "still running") &&
				!errors.Is(secondErr, windows.ERROR_ACCESS_DENIED)) {
			releaseFirst()
			t.Fatalf("concurrent package owner error=%v", secondErr)
		}
	case <-time.After(2 * time.Second):
		releaseFirst()
		t.Fatal("concurrent package owner did not respect its bounded wait")
	}
	releaseFirst()

	releaseAfter, err := acquireNamedNativePackageMutex(name, time.Second)
	if err != nil {
		t.Fatalf("package mutex remained stranded after release: %v", err)
	}
	releaseAfter()
}

func TestNativePackageUninstallRenamesRunningImageToUniqueTombstone(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "viiper.exe")
	if err := os.WriteFile(path, []byte("exact-native-broker"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := lockNativePackageUninstallFile(path, "broker", "", false)
	if err != nil {
		t.Fatalf("lock test broker: %v", err)
	}
	defer func() {
		if file.handle != 0 {
			windows.CloseHandle(file.handle) //nolint:errcheck
		}
	}()

	tombstone, err := renameNativePackageUninstallFileToTombstone(file)
	if err != nil {
		t.Fatalf("rename exact broker to reboot tombstone: %v", err)
	}
	if filepath.Dir(tombstone) != root ||
		!strings.HasPrefix(filepath.Base(tombstone), ".viiper.uninstall.") ||
		!strings.HasSuffix(filepath.Base(tombstone), ".delete") {
		t.Fatalf("unsafe reboot tombstone path %q", tombstone)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical broker path remained after tombstone rename: %v", err)
	}
	if err := windows.CloseHandle(file.handle); err != nil {
		t.Fatalf("close renamed exact broker handle: %v", err)
	}
	file.handle = 0
	contents, err := os.ReadFile(tombstone)
	if err != nil {
		t.Fatalf("read renamed tombstone: %v", err)
	}
	if string(contents) != "exact-native-broker" {
		t.Fatalf("renamed tombstone contents=%q", contents)
	}
}

func TestNativePackageUninstallDeletesRetainedExactFileHandle(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "owned.log")
	if err := os.WriteFile(path, []byte("exact-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	owned, err := lockNativePackageUninstallFile(path, "test", "", false)
	if err != nil {
		t.Fatalf("lock exact file: %v", err)
	}
	if err := deleteNativePackageUninstallFileHandle(owned.handle); err != nil {
		_ = os.NewFile(uintptr(owned.handle), path).Close()
		t.Fatalf("mark exact handle for deletion: %v", err)
	}
	if err := os.NewFile(uintptr(owned.handle), path).Close(); err != nil {
		t.Fatalf("close exact deleted handle: %v", err)
	}
	owned.handle = 0
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact handle path still exists: %v", err)
	}
}

func TestNativePackageUninstallDoesNotPrelockBrokerLeafWithoutDeleteSharing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "broker.exe")
	if err := os.WriteFile(path, []byte("MZ-test-image"), 0o600); err != nil {
		t.Fatal(err)
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := windows.CreateFile(
		pointer, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if owned, lockErr := lockNativePackageUninstallFile(path, "broker", "", true); lockErr == nil {
		if owned != nil && owned.handle != 0 {
			_ = windows.CloseHandle(owned.handle)
		}
		_ = windows.CloseHandle(readOnly)
		t.Fatal("a non-delete-shared prelock unexpectedly allowed the exact DELETE-capable snapshot")
	} else if !errors.Is(lockErr, windows.ERROR_SHARING_VIOLATION) {
		_ = windows.CloseHandle(readOnly)
		t.Fatalf("conflicting prelock error=%v, want sharing violation", lockErr)
	}
	if err := windows.CloseHandle(readOnly); err != nil {
		t.Fatal(err)
	}
	owned, err := lockNativePackageUninstallFile(path, "broker", "", true)
	if err != nil {
		t.Fatalf("direct exact broker snapshot: %v", err)
	}
	if err := windows.CloseHandle(owned.handle); err != nil {
		t.Fatal(err)
	}
	owned.handle = 0
}

func TestNativePackageUninstallIdentifiesExactRunningImageByFileID(t *testing.T) {
	t.Parallel()
	executable, err := currentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	pointer, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		pointer, windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	identity, err := nativePackageUninstallFileIdentity(handle)
	if err != nil {
		t.Fatal(err)
	}
	file := &windowsNativePackageUninstallFile{handle: handle, identity: identity}
	current, err := nativePackageUninstallIsCurrentExecutable(file)
	if err != nil {
		t.Fatal(err)
	}
	if !current {
		t.Fatal("exact current executable file ID was not recognized for safe reboot cleanup")
	}
}

func TestNativePackageUninstallRejectsHardLinkedManagedFile(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "owned.log")
	link := filepath.Join(directory, "alias.log")
	if err := os.WriteFile(path, []byte("not-single-link"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, link); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	owned, err := lockNativePackageUninstallFile(path, "test", "", false)
	if owned != nil || err == nil {
		if owned != nil && owned.handle != 0 {
			_ = os.NewFile(uintptr(owned.handle), path).Close()
		}
		t.Fatalf("hard-linked managed file accepted: owned=%+v err=%v", owned, err)
	}
}

func TestNativePackageUninstallPromotesActiveLogIdentityAfterWriterStops(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "active.log")
	writer, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := lockNativePackageUninstallLiveLog(path)
	if err != nil {
		_ = writer.Close()
		t.Fatalf("take active-log identity probe: %v", err)
	}
	defer func() {
		if probe.handle != 0 {
			_ = windows.CloseHandle(probe.handle)
		}
	}()
	if owned, err := lockNativePackageUninstallFile(path, "broker-log", "", false); err == nil {
		if owned != nil && owned.handle != 0 {
			_ = windows.CloseHandle(owned.handle)
		}
		_ = writer.Close()
		t.Fatal("delete-capable log lock unexpectedly succeeded while trusted writer was active")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("stop active log writer: %v", err)
	}
	owned, err := lockNativePackageUninstallFile(path, "broker-log", "", false)
	if err != nil {
		t.Fatalf("promote stopped log lock: %v", err)
	}
	defer func() {
		if owned.handle != 0 {
			_ = windows.CloseHandle(owned.handle)
		}
	}()
	if owned.identity != probe.identity {
		t.Fatalf("promoted identity=%+v probe=%+v", owned.identity, probe.identity)
	}
}

func TestNativePackageUninstallRejectsActiveLogIdentitySwap(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "active.log")
	moved := filepath.Join(directory, "moved.log")
	if err := os.WriteFile(path, []byte("captured"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := lockNativePackageUninstallLiveLog(path)
	if err != nil {
		t.Fatalf("take active-log identity probe: %v", err)
	}
	transaction := &windowsNativePackageUninstallTransaction{
		liveLog: probe, liveLogPath: path,
	}
	defer func() { _ = transaction.Close() }()
	if err := os.Rename(path, moved); err != nil {
		t.Skipf("rename while delete-shared identity probe is held: %v", err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = transaction.promoteNativePackageUninstallLiveLog(context.Background())
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("replacement log identity accepted: %v", err)
	}
	if len(transaction.ownedFiles) != 0 {
		t.Fatalf("replacement log became installer-owned: %+v", transaction.ownedFiles)
	}
}

func TestNativePackageUninstallCapturesLogCreatedBeforeStop(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "created-during-stop.log")
	transaction := &windowsNativePackageUninstallTransaction{liveLogPath: path}
	defer func() { _ = transaction.Close() }()
	if err := os.WriteFile(path, []byte("trusted broker output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := transaction.promoteNativePackageUninstallLiveLog(context.Background()); err != nil {
		t.Fatalf("capture log created before service stop: %v", err)
	}
	if len(transaction.ownedFiles) != 1 || transaction.ownedFiles[0].path != path {
		t.Fatalf("created exact log was not locked: %+v", transaction.ownedFiles)
	}
}
