//go:build windows

package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const nativePackageMutexName = "VIIPER.NativePackage.Install.v1"
const nativePackageTokenSDDL = "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"

var nativePackageDriverFiles = []string{
	"ViiperUde.inf", "ViiperUde.sys", "ViiperUde.cat",
}

type windowsNativePackageTransaction struct {
	logger             *slog.Logger
	request            nativePackageRequest
	nestedBrokerCommit bool

	releaseMutex                 func()
	releaseServiceMutex          func()
	inputHandles                 []windows.Handle
	sourceHandle                 windows.Handle
	helperHandle                 windows.Handle
	nestedBrokerHealthy          bool
	nestedMutationStarted        bool
	nestedRollbackSucceeded      bool
	nestedServiceRollbackSettled bool

	programFiles string
	destination  string
	parent       string
	parentHandle windows.Handle
	parentMade   bool

	manager                nativeSCM
	service                nativeManagedService
	serviceSnapshot        nativePackageServiceSnapshot
	priorServiceExecutable string
	priorExecutableRelease func()
	stoppedTrustedService  bool

	temporaryPath        string
	backupPath           string
	destinationPublished bool
	destinationRelease   func()
	tokenPath            string
	tokenSHA256          string
	tokenHandle          windows.Handle
	installProof         bool
	closed               bool
}

func installNativePackage(
	ctx context.Context,
	logger *slog.Logger,
	request nativePackageRequest,
) error {
	transaction := &windowsNativePackageTransaction{logger: logger, request: request}
	return runNativePackageTransaction(ctx, logger, transaction)
}

func commitNativePackageBroker(
	logger *slog.Logger,
	tokenPath, expectedTokenSHA256, expectedBrokerSHA256, targetUserSID, deadlineUnixMS string,
) (nativePackageBrokerCommitResult, error) {
	preflightFailure := func(err error) (nativePackageBrokerCommitResult, error) {
		return nativePackageBrokerPreflightFailure(err)
	}
	deadlineMilliseconds, err := strconv.ParseInt(deadlineUnixMS, 10, 64)
	if err != nil || deadlineMilliseconds <= 0 {
		return preflightFailure(errors.New("native package transaction deadline must be positive Unix milliseconds"))
	}
	deadline := time.UnixMilli(deadlineMilliseconds)
	if !deadline.After(time.Now()) || deadline.After(time.Now().Add(nativePackageTransactionTimeout)) {
		return preflightFailure(errors.New("native package transaction deadline is expired or outside the package budget"))
	}
	if !filepath.IsAbs(tokenPath) || strings.IndexByte(tokenPath, 0) >= 0 {
		return preflightFailure(errors.New("native package transaction token path must be absolute and contain no NUL"))
	}
	if _, err := validateNativeInstallingUserSID(targetUserSID); err != nil {
		return preflightFailure(fmt.Errorf("validate package transaction target SID: %w", err))
	}
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return preflightFailure(fmt.Errorf("resolve Program Files: %w", err))
	}
	expectedParent := filepath.Join(filepath.Clean(programFiles), "VIIPER")
	base := filepath.Base(tokenPath)
	if !strings.EqualFold(filepath.Dir(filepath.Clean(tokenPath)), expectedParent) ||
		!strings.HasPrefix(strings.ToLower(base), ".viiper.transaction.") ||
		!strings.HasSuffix(strings.ToLower(base), ".token") {
		return preflightFailure(fmt.Errorf("package transaction token escaped the managed VIIPER directory: %s", tokenPath))
	}
	handle, err := lockNativePackageInput(tokenPath)
	if err != nil {
		return preflightFailure(fmt.Errorf("lock package transaction token: %w", err))
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	if err := validateNativeSecurityDescriptor(handle, nativePackageTokenSDDL); err != nil {
		return preflightFailure(fmt.Errorf("validate package transaction token ACL: %w", err))
	}
	hash, err := hashNativePackageHandle(handle)
	if err != nil {
		return preflightFailure(fmt.Errorf("hash package transaction token: %w", err))
	}
	if !strings.EqualFold(hash, expectedTokenSHA256) {
		return preflightFailure(errors.New("package transaction token SHA-256 does not match the active installer"))
	}
	if !nativePackageSHA256.MatchString(expectedBrokerSHA256) {
		return preflightFailure(errors.New("package transaction broker SHA-256 is malformed"))
	}
	held, err := nativePackageMutexHeldByAnotherOwner(nativePackageMutexName)
	if err != nil {
		return preflightFailure(fmt.Errorf("verify outer package transaction mutex: %w", err))
	}
	if !held {
		return preflightFailure(errors.New("outer native package transaction mutex is not held"))
	}
	executable, err := currentExecutable()
	if err != nil {
		return preflightFailure(fmt.Errorf("resolve nested broker executable: %w", err))
	}
	transaction := &windowsNativePackageTransaction{
		logger: logger,
		request: nativePackageRequest{
			brokerSource: executable, expectedBrokerSHA256: expectedBrokerSHA256,
			targetUserSID: targetUserSID,
		},
		nestedBrokerCommit: true,
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	err = runNativePackageTransaction(ctx, logger, transaction)
	if err == nil {
		return nativePackageBrokerCommitResult{
			success: true, changed: transaction.nestedMutationStarted,
			rollback: "not-needed", exitCode: 0,
		}, nil
	}
	if !transaction.nestedMutationStarted {
		return nativePackageBrokerPreflightFailure(err)
	}
	if transaction.nestedRollbackSucceeded {
		return nativePackageBrokerCommitResult{
			changed: true, rollback: "succeeded", exitCode: 1,
		}, err
	}
	return nativePackageBrokerCommitResult{
		changed: true, rollback: "failed", exitCode: 3,
	}, err
}

func (t *windowsNativePackageTransaction) Preflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.nestedBrokerCommit {
		return t.preflightNestedBrokerCommit()
	}
	mutexBudget := nativePackageTransactionTimeout
	if deadline, ok := ctx.Deadline(); ok {
		mutexBudget = time.Until(deadline)
		if mutexBudget <= 0 {
			return context.DeadlineExceeded
		}
	}
	release, err := acquireNamedNativePackageMutex(nativePackageMutexName, mutexBudget)
	if err != nil {
		return err
	}
	t.releaseMutex = release
	if _, err := validateNativeInstallingUserSID(t.request.targetUserSID); err != nil {
		return fmt.Errorf("validate target user SID: %w", err)
	}
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return fmt.Errorf("resolve Program Files known folder: %w", err)
	}
	t.programFiles = filepath.Clean(programFiles)
	t.parent = filepath.Join(t.programFiles, "VIIPER")
	t.destination = filepath.Join(t.parent, "viiper.exe")
	if _, err := nativeServiceExecutableParent(t.programFiles, t.destination); err != nil {
		return err
	}
	programFilesHandle, err := openNativePathWithoutReparse(
		t.programFiles, windows.FILE_READ_ATTRIBUTES, true,
	)
	if err != nil {
		return fmt.Errorf("lock Program Files root: %w", err)
	}
	t.inputHandles = append(t.inputHandles, programFilesHandle)
	for _, input := range []struct {
		name      string
		directory string
	}{
		{name: "broker source", directory: filepath.Dir(t.request.brokerSource)},
		{name: "driver helper", directory: filepath.Dir(t.request.driverHelper)},
		{name: "submission manifest", directory: filepath.Dir(t.request.submissionManifest)},
		{name: "signed driver package", directory: t.request.packageDirectory},
	} {
		handles, lockErr := lockNativePackageDirectoryChain(input.directory)
		if lockErr != nil {
			return fmt.Errorf("lock %s directory chain: %w", input.name, lockErr)
		}
		t.inputHandles = append(t.inputHandles, handles...)
	}

	t.sourceHandle, err = t.lockAndVerifyInput(
		t.request.brokerSource, t.request.expectedBrokerSHA256, true,
	)
	if err != nil {
		return fmt.Errorf("verify installer-bound VIIPER broker: %w", err)
	}
	t.helperHandle, err = t.lockAndVerifyInput(
		t.request.driverHelper, t.request.expectedHelperSHA256, true,
	)
	if err != nil {
		return fmt.Errorf("verify installer-bound driver helper: %w", err)
	}
	entries, err := os.ReadDir(t.request.packageDirectory)
	if err != nil {
		return fmt.Errorf("enumerate signed driver package: %w", err)
	}
	if len(entries) != len(nativePackageDriverFiles) {
		return fmt.Errorf("signed runtime driver package must contain exactly INF, SYS, and CAT, found %d files", len(entries))
	}
	for _, expected := range nativePackageDriverFiles {
		matches := 0
		for _, entry := range entries {
			if entry.Name() == expected && entry.Type().IsRegular() {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("signed driver package must contain one case-exact regular %s", expected)
		}
		expectedHash := map[string]string{
			"ViiperUde.inf": t.request.expectedInfSHA256,
			"ViiperUde.sys": t.request.expectedSysSHA256,
			"ViiperUde.cat": t.request.expectedCatSHA256,
		}[expected]
		handle, lockErr := t.lockAndVerifyInput(
			filepath.Join(t.request.packageDirectory, expected), expectedHash, false,
		)
		if lockErr != nil {
			return fmt.Errorf("verify installer-bound signed driver file %s: %w", expected, lockErr)
		}
		_ = handle
	}
	manifestHandle, err := t.lockAndVerifyInput(
		t.request.submissionManifest, t.request.expectedManifestSHA256, false,
	)
	if err != nil {
		return fmt.Errorf("verify installer-bound driver manifest: %w", err)
	}
	_ = manifestHandle

	if attributes, attrErr := nativePathAttributes(t.parent); attrErr == nil {
		if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
			attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.New("managed VIIPER directory is not a regular non-reparse directory")
		}
		parent, openErr := openNativePathWithoutReparse(
			t.parent, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, true,
		)
		if openErr != nil {
			return fmt.Errorf("open managed VIIPER directory: %w", openErr)
		}
		defer windows.CloseHandle(parent) //nolint:errcheck
		if validateErr := validateNativeSecurityDescriptor(parent, nativeBrokerDirectorySDDL); validateErr != nil {
			return fmt.Errorf("managed VIIPER directory is not installer-owned: %w", validateErr)
		}
	} else if !errors.Is(attrErr, windows.ERROR_FILE_NOT_FOUND) &&
		!errors.Is(attrErr, windows.ERROR_PATH_NOT_FOUND) {
		return fmt.Errorf("inspect managed VIIPER directory: %w", attrErr)
	}
	return nil
}

func (t *windowsNativePackageTransaction) preflightNestedBrokerCommit() error {
	t.nestedServiceRollbackSettled = true
	if _, err := validateNativeInstallingUserSID(t.request.targetUserSID); err != nil {
		return fmt.Errorf("validate nested broker target SID: %w", err)
	}
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return fmt.Errorf("resolve Program Files known folder: %w", err)
	}
	t.programFiles = filepath.Clean(programFiles)
	t.parent = filepath.Join(t.programFiles, "VIIPER")
	t.destination = filepath.Join(t.parent, "viiper.exe")
	if _, err := nativeServiceExecutableParent(t.programFiles, t.destination); err != nil {
		return err
	}
	programFilesHandle, err := openNativePathWithoutReparse(
		t.programFiles, windows.FILE_READ_ATTRIBUTES, true,
	)
	if err != nil {
		return fmt.Errorf("lock Program Files root: %w", err)
	}
	t.inputHandles = append(t.inputHandles, programFilesHandle)
	handles, err := lockNativePackageDirectoryChain(filepath.Dir(t.request.brokerSource))
	if err != nil {
		return fmt.Errorf("lock nested broker source directory chain: %w", err)
	}
	t.inputHandles = append(t.inputHandles, handles...)
	t.sourceHandle, err = t.lockAndVerifyInput(
		t.request.brokerSource, t.request.expectedBrokerSHA256, true,
	)
	if err != nil {
		return fmt.Errorf("verify installer-bound nested VIIPER broker: %w", err)
	}
	return nil
}

func (t *windowsNativePackageTransaction) InspectService(
	ctx context.Context,
) (nativePackageServiceSnapshot, error) {
	if !t.nestedBrokerCommit {
		t.serviceSnapshot = nativePackageServiceSnapshot{disposition: nativePackageServiceAbsent}
		return t.serviceSnapshot, nil
	}
	budget := nativePackageTransactionTimeout
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
		if budget <= 0 {
			return nativePackageServiceSnapshot{}, context.DeadlineExceeded
		}
	}
	release, err := acquireNativeInstallMutex(budget)
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("lock nested native broker transaction: %w", err)
	}
	t.releaseServiceMutex = release
	manager, err := mgr.Connect()
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("connect to SCM: %w", err)
	}
	t.manager = &windowsNativeSCM{manager: manager}
	service, err := t.manager.OpenService(NativeBrokerServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		t.serviceSnapshot = nativePackageServiceSnapshot{disposition: nativePackageServiceAbsent}
		return t.finalizeServiceInspection(ctx, t.serviceSnapshot)
	}
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("open %s: %w", NativeBrokerServiceName, err)
	}
	t.service = service
	config, err := service.Config()
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("query %s config: %w", NativeBrokerServiceName, err)
	}
	priorExecutable, err := nativeServiceExecutableFromCommandLine(config.BinaryPathName)
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("parse %s executable: %w", NativeBrokerServiceName, err)
	}
	if _, err := nativeServiceExecutableParent(t.programFiles, priorExecutable); err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf(
			"refusing to delete or adopt non-owned %s: %w", NativeBrokerServiceName, err,
		)
	}
	t.priorServiceExecutable = priorExecutable
	status, err := service.Query()
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("query %s state: %w", NativeBrokerServiceName, err)
	}
	status, err = settleNativeServiceSnapshot(ctx, service, status, waitContext)
	if err != nil {
		return nativePackageServiceSnapshot{}, err
	}
	securityDescriptor, err := service.SecurityDescriptor()
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("query %s DACL: %w", NativeBrokerServiceName, err)
	}
	keyPath, err := nativeServiceKeyFilePath()
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("resolve native broker credential: %w", err)
	}
	expectedConfig, _, err := nativeBrokerServiceConfiguration(priorExecutable, keyPath)
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("construct canonical native broker service: %w", err)
	}
	recovery, err := service.RecoveryActions()
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("query %s recovery actions: %w",
			NativeBrokerServiceName, err)
	}
	reset, err := service.ResetPeriod()
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("query %s recovery reset: %w",
			NativeBrokerServiceName, err)
	}
	nonCrash, err := service.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("query %s recovery mode: %w",
			NativeBrokerServiceName, err)
	}
	canonical := isCanonicalNativePackageService(
		config, expectedConfig, securityDescriptor, recovery, reset, nonCrash,
	)
	disposition := nativePackageServiceWeakExactOwned
	if canonical {
		releaseExecutable, lockErr := lockNativePriorServiceExecutable(priorExecutable)
		if lockErr == nil {
			disposition = nativePackageServiceTrusted
			t.priorExecutableRelease = releaseExecutable
		} else {
			// An exact service name/path with weak image ACLs is stale package
			// ownership, not a trustworthy rollback source. It is removed and
			// recreated; never "repair" its ACL while old handles may exist.
			t.logger.Warn("Replacing weak exact-owned native broker service image",
				"path", priorExecutable, "error", lockErr)
		}
	}
	t.serviceSnapshot = nativePackageServiceSnapshot{
		disposition: disposition,
		wasRunning:  status.State == svc.Running,
	}
	return t.finalizeServiceInspection(ctx, t.serviceSnapshot)
}

func (t *windowsNativePackageTransaction) finalizeServiceInspection(
	ctx context.Context,
	snapshot nativePackageServiceSnapshot,
) (nativePackageServiceSnapshot, error) {
	if snapshot.disposition == nativePackageServiceTrusted && snapshot.wasRunning {
		healthy, err := t.verifyExactBrokerHealth(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nativePackageServiceSnapshot{}, ctx.Err()
			}
			t.logger.Info("Exact native broker requires transactional repair", "reason", err)
		} else {
			t.nestedBrokerHealthy = healthy
		}
	}
	t.serviceSnapshot = snapshot
	return snapshot, nil
}

func (t *windowsNativePackageTransaction) verifyExactBrokerHealth(ctx context.Context) (bool, error) {
	if t.service == nil || !strings.EqualFold(t.priorServiceExecutable, t.destination) {
		return false, errors.New("native broker service does not use the canonical package executable")
	}
	handle, err := lockNativePackageInput(t.priorServiceExecutable)
	if err != nil {
		return false, fmt.Errorf("lock exact native broker image: %w", err)
	}
	hash, hashErr := hashNativePackageHandle(handle)
	closeErr := windows.CloseHandle(handle)
	if hashErr != nil {
		return false, fmt.Errorf("hash exact native broker image: %w", hashErr)
	}
	if closeErr != nil {
		return false, fmt.Errorf("close exact native broker image: %w", closeErr)
	}
	if !strings.EqualFold(hash, t.request.expectedBrokerSHA256) {
		return false, fmt.Errorf("native broker SHA-256=%s expected=%s", hash, t.request.expectedBrokerSHA256)
	}

	credential, err := readNativeCredentialReadOnly(t.request.targetUserSID)
	if err != nil {
		return false, fmt.Errorf("read protected native broker credential: %w", err)
	}
	legacy, err := snapshotNativeLegacyStartup(ctx, t.request.targetUserSID)
	if err != nil {
		return false, fmt.Errorf("inspect legacy native broker ownership: %w", err)
	}
	if legacy.release != nil {
		defer legacy.release()
	}
	if nativeLegacyStartupOwnsRuntime(legacy) {
		return false, errors.New("active legacy VIIPER startup ownership is still registered")
	}

	servicePID, err := requireNativeServiceProcess(t.service, 0)
	if err != nil {
		return false, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := verifyNativeBrokerOnce(probeCtx, strings.TrimSpace(string(credential))); err != nil {
		return false, err
	}
	if _, err := requireNativeServiceProcess(t.service, servicePID); err != nil {
		return false, fmt.Errorf("revalidate exact native broker after authenticated ping: %w", err)
	}
	return true, nil
}

func isCanonicalNativePackageService(
	actual, expected mgr.Config,
	securityDescriptor string,
	recovery []mgr.RecoveryAction,
	reset uint32,
	nonCrash bool,
) bool {
	return compareNativeSecurityDescriptorStrings(
		securityDescriptor, nativeBrokerServiceSDDL,
	) == nil && nativeServiceConfigsEqual(actual, expected) &&
		slices.Equal(recovery, nativeServiceRecoveryActions) &&
		reset == nativeServiceRecoveryResetSecond && nonCrash
}

func (t *windowsNativePackageTransaction) Prepare(
	ctx context.Context,
	snapshot nativePackageServiceSnapshot,
) error {
	if snapshot.disposition != t.serviceSnapshot.disposition ||
		snapshot.wasRunning != t.serviceSnapshot.wasRunning {
		return errors.New("native service snapshot changed before preparation")
	}
	if !t.nestedBrokerCommit {
		return t.preparePackageCoordination()
	}
	if t.nestedBrokerHealthy {
		return nil
	}
	// From this point onward the nested callback may stop/delete SCM state or
	// publish the canonical broker image. Any failure must prove rollback before
	// the still-running helper may touch its captured driver snapshot again.
	t.nestedMutationStarted = true
	if t.service != nil && snapshot.disposition == nativePackageServiceWeakExactOwned {
		if snapshot.wasRunning {
			if err := stopNativeService(ctx, t.service, waitContext); err != nil {
				return fmt.Errorf("stop weak exact-owned %s: %w", NativeBrokerServiceName, err)
			}
		}
		if err := t.service.Delete(); err != nil &&
			!errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return fmt.Errorf("delete weak exact-owned %s: %w", NativeBrokerServiceName, err)
		}
		t.service.Close() //nolint:errcheck
		t.service = nil
		if err := waitForNativePackageServiceDeletion(ctx, t.manager); err != nil {
			return err
		}
	}
	if t.service != nil && snapshot.disposition == nativePackageServiceTrusted &&
		strings.EqualFold(t.priorServiceExecutable, t.destination) {
		if snapshot.wasRunning {
			// STOP is itself the mutation. Arm reconciliation before sending it so
			// a timeout while StopPending cannot strand a formerly-running service.
			t.stoppedTrustedService = true
			if err := stopNativeService(ctx, t.service, waitContext); err != nil {
				return fmt.Errorf("quiesce trusted %s for atomic image replacement: %w",
					NativeBrokerServiceName, err)
			}
		}
		// The read-only preflight lock deliberately denies rename/delete. Once
		// the exact trusted service is quiescent, release that lock so the
		// protected image can move to the rollback name in the same directory.
		if t.priorExecutableRelease != nil {
			t.priorExecutableRelease()
			t.priorExecutableRelease = nil
		}
	}
	return t.stageBrokerExecutable()
}

func (t *windowsNativePackageTransaction) InstallDriverAndBroker(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.nestedBrokerCommit {
		if t.releaseServiceMutex == nil {
			return errors.New("nested native broker transaction does not hold the service mutex")
		}
		if !t.nestedBrokerHealthy {
			var evidence nativeBrokerInstallEvidence
			if err := installNativeBrokerTransactionWithEvidence(
				ctx, t.logger, t.destination,
				productionNativeInstallDependencies(t.request.targetUserSID),
				&evidence,
			); err != nil {
				t.nestedServiceRollbackSettled =
					!evidence.mutationStarted || evidence.rollbackSucceeded
				return fmt.Errorf("repair native broker transaction: %w", err)
			}
		}
	} else {
		if t.releaseServiceMutex != nil {
			return errors.New("outer native package transaction unexpectedly holds the service mutex")
		}
		if err := t.runDriverHelper(ctx); err != nil {
			return err
		}
	}
	// A deadline that expires after the synchronous mutating helper starts must
	// not turn its authenticated success into a contradictory outer rollback.
	// The helper owns the driver snapshot and the nested broker owns its bounded
	// SCM rollback; wait for that authoritative result, then commit its proof.
	t.installProof = true
	return nil
}

func (t *windowsNativePackageTransaction) VerifyAuthenticatedHealth(ctx context.Context) error {
	if t.nestedBrokerCommit && t.nestedBrokerHealthy {
		healthy, err := t.verifyExactBrokerHealth(ctx)
		if err != nil {
			return fmt.Errorf("reverify exact native package no-op: %w", err)
		}
		if !healthy {
			return errors.New("exact native package lost authenticated health before no-op commit")
		}
	}
	// ViiperUdeCtl does not return success until the staged broker's native
	// service transaction has performed authenticated ABI/capability health,
	// removed legacy ownership, and authenticated a second time. Preserve that
	// proof rather than adding a racy third ping after the inner commit.
	if !t.installProof {
		return errors.New("driver helper returned no authenticated broker health proof")
	}
	// The nested broker accepts this proof only when its authenticated health
	// commit completed under the exact outer deadline. A scheduler delay between
	// child exit and this check must not trigger a contradictory driver rollback.
	if err := ctx.Err(); err != nil {
		t.logger.Warn("Native package proof completed at the transaction deadline; finishing outer cleanup",
			"deadline", err)
	}
	return nil
}

func (t *windowsNativePackageTransaction) Commit(context.Context) error {
	if t.destinationRelease != nil {
		t.destinationRelease()
		t.destinationRelease = nil
	}
	if err := t.releaseCoordinationToken(); err != nil {
		// The nested broker transaction has already authenticated the native
		// service and removed legacy ownership. A stale token is inert without
		// the outer package mutex, so retain it for repair instead of turning a
		// committed installation into an unsafe rollback.
		t.logger.Warn("Could not remove protected package transaction token after commit",
			"path", t.tokenPath, "error", err)
	}
	if t.backupPath != "" {
		if err := deleteNativePackageFile(t.backupPath); err != nil {
			// Cleanup cannot invalidate an already-authenticated inner transaction.
			// Keep the administrator-only backup for the next repair instead.
			t.logger.Warn("Could not remove protected prior broker backup after commit",
				"path", t.backupPath, "error", err)
		} else {
			t.backupPath = ""
		}
	}
	return nil
}

func (t *windowsNativePackageTransaction) Rollback(ctx context.Context) (resultErr error) {
	defer func() {
		if t.nestedBrokerCommit && t.nestedMutationStarted && resultErr == nil &&
			t.nestedServiceRollbackSettled {
			t.nestedRollbackSucceeded = true
		}
	}()
	var rollbackErrors []error
	if t.destinationRelease != nil {
		t.destinationRelease()
		t.destinationRelease = nil
	}
	if err := t.releaseCoordinationToken(); err != nil {
		rollbackErrors = append(rollbackErrors,
			fmt.Errorf("remove package transaction token: %w", err))
	}
	if t.nestedBrokerCommit && t.nestedMutationStarted &&
		!t.nestedServiceRollbackSettled {
		// The inner SCM transaction deliberately leaves an indeterminate service
		// stopped. Do not delete/replace the image it may still reference, restore
		// a prior image under an indeterminate configuration, or restart it. Keep
		// both protected images for explicit external reconciliation.
		rollbackErrors = append(rollbackErrors, errors.New(
			"nested native broker service rollback is unsettled; retaining staged and prior broker images and leaving the service stopped for external reconciliation"))
		return errors.Join(rollbackErrors...)
	}
	restored := true
	if err := t.restoreBrokerExecutable(); err != nil {
		restored = false
		rollbackErrors = append(rollbackErrors, err)
	}
	if t.stoppedTrustedService && t.service != nil && t.serviceSnapshot.wasRunning {
		if !restored {
			rollbackErrors = append(rollbackErrors,
				errors.New("refusing to restart prior native broker because its image was not restored"))
			return errors.Join(rollbackErrors...)
		}
		release, err := lockNativePriorServiceExecutable(t.priorServiceExecutable)
		if err != nil {
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("revalidate restored native broker before restart: %w", err))
			return errors.Join(rollbackErrors...)
		}
		defer release()
		if err := reconcileNativePackageServiceRunning(ctx, t.service); err != nil {
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("restore prior trusted %s run state: %w", NativeBrokerServiceName, err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func (t *windowsNativePackageTransaction) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	if t.destinationRelease != nil {
		t.destinationRelease()
		t.destinationRelease = nil
	}
	if t.priorExecutableRelease != nil {
		t.priorExecutableRelease()
		t.priorExecutableRelease = nil
	}
	if t.tokenHandle != 0 {
		windows.CloseHandle(t.tokenHandle) //nolint:errcheck
		t.tokenHandle = 0
	}
	if t.service != nil {
		t.service.Close() //nolint:errcheck
	}
	if t.manager != nil {
		t.manager.Close() //nolint:errcheck
	}
	if t.parentHandle != 0 {
		windows.CloseHandle(t.parentHandle) //nolint:errcheck
	}
	for index := len(t.inputHandles) - 1; index >= 0; index-- {
		windows.CloseHandle(t.inputHandles[index]) //nolint:errcheck
	}
	if t.releaseServiceMutex != nil {
		t.releaseServiceMutex()
		t.releaseServiceMutex = nil
	}
	if t.releaseMutex != nil {
		t.releaseMutex()
		t.releaseMutex = nil
	}
	return nil
}

func (t *windowsNativePackageTransaction) releaseCoordinationToken() error {
	if t.tokenHandle != 0 {
		if err := windows.CloseHandle(t.tokenHandle); err != nil {
			return fmt.Errorf("close protected package transaction token: %w", err)
		}
		t.tokenHandle = 0
	}
	if t.tokenPath == "" {
		return nil
	}
	path := t.tokenPath
	if err := deleteNativePackageFile(path); err != nil &&
		!errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return err
	}
	t.tokenPath = ""
	t.tokenSHA256 = ""
	return nil
}

func (t *windowsNativePackageTransaction) lockAndVerifyInput(
	path, expectedHash string,
	requirePE bool,
) (windows.Handle, error) {
	handle, err := lockNativePackageInput(path)
	if err != nil {
		return 0, err
	}
	hash, err := hashNativePackageHandle(handle)
	if err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, err
	}
	if !strings.EqualFold(hash, expectedHash) {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, fmt.Errorf("SHA-256=%s expected=%s", hash, expectedHash)
	}
	if requirePE {
		if err := requireNativePackagePE(handle); err != nil {
			windows.CloseHandle(handle) //nolint:errcheck
			return 0, err
		}
	}
	t.inputHandles = append(t.inputHandles, handle)
	return handle, nil
}

func (t *windowsNativePackageTransaction) runDriverHelper(ctx context.Context) error {
	text, err := t.executeDriverHelper(ctx)
	processExitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return fmt.Errorf("wait for native driver helper: %w: %s", err, text)
		}
		processExitCode = exitError.ExitCode()
	}
	proof, proofErr := parseNativePackageInstallProof(text, processExitCode)
	if proofErr != nil {
		return fmt.Errorf("validate native driver helper proof: %w: %s", proofErr, text)
	}
	if proof.exitCode == nativePackageRebootRequiredCode {
		return &nativePackageRebootRequiredError{cause: fmt.Errorf("%w: %s", err, text)}
	}
	if !proof.success {
		return fmt.Errorf("native driver helper failed with exit %d: %w: %s",
			proof.exitCode, err, text)
	}
	return nil
}

func (t *windowsNativePackageTransaction) executeDriverHelper(ctx context.Context) (string, error) {
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) {
		return "", context.DeadlineExceeded
	}
	arguments := []string{
		"install", filepath.Join(t.request.packageDirectory, "ViiperUde.inf"),
		"--manifest", t.request.submissionManifest,
		"--manifest-sha256", t.request.expectedManifestSHA256,
		"--source-revision", t.request.sourceRevision,
		"--validation-mode", t.request.driverValidationMode,
		"--expected-inf-sha256", t.request.expectedInfSHA256,
		"--expected-sys-sha256", t.request.expectedSysSHA256,
		"--expected-cat-sha256", t.request.expectedCatSHA256,
		"--transaction-deadline-unix-ms", strconv.FormatInt(deadline.UnixMilli(), 10),
		"--broker-executable", t.request.brokerSource,
		"--broker-sha256", t.request.expectedBrokerSHA256,
		"--broker-token", t.tokenPath,
		"--broker-token-sha256", t.tokenSHA256,
		"--target-user-sid", t.request.targetUserSID,
	}
	// Do not use CommandContext: killing ViiperUdeCtl could interrupt its in-memory
	// DriverStore rollback or the broker's deferred SCM/credential rollback.
	command := exec.Command(t.request.driverHelper, arguments...)
	command.Dir = filepath.Dir(t.request.driverHelper)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return "", err
	}
	// The helper owns the driver snapshot and nested broker rollback. Its
	// propagated absolute deadline is cooperative; never terminate it here.
	err := waitNativePackageHelper(command)
	text := strings.TrimSpace(output.String())
	return text, err
}

func reconcileNativePackageServiceRunning(ctx context.Context, service nativeManagedService) error {
	for {
		status, err := service.Query()
		if err != nil {
			return err
		}
		switch status.State {
		case svc.Running:
			return nil
		case svc.Stopped:
			if err := service.Start(); err != nil {
				return err
			}
		case svc.StartPending, svc.StopPending:
			// Reconcile the partial forward STOP before deciding whether START is
			// required. Both paths remain bounded by the rollback-only context.
		default:
			return fmt.Errorf("unexpected service state %d during rollback", status.State)
		}
		if err := waitContext(ctx, nativeServiceStatePoll); err != nil {
			return err
		}
	}
}

func (t *windowsNativePackageTransaction) stageCoordinationToken() error {
	path, err := t.uniqueManagedPath("transaction")
	if err != nil {
		return err
	}
	path = strings.TrimSuffix(path, ".tmp") + ".token"
	content := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, content); err != nil {
		return fmt.Errorf("generate package transaction token: %w", err)
	}
	security, err := nativeSecurityAttributes(nativePackageTokenSDDL)
	if err != nil {
		return err
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ, security, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_FLAG_OPEN_REPARSE_POINT|
			windows.FILE_FLAG_WRITE_THROUGH, 0)
	if err != nil {
		return fmt.Errorf("create protected package transaction token: %w", err)
	}
	fail := func(failErr error) error {
		windows.CloseHandle(handle) //nolint:errcheck
		_ = deleteNativePackageFile(path)
		return failErr
	}
	var written uint32
	if err := windows.WriteFile(handle, content, &written, nil); err != nil {
		return fail(err)
	}
	if written != uint32(len(content)) {
		return fail(io.ErrShortWrite)
	}
	if err := windows.FlushFileBuffers(handle); err != nil {
		return fail(err)
	}
	if err := validateNativeSecurityDescriptor(handle, nativePackageTokenSDDL); err != nil {
		return fail(err)
	}
	if err := requireSingleNativeFileLink(handle); err != nil {
		return fail(err)
	}
	sum := sha256.Sum256(content)
	t.tokenPath = path
	t.tokenSHA256 = hex.EncodeToString(sum[:])
	t.tokenHandle = handle
	return nil
}

func (t *windowsNativePackageTransaction) ensureManagedPackageDirectory() error {
	if t.parentHandle != 0 {
		return nil
	}
	if attributes, err := nativePathAttributes(t.parent); err != nil {
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
			!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return err
		}
		security, securityErr := nativeSecurityAttributes(nativeBrokerDirectorySDDL)
		if securityErr != nil {
			return securityErr
		}
		parentPointer, pointerErr := windows.UTF16PtrFromString(t.parent)
		if pointerErr != nil {
			return pointerErr
		}
		if createErr := windows.CreateDirectory(parentPointer, security); createErr != nil {
			return fmt.Errorf("atomically create protected VIIPER directory: %w", createErr)
		}
		t.parentMade = true
	} else if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("managed VIIPER path is not a regular directory")
	}
	parent, err := openNativePathWithoutReparse(
		t.parent, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, true,
	)
	if err != nil {
		return fmt.Errorf("lock protected VIIPER directory: %w", err)
	}
	t.parentHandle = parent
	if err := validateNativeSecurityDescriptor(parent, nativeBrokerDirectorySDDL); err != nil {
		return fmt.Errorf("validate protected VIIPER directory: %w", err)
	}
	return nil
}

func (t *windowsNativePackageTransaction) preparePackageCoordination() error {
	if t.nestedBrokerCommit {
		return errors.New("nested broker transaction cannot create the outer coordination token")
	}
	if err := t.ensureManagedPackageDirectory(); err != nil {
		return err
	}
	if t.tokenPath != "" || t.tokenHandle != 0 {
		return errors.New("native package coordination token is already staged")
	}
	if err := t.stageCoordinationToken(); err != nil {
		return err
	}
	return nil
}

func (t *windowsNativePackageTransaction) stageBrokerExecutable() error {
	if !t.nestedBrokerCommit {
		return errors.New("broker image staging is owned by the nested service transaction")
	}
	if err := t.ensureManagedPackageDirectory(); err != nil {
		return err
	}
	var err error
	if existing, openErr := openNativePathWithoutReparse(
		t.destination, windows.GENERIC_READ|windows.READ_CONTROL, false,
	); openErr == nil {
		if err := requireSingleNativeFileLink(existing); err != nil {
			windows.CloseHandle(existing) //nolint:errcheck
			return err
		}
		if err := validateNativeSecurityDescriptor(existing, nativeBrokerExecutableSDDL); err != nil {
			windows.CloseHandle(existing) //nolint:errcheck
			return fmt.Errorf("existing broker is not installer-owned: %w", err)
		}
		existingHash, hashErr := hashNativePackageHandle(existing)
		windows.CloseHandle(existing) //nolint:errcheck
		if hashErr != nil {
			return hashErr
		}
		if strings.EqualFold(existingHash, t.request.expectedBrokerSHA256) {
			release, err := lockNativeServiceExecutableReadOnly(t.destination)
			if err != nil {
				return err
			}
			t.destinationRelease = release
			return nil
		}
		backupPath, err := t.uniqueManagedPath("rollback")
		if err != nil {
			return err
		}
		if err := moveNativePackageFile(t.destination, backupPath, false); err != nil {
			return fmt.Errorf("retain prior broker for rollback: %w", err)
		}
		// Publish rollback ownership only after the atomic rename succeeds. On a
		// failed rename the canonical prior image is still in place and may be
		// safely revalidated/restarted by Rollback.
		t.backupPath = backupPath
	} else if !errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) &&
		!errors.Is(openErr, windows.ERROR_PATH_NOT_FOUND) {
		return fmt.Errorf("inspect existing broker: %w", openErr)
	}

	t.temporaryPath, err = t.uniqueManagedPath("staging")
	if err != nil {
		return err
	}
	if err := copyNativePackageHandleAtomically(
		t.sourceHandle, t.temporaryPath, t.request.expectedBrokerSHA256,
	); err != nil {
		return err
	}
	if err := moveNativePackageFile(t.temporaryPath, t.destination, false); err != nil {
		return fmt.Errorf("publish staged broker: %w", err)
	}
	t.temporaryPath = ""
	t.destinationPublished = true
	release, err := lockNativeServiceExecutableReadOnly(t.destination)
	if err != nil {
		return fmt.Errorf("verify published protected broker: %w", err)
	}
	t.destinationRelease = release
	return nil
}

func (t *windowsNativePackageTransaction) restoreBrokerExecutable() error {
	var restoreErrors []error
	if t.temporaryPath != "" {
		if err := deleteNativePackageFile(t.temporaryPath); err != nil &&
			!errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			restoreErrors = append(restoreErrors, fmt.Errorf("remove staged broker: %w", err))
		}
		t.temporaryPath = ""
	}
	if t.destinationPublished {
		handle, err := openNativePathWithoutReparse(t.destination, windows.GENERIC_READ, false)
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("lock rejected broker for rollback: %w", err))
		} else {
			hash, hashErr := hashNativePackageHandle(handle)
			windows.CloseHandle(handle) //nolint:errcheck
			if hashErr != nil || !strings.EqualFold(hash, t.request.expectedBrokerSHA256) {
				restoreErrors = append(restoreErrors,
					errors.New("refusing to remove broker that changed after protected staging"))
			} else if deleteErr := deleteNativePackageFile(t.destination); deleteErr != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("remove rejected broker: %w", deleteErr))
			}
		}
		t.destinationPublished = false
	}
	if t.backupPath != "" {
		if err := moveNativePackageFile(t.backupPath, t.destination, false); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore prior broker: %w", err))
		} else {
			t.backupPath = ""
		}
	}
	if t.parentMade {
		if t.parentHandle != 0 {
			windows.CloseHandle(t.parentHandle) //nolint:errcheck
			t.parentHandle = 0
		}
		pointer, err := windows.UTF16PtrFromString(t.parent)
		if err == nil {
			err = windows.RemoveDirectory(pointer)
		}
		if err != nil && !errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) {
			restoreErrors = append(restoreErrors, fmt.Errorf("remove created VIIPER directory: %w", err))
		}
		t.parentMade = false
	}
	return errors.Join(restoreErrors...)
}

func (t *windowsNativePackageTransaction) uniqueManagedPath(label string) (string, error) {
	var suffix [12]byte
	if _, err := io.ReadFull(rand.Reader, suffix[:]); err != nil {
		return "", err
	}
	return filepath.Join(t.parent, ".viiper."+label+"."+hex.EncodeToString(suffix[:])+".tmp"), nil
}

func acquireNamedNativePackageMutex(name string, timeout time.Duration) (func(), error) {
	return acquireNativeNamedMutex(
		name, timeout, "another VIIPER native package transaction is still running",
	)
}

// nativePackageMutexHeldByAnotherOwner proves that this short-lived broker
// commit is nested inside the signed outer package transaction. The helper is
// a separate process/thread, so acquiring the mutex here would deadlock; a
// zero-time wait must instead report WAIT_TIMEOUT. If the mutex is absent,
// abandoned, or acquirable, no authorized outer transaction exists.
func nativePackageMutexHeldByAnotherOwner(name string) (bool, error) {
	return nativeNamedMutexHeldByAnotherOwner(name)
}

func lockNativePackageInput(path string) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, err
	}
	info := nativeFileAttributeTagInfo{}
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, errors.New("input is not a regular non-reparse file")
	}
	if err := requireSingleNativeFileLink(handle); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, err
	}
	return handle, nil
}

// lockNativePackageDirectoryChain prevents path redirection after hashing.
// Holding only the final file does not stop an ancestor directory from being
// renamed and replaced before CreateProcess/SetupAPI reopens the same string.
func lockNativePackageDirectoryChain(directory string) ([]windows.Handle, error) {
	directory = filepath.Clean(directory)
	if !filepath.IsAbs(directory) || strings.IndexByte(directory, 0) >= 0 {
		return nil, fmt.Errorf("package input directory must be absolute and contain no NUL: %s", directory)
	}
	volume := filepath.VolumeName(directory)
	if len(volume) != 2 || volume[1] != ':' {
		return nil, fmt.Errorf("package input directory must use a local drive path: %s", directory)
	}
	root := volume + string(filepath.Separator)
	relative, err := filepath.Rel(root, directory)
	if err != nil || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("package input directory escaped its volume root: %s", directory)
	}
	paths := []string{root}
	current := root
	if relative != "." {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			if component == "" || component == "." || component == ".." {
				return nil, fmt.Errorf("package input directory has an unsafe component: %s", directory)
			}
			current = filepath.Join(current, component)
			paths = append(paths, current)
		}
	}
	handles := make([]windows.Handle, 0, len(paths))
	for _, path := range paths {
		handle, openErr := openNativePathWithoutReparse(
			path, windows.FILE_READ_ATTRIBUTES, true,
		)
		if openErr != nil {
			for index := len(handles) - 1; index >= 0; index-- {
				windows.CloseHandle(handles[index]) //nolint:errcheck
			}
			return nil, fmt.Errorf("open non-reparse ancestor %s: %w", path, openErr)
		}
		handles = append(handles, handle)
	}
	return handles, nil
}

func hashNativePackageHandle(handle windows.Handle) (string, error) {
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return "", err
	}
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		var read uint32
		if err := windows.ReadFile(handle, buffer, &read, nil); err != nil {
			return "", err
		}
		if read == 0 {
			break
		}
		_, _ = hash.Write(buffer[:read])
	}
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func requireNativePackagePE(handle windows.Handle) error {
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return err
	}
	header := make([]byte, 2)
	var read uint32
	if err := windows.ReadFile(handle, header, &read, nil); err != nil {
		return err
	}
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return err
	}
	if read != 2 || header[0] != 'M' || header[1] != 'Z' {
		return errors.New("file is not a Windows PE image")
	}
	return nil
}

func nativeSecurityAttributes(sddl string) (*windows.SecurityAttributes, error) {
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}, nil
}

func copyNativePackageHandleAtomically(
	source windows.Handle,
	destination, expectedHash string,
) (resultErr error) {
	security, err := nativeSecurityAttributes(nativeBrokerExecutableSDDL)
	if err != nil {
		return err
	}
	pointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	target, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ, security, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_WRITE_THROUGH, 0)
	if err != nil {
		return fmt.Errorf("create protected staged broker: %w", err)
	}
	defer func() {
		windows.CloseHandle(target) //nolint:errcheck
		if resultErr != nil {
			_ = deleteNativePackageFile(destination)
		}
	}()
	if _, err := windows.SetFilePointer(source, 0, nil, windows.FILE_BEGIN); err != nil {
		return err
	}
	buffer := make([]byte, 64*1024)
	for {
		var read uint32
		if err := windows.ReadFile(source, buffer, &read, nil); err != nil {
			return err
		}
		if read == 0 {
			break
		}
		var written uint32
		if err := windows.WriteFile(target, buffer[:read], &written, nil); err != nil {
			return err
		}
		if written != read {
			return io.ErrShortWrite
		}
	}
	if err := windows.FlushFileBuffers(target); err != nil {
		return err
	}
	if err := validateNativeSecurityDescriptor(target, nativeBrokerExecutableSDDL); err != nil {
		return err
	}
	if err := requireSingleNativeFileLink(target); err != nil {
		return err
	}
	hash, err := hashNativePackageHandle(target)
	if err != nil {
		return err
	}
	if !strings.EqualFold(hash, expectedHash) {
		return fmt.Errorf("staged broker SHA-256=%s expected=%s", hash, expectedHash)
	}
	return nil
}

func moveNativePackageFile(source, destination string, replace bool) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if replace {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	return windows.MoveFileEx(from, to, flags)
}

func deleteNativePackageFile(path string) error {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.DeleteFile(pointer)
}

func nativePathAttributes(path string) (uint32, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.GetFileAttributes(pointer)
}

func waitForNativePackageServiceDeletion(ctx context.Context, manager nativeSCM) error {
	for {
		service, err := manager.OpenService(NativeBrokerServiceName)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		if err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return err
		}
		if service != nil {
			service.Close() //nolint:errcheck
		}
		if err := waitContext(ctx, nativeServiceStatePoll); err != nil {
			return fmt.Errorf("wait for weak %s deletion: %w", NativeBrokerServiceName, err)
		}
	}
}
