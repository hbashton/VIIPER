//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const nativeMutexProbeTestEnvironment = "VIIPER_NATIVE_MUTEX_PROBE_TEST"

func requireNativeMutexAdministrator(t *testing.T) {
	t.Helper()
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("create Administrators SID: %v", err)
	}
	member, err := windows.GetCurrentProcessToken().IsMember(administrators)
	if err != nil {
		t.Fatalf("check Administrators token membership: %v", err)
	}
	if !member {
		t.Skip("Administrators-bound private namespace requires an elevated test token")
	}
}

func TestNativeMutexRejectsPublicNamespaceObjectNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", `Global\VIIPER`, `Local\VIIPER`, `nested/name`} {
		if _, err := nativePrivateMutexName(name); err == nil {
			t.Errorf("nativePrivateMutexName(%q) accepted a namespace escape", name)
		}
	}
}

func TestNativeMutexWaitMillisecondsIsBounded(t *testing.T) {
	t.Parallel()
	if got := nativeMutexWaitMilliseconds(-time.Second); got != 0 {
		t.Fatalf("negative timeout converted to %d, want 0", got)
	}
	if got := nativeMutexWaitMilliseconds(time.Nanosecond); got != 0 {
		t.Fatalf("sub-millisecond timeout converted to %d, want 0", got)
	}
	if got := nativeMutexWaitMilliseconds(time.Second); got != 1000 {
		t.Fatalf("one-second timeout converted to %d, want 1000", got)
	}
	if got := nativeMutexWaitMilliseconds(time.Duration(windows.INFINITE) * time.Millisecond); got != windows.INFINITE-1 {
		t.Fatalf("oversized timeout converted to %d, want %d", got, uint32(windows.INFINITE-1))
	}
}

func TestNativeMutexNestedPackageProbe(t *testing.T) {
	requireNativeMutexAdministrator(t)
	name := "VIIPER_NATIVE_PACKAGE_PROBE_TEST_" + filepath.Base(t.TempDir())
	release, err := acquireNamedNativePackageMutex(name, time.Second)
	if err != nil {
		t.Fatalf("acquire package mutex: %v", err)
	}
	defer release()

	command := exec.Command(os.Args[0], "-test.run=^TestNativeMutexNestedPackageProbeChild$")
	command.Env = append(os.Environ(), nativeMutexProbeTestEnvironment+"="+name)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run nested mutex probe: %v\n%s", err, output)
	}
}

func TestNativeMutexNestedPackageProbeChild(t *testing.T) {
	name := os.Getenv(nativeMutexProbeTestEnvironment)
	if name == "" {
		return
	}
	held, err := nativePackageMutexHeldByAnotherOwner(name)
	if err != nil {
		t.Fatalf("probe parent-owned package mutex: %v", err)
	}
	if !held {
		t.Fatal("parent-owned package mutex was not observed inside the exact private namespace")
	}
}

func TestNativeMutexPrivateNamespaceSourceContract(t *testing.T) {
	t.Parallel()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve native mutex test source")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(current), "native_mutex_windows.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		`nativeMutexObjectSDDL     = "O:BAG:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)"`,
		`windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)`,
		`NewProc("CreateBoundaryDescriptorW")`,
		`NewProc("AddSIDToBoundaryDescriptor")`,
		`NewProc("CreatePrivateNamespaceW")`,
		`NewProc("OpenPrivateNamespaceW")`,
		`NewProc("ClosePrivateNamespace")`,
		`nativeDeleteBoundaryDescriptor.Call`,
		`nativeMutexNamespaceOpenMu.Lock()`,
		`errors.Is(createErr, windows.ERROR_DUP_NAME)`,
		`errors.Is(openErr, windows.ERROR_DUP_NAME)`,
		`scope.namespace = windows.Handle(result)`,
		`createNamedNativeMutex(attributes, name)`,
		`runtime.LockOSThread()`,
		`windows.WAIT_ABANDONED`,
		`windows.OpenMutex(`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("native mutex namespace lost %q", required)
		}
	}
	for _, forbidden := range []string{`Global\VIIPER.NativePackage`, `Global\VIIPER.NativeBroker`} {
		if strings.Contains(text, forbidden) {
			t.Errorf("native mutex helper retains squattable public name %q", forbidden)
		}
	}
	if nativePackageMutexName == nativeInstallMutexName {
		t.Fatal("package and service transactions unexpectedly share one mutex object")
	}
	if strings.ContainsAny(nativePackageMutexName+nativeInstallMutexName, `\\/`) {
		t.Fatal("native mutex object names escape the private namespace")
	}
}

func TestNativeMutexAbsentProbeIsNotHeld(t *testing.T) {
	requireNativeMutexAdministrator(t)
	name := "VIIPER_NATIVE_ABSENT_PROBE_TEST_" + filepath.Base(t.TempDir())
	held, err := nativePackageMutexHeldByAnotherOwner(name)
	if err != nil {
		t.Fatalf("probe absent package mutex: %v", err)
	}
	if held {
		t.Fatal("absent package mutex reported as held")
	}
}
