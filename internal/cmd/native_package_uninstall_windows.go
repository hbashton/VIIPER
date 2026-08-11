//go:build windows

package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const nativeFileDispositionInfoClass = 4

var setNativeFileInformationByHandle = windows.NewLazySystemDLL(
	"kernel32.dll",
).NewProc("SetFileInformationByHandle")

type windowsNativePackageUninstallSnapshot struct {
	config                  mgr.Config
	status                  svc.Status
	securityDescriptor      string
	recoveryActions         []mgr.RecoveryAction
	recoveryResetSeconds    uint32
	recoverNonCrash         bool
	serviceExecutable       string
	serviceExecutableSHA256 string
}

type windowsNativePackageUninstallFile struct {
	kind     string
	path     string
	hash     string
	identity windowsNativePackageUninstallFileIdentity
	handle   windows.Handle
}

type windowsNativePackageUninstallFileIdentity struct {
	volumeSerialNumber uint32
	fileIndex          uint64
}

type windowsNativePackageUninstallLiveLog struct {
	path     string
	identity windowsNativePackageUninstallFileIdentity
	handle   windows.Handle
}

type windowsNativePackageUninstallTransaction struct {
	logger  *slog.Logger
	request nativePackageUninstallRequest

	releasePackageMutex func()
	releaseServiceMutex func()
	helperHandles       []windows.Handle
	managedDirectories  []windows.Handle
	helperHandle        windows.Handle

	userSID     string
	manager     nativeSCM
	service     nativeManagedService
	snapshot    *windowsNativePackageUninstallSnapshot
	ownedFiles  []*windowsNativePackageUninstallFile
	liveLog     *windowsNativePackageUninstallLiveLog
	liveLogPath string

	closed bool
}

func uninstallNativePackage(
	ctx context.Context,
	logger *slog.Logger,
	request nativePackageUninstallRequest,
) error {
	transaction := &windowsNativePackageUninstallTransaction{logger: logger, request: request}
	return runNativePackageUninstallTransaction(ctx, logger, transaction)
}

func remainingNativePackageUninstallBudget(ctx context.Context) (time.Duration, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nativePackageTransactionTimeout, nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, context.DeadlineExceeded
	}
	return remaining, nil
}

func (t *windowsNativePackageUninstallTransaction) LockPackage(ctx context.Context) error {
	budget, err := remainingNativePackageUninstallBudget(ctx)
	if err != nil {
		return err
	}
	release, err := acquireNamedNativePackageMutex(nativePackageMutexName, budget)
	if err != nil {
		return err
	}
	t.releasePackageMutex = release
	return nil
}

func (t *windowsNativePackageUninstallTransaction) LockService(ctx context.Context) error {
	if t.releasePackageMutex == nil {
		return errors.New("native package mutex must be held before the broker service mutex")
	}
	budget, err := remainingNativePackageUninstallBudget(ctx)
	if err != nil {
		return err
	}
	release, err := acquireNativeInstallMutex(budget)
	if err != nil {
		return err
	}
	t.releaseServiceMutex = release
	return nil
}

func (t *windowsNativePackageUninstallTransaction) Preflight(ctx context.Context) error {
	if t.releasePackageMutex == nil || t.releaseServiceMutex == nil {
		return errors.New("native package uninstall mutex order is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	userSID, err := resolveNativeInstallingUserSID(t.request.targetUserSID)
	if err != nil {
		return fmt.Errorf("resolve exact native broker credential owner: %w", err)
	}
	t.userSID = userSID

	directoryHandles, err := lockNativePackageDirectoryChain(filepath.Dir(t.request.driverHelper))
	if err != nil {
		return fmt.Errorf("lock packaged driver helper directory chain: %w", err)
	}
	t.helperHandles = append(t.helperHandles, directoryHandles...)
	helper, err := lockNativePackageInput(t.request.driverHelper)
	if err != nil {
		return fmt.Errorf("lock packaged driver helper: %w", err)
	}
	t.helperHandle = helper
	helperHash, err := hashNativePackageHandle(helper)
	if err != nil {
		return fmt.Errorf("hash packaged driver helper: %w", err)
	}
	if !strings.EqualFold(helperHash, t.request.expectedHelperSHA256) {
		return fmt.Errorf("packaged driver helper SHA-256=%s expected=%s",
			helperHash, t.request.expectedHelperSHA256)
	}
	if err := requireNativePackagePE(helper); err != nil {
		return fmt.Errorf("validate packaged driver helper image: %w", err)
	}
	return nil
}

func (t *windowsNativePackageUninstallTransaction) InspectService(
	ctx context.Context,
) (nativePackageUninstallServiceSnapshot, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("connect to SCM: %w", err)
	}
	t.manager = &windowsNativeSCM{manager: manager}
	service, err := t.manager.OpenService(NativeBrokerServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		if waitErr := waitForNativePackageServiceDeletion(ctx, t.manager); waitErr != nil {
			return nativePackageUninstallServiceSnapshot{}, fmt.Errorf(
				"reconcile previously committed %s deletion: %w",
				NativeBrokerServiceName, waitErr,
			)
		}
		if inspectErr := t.inspectOrphanedExactManagedFiles(); inspectErr != nil {
			return nativePackageUninstallServiceSnapshot{}, inspectErr
		}
		return nativePackageUninstallServiceSnapshot{}, nil
	}
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		if err := t.inspectOrphanedExactManagedFiles(); err != nil {
			return nativePackageUninstallServiceSnapshot{}, err
		}
		return nativePackageUninstallServiceSnapshot{}, nil
	}
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("open %s: %w",
			NativeBrokerServiceName, err)
	}
	t.service = service
	config, err := service.Config()
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("query %s config: %w",
			NativeBrokerServiceName, err)
	}
	executable, err := nativeServiceExecutableFromCommandLine(config.BinaryPathName)
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("parse %s executable: %w",
			NativeBrokerServiceName, err)
	}
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("resolve Program Files: %w", err)
	}
	if _, err := nativeServiceExecutableParent(programFiles, executable); err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf(
			"refusing to stop non-owned %s: %w", NativeBrokerServiceName, err,
		)
	}
	keyPath, err := nativeServiceKeyFilePath()
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, err
	}
	expectedConfig, _, err := nativeBrokerServiceConfiguration(executable, keyPath)
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, err
	}
	securityDescriptor, err := service.SecurityDescriptor()
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("query %s security: %w",
			NativeBrokerServiceName, err)
	}
	recovery, err := service.RecoveryActions()
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("query %s recovery actions: %w",
			NativeBrokerServiceName, err)
	}
	reset, err := service.ResetPeriod()
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("query %s recovery reset: %w",
			NativeBrokerServiceName, err)
	}
	nonCrash, err := service.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("query %s recovery mode: %w",
			NativeBrokerServiceName, err)
	}
	if !isCanonicalNativePackageService(
		config, expectedConfig, securityDescriptor, recovery, reset, nonCrash,
	) {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf(
			"refusing to stop %s because its LocalSystem configuration, security, or recovery ownership is not exact",
			NativeBrokerServiceName,
		)
	}
	status, err := service.Query()
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf("query %s state: %w",
			NativeBrokerServiceName, err)
	}
	status, err = settleNativeServiceSnapshot(ctx, service, status, waitContext)
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, err
	}
	if status.State != svc.Running && status.State != svc.Stopped {
		return nativePackageUninstallServiceSnapshot{}, fmt.Errorf(
			"refusing native package removal while %s is in state %d",
			NativeBrokerServiceName, status.State,
		)
	}
	broker, err := t.inspectExactBrokerFile(executable, true)
	if err != nil {
		return nativePackageUninstallServiceSnapshot{}, err
	}
	if broker == nil {
		return nativePackageUninstallServiceSnapshot{}, errors.New("exact native broker executable is absent")
	}
	if err := t.inspectCredentialFiles(true, status.State == svc.Running); err != nil {
		return nativePackageUninstallServiceSnapshot{}, err
	}
	t.snapshot = &windowsNativePackageUninstallSnapshot{
		config: config, status: status, securityDescriptor: securityDescriptor,
		recoveryActions:      append([]mgr.RecoveryAction(nil), recovery...),
		recoveryResetSeconds: reset, recoverNonCrash: nonCrash,
		serviceExecutable: executable, serviceExecutableSHA256: broker.hash,
	}
	if err := t.inspectOtherExactBrokerFiles(executable); err != nil {
		return nativePackageUninstallServiceSnapshot{}, err
	}
	return nativePackageUninstallServiceSnapshot{
		exists: true, wasRunning: status.State == svc.Running, opaque: t.snapshot,
	}, nil
}

func (t *windowsNativePackageUninstallTransaction) inspectOrphanedExactManagedFiles() error {
	if err := t.inspectCredentialFiles(false, false); err != nil {
		return err
	}
	return t.inspectOtherExactBrokerFiles("")
}

func (t *windowsNativePackageUninstallTransaction) inspectOtherExactBrokerFiles(exclude string) error {
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return fmt.Errorf("resolve Program Files: %w", err)
	}
	candidates := []string{
		filepath.Join(filepath.Clean(programFiles), "VIIPER", "viiper.exe"),
		filepath.Join(filepath.Clean(programFiles), "DS4Windows", "VIIPER", "viiper.exe"),
	}
	for _, candidate := range candidates {
		if exclude != "" && strings.EqualFold(filepath.Clean(candidate), filepath.Clean(exclude)) {
			continue
		}
		if t.hasOwnedFile(candidate) {
			continue
		}
		if _, err := t.inspectExactBrokerFile(candidate, false); err != nil {
			return err
		}
	}
	return nil
}

func (t *windowsNativePackageUninstallTransaction) inspectExactBrokerFile(
	path string,
	required bool,
) (*windowsNativePackageUninstallFile, error) {
	attributes, err := nativePathAttributes(path)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			if required {
				return nil, fmt.Errorf("exact native broker executable is missing: %s", path)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("inspect exact managed broker path %s: %w", path, err)
	}
	if attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		if required {
			return nil, fmt.Errorf("exact native broker path is not a regular non-reparse file: %s", path)
		}
		t.logger.Warn("Leaving non-owned file at an exact native broker path", "path", path)
		return nil, nil
	}
	if err := t.lockExactBrokerDirectoryChain(path); err != nil {
		if required {
			return nil, fmt.Errorf("lock exact installer-owned native broker directories: %w", err)
		}
		t.logger.Warn("Leaving broker path whose exact installer ownership did not verify",
			"path", path, "error", err)
		return nil, nil
	}
	owned, err := lockNativePackageUninstallFile(
		path, "broker", nativeBrokerExecutableSDDL, true,
	)
	if err != nil {
		if required {
			return nil, fmt.Errorf("snapshot exact installer-owned native broker: %w", err)
		}
		t.logger.Warn("Leaving broker path that changed during exact ownership snapshot",
			"path", path, "error", err)
		return nil, nil
	}
	t.ownedFiles = append(t.ownedFiles, owned)
	return owned, nil
}

// lockExactBrokerDirectoryChain retains every ancestor against rename while
// validating the package-owned directories below Program Files. Do not call
// lockNativePriorServiceExecutable here: its read-only leaf handle deliberately
// denies delete sharing and would make the subsequent exact DELETE-capable
// snapshot fail with ERROR_SHARING_VIOLATION on every healthy installation.
func (t *windowsNativePackageUninstallTransaction) lockExactBrokerDirectoryChain(
	executable string,
) error {
	programFiles, err := windows.KnownFolderPath(
		windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT,
	)
	if err != nil {
		return fmt.Errorf("resolve Program Files: %w", err)
	}
	parent, err := nativeServiceExecutableParent(programFiles, executable)
	if err != nil {
		return err
	}
	chain, err := lockNativePackageDirectoryChain(parent)
	if err != nil {
		return err
	}
	validated := make([]windows.Handle, 0, 2)
	fail := func(failErr error) error {
		closeNativePackageUninstallHandles(validated)
		closeNativePackageUninstallHandles(chain)
		return failErr
	}
	relative, err := filepath.Rel(filepath.Clean(programFiles), filepath.Clean(parent))
	if err != nil || relative == "." || filepath.IsAbs(relative) ||
		relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fail(fmt.Errorf("exact native broker parent escaped Program Files: %s", parent))
	}
	current := filepath.Clean(programFiles)
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return fail(fmt.Errorf("exact native broker parent contains an unsafe component: %s", parent))
		}
		current = filepath.Join(current, component)
		handle, openErr := openNativePathWithoutReparse(
			current, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, true,
		)
		if openErr != nil {
			return fail(fmt.Errorf("open protected broker directory %s: %w", current, openErr))
		}
		validated = append(validated, handle)
		if securityErr := validateNativeSecurityDescriptor(
			handle, nativeBrokerDirectorySDDL,
		); securityErr != nil {
			return fail(fmt.Errorf("validate protected broker directory %s: %w", current, securityErr))
		}
	}
	t.managedDirectories = append(t.managedDirectories, chain...)
	t.managedDirectories = append(t.managedDirectories, validated...)
	return nil
}

func (t *windowsNativePackageUninstallTransaction) inspectCredentialFiles(
	required bool,
	brokerMayWriteLog bool,
) error {
	keyPath, err := nativeServiceKeyFilePath()
	if err != nil {
		return err
	}
	directory := filepath.Dir(keyPath)
	attributes, err := nativePathAttributes(directory)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			if required {
				return errors.New("exact native broker credential directory is missing")
			}
			return nil
		}
		return fmt.Errorf("inspect exact credential directory: %w", err)
	}
	if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		if required {
			return errors.New("exact native broker credential path is not a regular directory")
		}
		t.logger.Warn("Leaving non-owned native credential path", "path", directory)
		return nil
	}
	chain, err := lockNativePackageDirectoryChain(directory)
	if err != nil {
		if required {
			return fmt.Errorf("lock exact native credential directory chain: %w", err)
		}
		t.logger.Warn("Leaving native credential path with unsafe ancestors",
			"path", directory, "error", err)
		return nil
	}
	directoryHandle, err := openNativePathWithoutReparse(
		directory, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, true,
	)
	if err != nil {
		closeNativePackageUninstallHandles(chain)
		return fmt.Errorf("open exact native credential directory: %w", err)
	}
	if err := validateNativeSecurityDescriptor(
		directoryHandle, nativeCredentialDirectorySDDL(t.userSID),
	); err != nil {
		windows.CloseHandle(directoryHandle) //nolint:errcheck
		closeNativePackageUninstallHandles(chain)
		if required {
			return fmt.Errorf("validate exact native credential directory ownership: %w", err)
		}
		t.logger.Warn("Leaving native credential directory whose exact ownership did not verify",
			"path", directory, "error", err)
		return nil
	}
	t.managedDirectories = append(t.managedDirectories, chain...)
	t.managedDirectories = append(t.managedDirectories, directoryHandle)
	credential, err := lockNativePackageUninstallFile(
		keyPath, "credential", nativeCredentialFileSDDL(t.userSID), false,
	)
	if err != nil {
		if (errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)) && !required {
			credential = nil
		} else {
			return fmt.Errorf("snapshot exact native broker credential: %w", err)
		}
	}
	if required && credential == nil {
		return errors.New("exact native broker credential is missing")
	}
	if credential != nil {
		t.ownedFiles = append(t.ownedFiles, credential)
	}
	logPath := filepath.Join(directory, nativeBrokerLogName)
	if brokerMayWriteLog {
		t.liveLogPath = logPath
		liveLog, err := lockNativePackageUninstallLiveLog(logPath)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return nil
			}
			return fmt.Errorf("snapshot active exact native broker log identity: %w", err)
		}
		t.liveLog = liveLog
		return nil
	}
	logFile, err := lockNativePackageUninstallFile(logPath, "broker-log", "", false)
	if err != nil {
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return fmt.Errorf("snapshot exact native broker log: %w", err)
		}
		logFile = nil
	}
	if logFile != nil {
		t.ownedFiles = append(t.ownedFiles, logFile)
	}
	return nil
}

func lockNativePackageUninstallLiveLog(
	path string,
) (*windowsNativePackageUninstallLiveLog, error) {
	pointer, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	// The trusted broker opens its log for writing with read/write sharing. This
	// probe therefore cannot request DELETE yet, but its retained identity lets
	// us prove that the stronger post-STOP handle names the same exact file.
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	fail := func(failErr error) (*windowsNativePackageUninstallLiveLog, error) {
		windows.CloseHandle(handle) //nolint:errcheck
		return nil, failErr
	}
	info := nativeFileAttributeTagInfo{}
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fail(err)
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return fail(errors.New("active managed broker log is not a regular non-reparse file"))
	}
	identity, err := nativePackageUninstallFileIdentity(handle)
	if err != nil {
		return fail(err)
	}
	return &windowsNativePackageUninstallLiveLog{
		path: filepath.Clean(path), identity: identity, handle: handle,
	}, nil
}

func lockNativePackageUninstallFile(
	path, kind, expectedSDDL string,
	requirePE bool,
) (*windowsNativePackageUninstallFile, error) {
	pointer, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.DELETE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	fail := func(failErr error) (*windowsNativePackageUninstallFile, error) {
		windows.CloseHandle(handle) //nolint:errcheck
		return nil, failErr
	}
	info := nativeFileAttributeTagInfo{}
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fail(err)
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return fail(errors.New("managed uninstall target is not a regular non-reparse file"))
	}
	identity, err := nativePackageUninstallFileIdentity(handle)
	if err != nil {
		return fail(err)
	}
	if expectedSDDL != "" {
		if err := validateNativeSecurityDescriptor(handle, expectedSDDL); err != nil {
			return fail(err)
		}
	}
	if requirePE {
		if err := requireNativePackagePE(handle); err != nil {
			return fail(err)
		}
	}
	hash, err := hashNativePackageHandle(handle)
	if err != nil {
		return fail(err)
	}
	return &windowsNativePackageUninstallFile{
		kind: kind, path: filepath.Clean(path), hash: hash,
		identity: identity, handle: handle,
	}, nil
}

func nativePackageUninstallFileIdentity(
	handle windows.Handle,
) (windowsNativePackageUninstallFileIdentity, error) {
	info := windows.ByHandleFileInformation{}
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return windowsNativePackageUninstallFileIdentity{}, fmt.Errorf("query managed file identity: %w", err)
	}
	if err := validateNativeFileLinkCount(info.NumberOfLinks); err != nil {
		return windowsNativePackageUninstallFileIdentity{}, err
	}
	return windowsNativePackageUninstallFileIdentity{
		volumeSerialNumber: info.VolumeSerialNumber,
		fileIndex:          uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

func (t *windowsNativePackageUninstallTransaction) hasOwnedFile(path string) bool {
	for _, file := range t.ownedFiles {
		if strings.EqualFold(file.path, filepath.Clean(path)) {
			return true
		}
	}
	return false
}

func (t *windowsNativePackageUninstallTransaction) StopService(
	ctx context.Context,
	snapshot nativePackageUninstallServiceSnapshot,
) error {
	windowsSnapshot, err := t.requireSnapshot(snapshot)
	if err != nil {
		return err
	}
	if windowsSnapshot == nil {
		return nil
	}
	if err := t.verifyExactServiceSnapshot(ctx, windowsSnapshot, false); err != nil {
		return err
	}
	if err := stopNativeService(ctx, t.service, waitContext); err != nil {
		return err
	}
	status, err := t.service.Query()
	if err != nil {
		return fmt.Errorf("verify exact native broker stopped: %w", err)
	}
	if status.State != svc.Stopped {
		return fmt.Errorf("exact native broker remained in state %d after stop", status.State)
	}
	if err := t.promoteNativePackageUninstallLiveLog(ctx); err != nil {
		return &nativePackageUninstallUnsafeRestoreError{cause: err}
	}
	return nil
}

func (t *windowsNativePackageUninstallTransaction) promoteNativePackageUninstallLiveLog(
	ctx context.Context,
) error {
	if t.liveLogPath == "" {
		return nil
	}
	var owned *windowsNativePackageUninstallFile
	for {
		var err error
		owned, err = lockNativePackageUninstallFile(t.liveLogPath, "broker-log", "", false)
		if err == nil {
			break
		}
		if (errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)) &&
			t.liveLog == nil {
			t.liveLogPath = ""
			return nil
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return fmt.Errorf("lock stopped exact native broker log: %w", err)
		}
		if err := waitContext(ctx, 25*time.Millisecond); err != nil {
			return fmt.Errorf("wait for stopped exact native broker log handle: %w", err)
		}
	}
	if t.liveLog != nil && owned.identity != t.liveLog.identity {
		windows.CloseHandle(owned.handle) //nolint:errcheck
		return errors.New("exact native broker log identity changed across service stop")
	}
	if t.liveLog != nil {
		if err := windows.CloseHandle(t.liveLog.handle); err != nil {
			windows.CloseHandle(owned.handle) //nolint:errcheck
			return fmt.Errorf("close active exact native broker log identity: %w", err)
		}
		t.liveLog = nil
	}
	t.liveLogPath = ""
	t.ownedFiles = append(t.ownedFiles, owned)
	return nil
}

func (t *windowsNativePackageUninstallTransaction) RemoveDriver(
	ctx context.Context,
) (nativePackageRemoveResult, error) {
	if err := ctx.Err(); err != nil {
		return nativePackageRemoveResult{serviceRestoreVerified: true}, err
	}
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) {
		return nativePackageRemoveResult{serviceRestoreVerified: true}, context.DeadlineExceeded
	}
	arguments := []string{
		"remove", "--transaction-deadline-unix-ms", strconv.FormatInt(deadline.UnixMilli(), 10),
	}
	// Never use CommandContext or kill this process: once SetupAPI mutation starts,
	// ViiperUdeCtl owns the exact Driver Store backup and cooperative rollback.
	command := exec.Command(t.request.driverHelper, arguments...)
	command.Dir = filepath.Dir(t.request.driverHelper)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return nativePackageRemoveResult{serviceRestoreVerified: true}, err
	}
	waitErr := command.Wait()
	exitCode := 0
	if waitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) {
			return nativePackageRemoveResult{}, waitErr
		}
		exitCode = exitError.ExitCode()
	}
	result, proofErr := parseNativePackageRemoveProof(output.String(), exitCode)
	if proofErr != nil {
		if waitErr != nil {
			return nativePackageRemoveResult{}, fmt.Errorf("%w (process: %v)", proofErr, waitErr)
		}
		return nativePackageRemoveResult{}, proofErr
	}
	return result, nil
}

func (t *windowsNativePackageUninstallTransaction) Cleanup(
	ctx context.Context,
	snapshot nativePackageUninstallServiceSnapshot,
) (bool, error) {
	windowsSnapshot, err := t.requireSnapshot(snapshot)
	if err != nil {
		return false, err
	}
	if windowsSnapshot != nil {
		if err := t.verifyExactServiceSnapshot(ctx, windowsSnapshot, true); err != nil {
			return false, fmt.Errorf("revalidate exact native broker before delete: %w", err)
		}
		if err := t.service.Delete(); err != nil &&
			!errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return false, fmt.Errorf("delete exact %s after driver removal: %w",
				NativeBrokerServiceName, err)
		}
		if err := t.service.Close(); err != nil {
			return false, fmt.Errorf("close exact %s after delete: %w", NativeBrokerServiceName, err)
		}
		t.service = nil
		if err := waitForNativePackageServiceDeletion(ctx, t.manager); err != nil {
			return false, fmt.Errorf("reconcile exact %s deletion: %w", NativeBrokerServiceName, err)
		}
	}
	var cleanupErrors []error
	cleanupRebootRequired := false
	for _, file := range t.ownedFiles {
		if file.handle == 0 {
			continue
		}
		actualHash, hashErr := hashNativePackageHandle(file.handle)
		if hashErr != nil {
			cleanupErrors = append(cleanupErrors,
				fmt.Errorf("revalidate exact %s before delete: %w", file.kind, hashErr))
			continue
		}
		if !strings.EqualFold(actualHash, file.hash) {
			cleanupErrors = append(cleanupErrors,
				fmt.Errorf("refusing to delete exact %s because its locked hash changed", file.kind))
			continue
		}
		if err := deleteNativePackageUninstallFileHandle(file.handle); err != nil {
			isCurrentExecutable, identityErr := nativePackageUninstallIsCurrentExecutable(file)
			if identityErr == nil && isCurrentExecutable &&
				(errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
					errors.Is(err, windows.ERROR_SHARING_VIOLATION)) {
				if scheduleErr := scheduleNativePackageUninstallFileAtReboot(file.path); scheduleErr == nil {
					cleanupRebootRequired = true
					if closeErr := windows.CloseHandle(file.handle); closeErr != nil {
						cleanupErrors = append(cleanupErrors,
							fmt.Errorf("close reboot-scheduled exact %s %s: %w", file.kind, file.path, closeErr))
					}
					file.handle = 0
					continue
				} else {
					cleanupErrors = append(cleanupErrors,
						fmt.Errorf("schedule running exact %s %s for reboot deletion: %w", file.kind, file.path, scheduleErr))
					continue
				}
			}
			cleanupErrors = append(cleanupErrors,
				fmt.Errorf("delete exact installer-owned %s %s: %w", file.kind, file.path, err))
			if identityErr != nil {
				cleanupErrors = append(cleanupErrors,
					fmt.Errorf("identify failed exact %s deletion as the running executable: %w", file.kind, identityErr))
			}
			continue
		}
		if err := windows.CloseHandle(file.handle); err != nil {
			cleanupErrors = append(cleanupErrors,
				fmt.Errorf("close deleted exact %s %s: %w", file.kind, file.path, err))
		}
		file.handle = 0
	}
	return cleanupRebootRequired, errors.Join(cleanupErrors...)
}

func deleteNativePackageUninstallFileHandle(handle windows.Handle) error {
	disposition := struct{ DeleteFile byte }{DeleteFile: 1}
	result, _, callErr := setNativeFileInformationByHandle.Call(
		uintptr(handle),
		nativeFileDispositionInfoClass,
		uintptr(unsafe.Pointer(&disposition)),
		unsafe.Sizeof(disposition),
	)
	if result != 0 {
		return nil
	}
	if callErr != nil && !errors.Is(callErr, syscall.Errno(0)) {
		return callErr
	}
	return syscall.EINVAL
}

func nativePackageUninstallIsCurrentExecutable(
	file *windowsNativePackageUninstallFile,
) (bool, error) {
	if file == nil || file.handle == 0 {
		return false, errors.New("exact managed file snapshot is unavailable")
	}
	executable, err := currentExecutable()
	if err != nil {
		return false, err
	}
	pointer, err := windows.UTF16PtrFromString(filepath.Clean(executable))
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	info := nativeFileAttributeTagInfo{}
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		return false, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return false, errors.New("current executable is not a regular non-reparse file")
	}
	identity, err := nativePackageUninstallFileIdentity(handle)
	if err != nil {
		return false, err
	}
	return identity == file.identity, nil
}

func scheduleNativePackageUninstallFileAtReboot(path string) error {
	pointer, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		pointer, nil,
		windows.MOVEFILE_DELAY_UNTIL_REBOOT|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func (t *windowsNativePackageUninstallTransaction) RestoreService(
	ctx context.Context,
	snapshot nativePackageUninstallServiceSnapshot,
) error {
	windowsSnapshot, err := t.requireSnapshot(snapshot)
	if err != nil {
		return err
	}
	if windowsSnapshot == nil {
		return nil
	}
	if err := t.verifyOwnedFileSnapshots(); err != nil {
		return fmt.Errorf("revalidate exact native broker files before restart: %w", err)
	}
	if err := t.verifyExactServiceSnapshot(ctx, windowsSnapshot, false); err != nil {
		return fmt.Errorf("revalidate exact native broker service before restart: %w", err)
	}
	if snapshot.wasRunning {
		return reconcileNativePackageServiceRunning(ctx, t.service)
	}
	return stopNativeService(ctx, t.service, waitContext)
}

func (t *windowsNativePackageUninstallTransaction) requireSnapshot(
	snapshot nativePackageUninstallServiceSnapshot,
) (*windowsNativePackageUninstallSnapshot, error) {
	if snapshot.exists != (t.snapshot != nil) {
		return nil, errors.New("native broker service existence changed after snapshot")
	}
	if t.snapshot == nil {
		if snapshot.opaque != nil || snapshot.wasRunning {
			return nil, errors.New("absent native broker snapshot carried mutable state")
		}
		return nil, nil
	}
	if snapshot.opaque != t.snapshot || snapshot.wasRunning != (t.snapshot.status.State == svc.Running) {
		return nil, errors.New("native broker service snapshot identity changed")
	}
	return t.snapshot, nil
}

func (t *windowsNativePackageUninstallTransaction) verifyExactServiceSnapshot(
	ctx context.Context,
	snapshot *windowsNativePackageUninstallSnapshot,
	requireStopped bool,
) error {
	if t.service == nil {
		return errors.New("exact native broker service handle is unavailable")
	}
	config, err := t.service.Config()
	if err != nil {
		return fmt.Errorf("query exact native broker config: %w", err)
	}
	if !nativeServiceConfigsEqual(config, snapshot.config) {
		return errors.New("exact native broker configuration changed during package removal")
	}
	securityDescriptor, err := t.service.SecurityDescriptor()
	if err != nil {
		return fmt.Errorf("query exact native broker security: %w", err)
	}
	if err := compareNativeSecurityDescriptorStrings(
		securityDescriptor, snapshot.securityDescriptor,
	); err != nil {
		return fmt.Errorf("exact native broker security changed during package removal: %w", err)
	}
	recovery, err := t.service.RecoveryActions()
	if err != nil {
		return err
	}
	reset, err := t.service.ResetPeriod()
	if err != nil {
		return err
	}
	nonCrash, err := t.service.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return err
	}
	if !slices.Equal(recovery, snapshot.recoveryActions) ||
		reset != snapshot.recoveryResetSeconds || nonCrash != snapshot.recoverNonCrash {
		return errors.New("exact native broker recovery ownership changed during package removal")
	}
	status, err := t.service.Query()
	if err != nil {
		return err
	}
	status, err = settleNativeServiceSnapshot(ctx, t.service, status, waitContext)
	if err != nil {
		return err
	}
	if status.State != svc.Running && status.State != svc.Stopped {
		return fmt.Errorf("exact native broker entered unexpected state %d", status.State)
	}
	if requireStopped && status.State != svc.Stopped {
		return errors.New("exact native broker restarted before owned cleanup")
	}
	return nil
}

func (t *windowsNativePackageUninstallTransaction) verifyOwnedFileSnapshots() error {
	for _, file := range t.ownedFiles {
		if file.handle == 0 {
			return fmt.Errorf("exact %s snapshot handle was released", file.kind)
		}
		hash, err := hashNativePackageHandle(file.handle)
		if err != nil {
			return fmt.Errorf("hash exact %s snapshot: %w", file.kind, err)
		}
		if !strings.EqualFold(hash, file.hash) {
			return fmt.Errorf("exact %s snapshot hash changed", file.kind)
		}
	}
	return nil
}

func (t *windowsNativePackageUninstallTransaction) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	var closeErrors []error
	if t.liveLog != nil {
		if err := windows.CloseHandle(t.liveLog.handle); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close active exact native broker log identity: %w", err))
		}
		t.liveLog = nil
	}
	t.liveLogPath = ""
	for index := len(t.ownedFiles) - 1; index >= 0; index-- {
		file := t.ownedFiles[index]
		if file.handle != 0 {
			if err := windows.CloseHandle(file.handle); err != nil {
				closeErrors = append(closeErrors,
					fmt.Errorf("close exact %s snapshot: %w", file.kind, err))
			}
			file.handle = 0
		}
	}
	closeNativePackageUninstallHandles(t.managedDirectories)
	t.managedDirectories = nil
	if t.helperHandle != 0 {
		if err := windows.CloseHandle(t.helperHandle); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close packaged driver helper: %w", err))
		}
		t.helperHandle = 0
	}
	closeNativePackageUninstallHandles(t.helperHandles)
	t.helperHandles = nil
	if t.service != nil {
		if err := t.service.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close exact native broker service: %w", err))
		}
		t.service = nil
	}
	if t.manager != nil {
		if err := t.manager.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close SCM: %w", err))
		}
		t.manager = nil
	}
	// Release nested thread-owned mutexes in reverse global acquisition order.
	if t.releaseServiceMutex != nil {
		t.releaseServiceMutex()
		t.releaseServiceMutex = nil
	}
	if t.releasePackageMutex != nil {
		t.releasePackageMutex()
		t.releasePackageMutex = nil
	}
	return errors.Join(closeErrors...)
}

func closeNativePackageUninstallHandles(handles []windows.Handle) {
	for index := len(handles) - 1; index >= 0; index-- {
		windows.CloseHandle(handles[index]) //nolint:errcheck
	}
}
