//go:build windows

package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- used only to reject Windows thumbprint collisions.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const nativePackageMutexName = "VIIPER.NativePackage.Install.v1"
const nativePackageTokenSDDL = "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"
const nativePackageLocalTestTrustCapabilitySchema = "viiper.native.local-test-trust-capability/v1"
const nativePackageLocalTestTrustCapabilityName = "local-test-trust-capability.json"
const nativePackageLocalTestTrustCapabilityMaximumBytes = 4096
const nativePackageLocalTestTrustOwnershipSchema = "viiper.native.local-test-trust-ownership/v1"
const nativePackageLocalTrustClearedName = "local-test-trust-cleared-v1.json"

var nativePackageDriverFiles = []string{
	"ViiperUde.inf", "ViiperUde.sys", "ViiperUde.cat",
}

type windowsNativePackageTransaction struct {
	logger             *slog.Logger
	request            nativePackageRequest
	nestedBrokerCommit bool

	releaseMutex                 func()
	releaseServiceMutex          func()
	releaseTrustLease            func() error
	trustLeaseDirectoryHandles   []windows.Handle
	inputHandles                 []windows.Handle
	sourceHandle                 windows.Handle
	helperHandle                 windows.Handle
	nestedBrokerHealthy          bool
	nestedMutationStarted        bool
	nestedRollbackSucceeded      bool
	nestedServiceRollbackSettled bool
	driverQuiesceRequested       bool
	driverBrokerHandoff          bool
	driverHelperSettled          bool
	driverCoordinationErr        error
	driverInstallProof           nativePackageInstallProof
	driverInstallProofPresent    bool
	pendingBrokerOuterSettlement bool
	replayedBrokerRecovery       bool

	programFiles string
	destination  string
	parent       string
	parentHandle windows.Handle
	parentMade   bool

	manager                nativeSCM
	service                nativeManagedService
	serviceSnapshot        nativePackageServiceSnapshot
	priorServiceExecutable string
	priorExecutableSHA256  string
	priorServiceConfig     mgr.Config
	priorServiceDACL       string
	priorServiceRecovery   []mgr.RecoveryAction
	priorServiceReset      uint32
	priorServiceNonCrash   bool
	priorExecutableRelease func()
	stoppedTrustedService  bool
	weakServiceMutation    bool
	weakServiceRemoved     bool

	temporaryPath         string
	backupPath            string
	destinationPublished  bool
	destinationRelease    func()
	tokenPath             string
	tokenSHA256           string
	tokenHandle           windows.Handle
	boundOuterTokenPath   string
	installProof          bool
	brokerJournal         *nativeBrokerJournal
	brokerJournalProof    nativeBrokerJournalProof
	brokerJournalCutpoint func(string) error
	closed                bool

	localTestCertificateHandle windows.Handle
	localTestCertificateDER    []byte
	localTestTrust             *nativePackageLocalTestTrustState
	localTestTrustCutpoint     func(string) error
}

type nativePackageLocalTestTrustCapability struct {
	Schema                 string `json:"schema"`
	Nonce                  string `json:"nonce"`
	ParentPID              uint32 `json:"parentPid"`
	ParentCreationFileTime uint64 `json:"parentCreationFileTime"`
	SourceRevision         string `json:"sourceRevision"`
	CertificatePath        string `json:"certificatePath"`
	CertificateSHA256      string `json:"certificateSha256"`
	PackageLockSHA256      string `json:"packageLockSha256"`
	TrustJournalSchema     string `json:"trustJournalSchema"`
	TrustJournalDirectory  string `json:"trustJournalDirectory"`
}

type nativePackageLocalTestTrustOwnership struct {
	Schema                   string `json:"schema"`
	SourceRevision           string `json:"sourceRevision"`
	CertificateSHA256        string `json:"certificateSha256"`
	PackageLockSHA256        string `json:"packageLockSha256"`
	BaselineRoot             int    `json:"baselineRoot"`
	BaselineTrustedPublisher int    `json:"baselineTrustedPublisher"`
}

type nativePackageLocalTestTrustPaths struct {
	directory    string
	preparing    string
	pending      string
	owned        string
	uninstalling string
	cleared      string
}

type nativePackageLocalTestTrustState struct {
	paths          nativePackageLocalTestTrustPaths
	record         nativePackageLocalTestTrustOwnership
	bytes          []byte
	certificateDER []byte
	rootStore      windows.Handle
	publisherStore windows.Handle
	state          string
	createdCurrent bool
	resumed        bool
	alreadyOwned   bool
}

func executeNativePackageLocalTestTrustStep(
	name string,
	cutBefore bool,
	operation func() error,
	cutpoint func(string) error,
) error {
	if operation == nil {
		return errors.New("local-test trust durable step has no operation")
	}
	if cutBefore && cutpoint != nil {
		if err := cutpoint(name); err != nil {
			return err
		}
	}
	if err := operation(); err != nil {
		return err
	}
	if !cutBefore && cutpoint != nil {
		if err := cutpoint(name); err != nil {
			return err
		}
	}
	return nil
}

func installNativePackage(
	ctx context.Context,
	logger *slog.Logger,
	request nativePackageRequest,
) error {
	transaction := &windowsNativePackageTransaction{logger: logger, request: request}
	if err := runNativePackageTransaction(ctx, logger, transaction); err != nil {
		return err
	}
	if transaction.replayedBrokerRecovery {
		return &nativePackageRecoveryRetryError{}
	}
	return nil
}

func commitNativePackageBroker(
	logger *slog.Logger,
	tokenPath, expectedTokenSHA256, expectedBrokerSHA256, targetUserSID, deadlineUnixMS string,
	recoveryOnly bool,
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
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	replayBudget := time.Until(deadline)
	releaseReplayMutex, err := acquireNativeInstallMutex(replayBudget)
	if err != nil {
		_, active, pathErr := nativeBrokerJournalPaths(targetUserSID)
		if pathErr == nil {
			if _, attributeErr := nativePathAttributes(active); attributeErr == nil ||
				(!errors.Is(attributeErr, windows.ERROR_FILE_NOT_FOUND) &&
					!errors.Is(attributeErr, windows.ERROR_PATH_NOT_FOUND)) {
				return nativePackageBrokerCommitResult{
					changed: true, rollback: "failed", exitCode: 3,
				}, fmt.Errorf("acquire service transaction mutex with an active broker journal: %w", err)
			}
		}
		return preflightFailure(fmt.Errorf("acquire service transaction mutex for broker proof replay: %w", err))
	}
	replayProof, replayed, activeJournal, replayErr := replayNativeBrokerNestedReadyProof(
		ctx, logger, targetUserSID, tokenPath, expectedTokenSHA256, expectedBrokerSHA256,
	)
	releaseReplayMutex()
	if replayErr != nil {
		if activeJournal {
			return nativePackageBrokerCommitResult{
				changed: true, rollback: "failed", exitCode: 3, journal: replayProof,
			}, replayErr
		}
		return preflightFailure(replayErr)
	}
	if replayed {
		logger.Info("Replayed exact durable nested broker readiness",
			"transactionId", replayProof.TransactionID, "journalDigest", replayProof.Digest)
		return nativePackageBrokerCommitResult{
			success: true, changed: true, rollback: "not-needed", exitCode: 0,
			journal: replayProof,
		}, nil
	}
	if activeJournal {
		return nativePackageBrokerCommitResult{
			changed: true, rollback: "succeeded", exitCode: 1, journal: replayProof,
		}, errors.New("reconciled an interrupted nested broker transaction to its exact prior state")
	}
	if recoveryOnly {
		return preflightFailure(errors.New(
			"broker recovery query found no exact active child transaction and will not start a new one",
		))
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
		nestedBrokerCommit:  true,
		tokenSHA256:         expectedTokenSHA256,
		boundOuterTokenPath: tokenPath,
	}
	err = runNativePackageTransaction(ctx, logger, transaction)
	if err == nil {
		return nativePackageBrokerCommitResult{
			success: true, changed: transaction.nestedMutationStarted,
			rollback: "not-needed", exitCode: 0, journal: transaction.brokerJournalProof,
		}, nil
	}
	if !transaction.nestedMutationStarted {
		return nativePackageBrokerPreflightFailure(err)
	}
	if transaction.nestedRollbackSucceeded {
		return nativePackageBrokerCommitResult{
			changed: true, rollback: "succeeded", exitCode: 1,
			journal: transaction.brokerJournalProof,
		}, err
	}
	return nativePackageBrokerCommitResult{
		changed: true, rollback: "failed", exitCode: 3,
		journal: transaction.brokerJournalProof,
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
	// Every outer install mode participates in the machine-global trust order.
	// Production does not mutate local-test trust, but it must serialize ahead
	// of Package and Service so it cannot become a successor underneath a
	// pending recovery or ownership transaction.
	if err := initializeNativePackageRecoveryTrustLease(); err != nil {
		return fmt.Errorf("initialize protected package trust lease: %w", err)
	}
	trustDeadline := time.Now().Add(mutexBudget)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(trustDeadline) {
		trustDeadline = contextDeadline
	}
	trustLease, trustDirectories, err := acquireNativePackageRecoveryTrustLease(ctx, trustDeadline)
	if err != nil {
		return fmt.Errorf("acquire protected package trust lease: %w", err)
	}
	t.trustLeaseDirectoryHandles = trustDirectories
	t.releaseTrustLease = func() error {
		err := releaseNativePackageRecoveryTrustLease(
			trustLease, t.trustLeaseDirectoryHandles,
		)
		t.trustLeaseDirectoryHandles = nil
		return err
	}
	packageBudget := mutexBudget
	if deadline, ok := ctx.Deadline(); ok {
		packageBudget = time.Until(deadline)
		if packageBudget <= 0 {
			return context.DeadlineExceeded
		}
	}
	release, err := acquireNamedNativePackageMutex(nativePackageMutexName, packageBudget)
	if err != nil {
		return err
	}
	t.releaseMutex = release
	if err := requireNativePackageTrustRecoveryClear(); err != nil {
		return fmt.Errorf("admit package install after failed-install trust recovery: %w", err)
	}
	if err := t.admitProductionLocalTestTrust(); err != nil {
		return fmt.Errorf("admit production install after local-test trust lifecycle: %w", err)
	}
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
	inputs := []struct {
		name      string
		directory string
	}{
		{name: "broker source", directory: filepath.Dir(t.request.brokerSource)},
		{name: "driver helper", directory: filepath.Dir(t.request.driverHelper)},
		{name: "submission manifest", directory: filepath.Dir(t.request.submissionManifest)},
		{name: "signed driver package", directory: t.request.packageDirectory},
	}
	if t.request.driverValidationMode == "local-test" {
		inputs = append(inputs, struct {
			name      string
			directory string
		}{name: "local-test certificate", directory: filepath.Dir(t.request.localTestCertificatePath)})
	}
	for _, input := range inputs {
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
	if t.request.driverValidationMode == "local-test" {
		if err := t.verifyLocalTestTrustCapability(); err != nil {
			return fmt.Errorf("verify parent-bound local-test trust capability: %w", err)
		}
		t.localTestCertificateHandle, err = t.lockAndVerifyInput(
			t.request.localTestCertificatePath,
			t.request.expectedLocalTestCertificateSHA256,
			false,
		)
		if err != nil {
			return fmt.Errorf("verify source-bound local-test certificate: %w", err)
		}
		t.localTestCertificateDER, err = readNativePackageRecoveryFile(
			t.localTestCertificateHandle, nativePackageRecoveryMaximumCertificateBytes,
		)
		if err != nil {
			return fmt.Errorf("read source-bound local-test certificate: %w", err)
		}
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

func (t *windowsNativePackageTransaction) verifyLocalTestTrustCapability() error {
	capabilityPath := filepath.Clean(t.request.localTestTrustCapability)
	if filepath.Base(capabilityPath) != nativePackageLocalTestTrustCapabilityName ||
		!strings.EqualFold(filepath.Dir(capabilityPath), filepath.Dir(t.request.brokerSource)) {
		return errors.New("local-test trust capability is not the exact protected sibling of the staged broker")
	}
	handle, err := lockNativePackageInput(capabilityPath)
	if err != nil {
		return fmt.Errorf("lock local-test trust capability: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			windows.CloseHandle(handle) //nolint:errcheck
		}
	}()
	if err := validateNativeSecurityDescriptor(
		handle, nativePackageRecoveryTrustLeaseFileSDDL,
	); err != nil {
		return fmt.Errorf("validate local-test trust capability security: %w", err)
	}
	digest, err := hashNativePackageHandle(handle)
	if err != nil {
		return fmt.Errorf("hash local-test trust capability: %w", err)
	}
	if digest != t.request.expectedTrustCapabilitySHA256 {
		return fmt.Errorf("local-test trust capability SHA-256=%s expected=%s",
			digest, t.request.expectedTrustCapabilitySHA256)
	}
	contents, err := readNativePackageCapabilityHandle(
		handle, nativePackageLocalTestTrustCapabilityMaximumBytes,
	)
	if err != nil {
		return fmt.Errorf("read local-test trust capability: %w", err)
	}
	capability := nativePackageLocalTestTrustCapability{}
	if err := decodeCanonicalNativeBrokerJSON(
		contents, &capability, nativePackageLocalTestTrustCapabilityMaximumBytes,
	); err != nil {
		return fmt.Errorf("decode canonical local-test trust capability: %w", err)
	}
	paths, err := resolveNativePackageRecoveryMarkerPaths()
	if err != nil {
		return err
	}
	parentPID, parentCreationFileTime, err := nativePackageParentIdentity()
	if err != nil {
		return err
	}
	if err := validateNativePackageLocalTestTrustCapability(
		capability, t.request, paths.directory, parentPID, parentCreationFileTime,
	); err != nil {
		return err
	}
	t.inputHandles = append(t.inputHandles, handle)
	keep = true
	return nil
}

func decodeCanonicalNativePackageLocalTestTrustOwnership(
	contents []byte,
) (nativePackageLocalTestTrustOwnership, error) {
	value := nativePackageLocalTestTrustOwnership{}
	if len(contents) < 2 || len(contents) > nativePackageLocalTestTrustCapabilityMaximumBytes ||
		contents[len(contents)-1] != '\n' || bytes.IndexByte(contents[:len(contents)-1], '\n') >= 0 {
		return value, errors.New("local-test trust ownership journal has invalid framing")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents[:len(contents)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode local-test trust ownership journal: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return value, errors.New("local-test trust ownership journal has trailing JSON")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return value, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(canonical, contents) {
		return value, errors.New("local-test trust ownership journal is not canonical")
	}
	if value.Schema != nativePackageLocalTestTrustOwnershipSchema ||
		!nativePackageHexRevision.MatchString(value.SourceRevision) ||
		!nativePackageSHA256.MatchString(value.CertificateSHA256) ||
		!nativePackageSHA256.MatchString(value.PackageLockSHA256) ||
		value.BaselineRoot < 0 || value.BaselineRoot > 1 ||
		value.BaselineTrustedPublisher < 0 || value.BaselineTrustedPublisher > 1 {
		return value, errors.New("local-test trust ownership journal schema or baselines are invalid")
	}
	return value, nil
}

func validateNativePackageLocalTestTrustCapability(
	capability nativePackageLocalTestTrustCapability,
	request nativePackageRequest,
	expectedJournalDirectory string,
	parentPID uint32,
	parentCreationFileTime uint64,
) error {
	if capability.Schema != nativePackageLocalTestTrustCapabilitySchema ||
		len(capability.Nonce) != 32 || capability.Nonce != strings.ToLower(capability.Nonce) {
		return errors.New("local-test trust capability schema or nonce is noncanonical")
	}
	if _, err := hex.DecodeString(capability.Nonce); err != nil {
		return errors.New("local-test trust capability nonce is not 128-bit lowercase hexadecimal")
	}
	if capability.SourceRevision != request.sourceRevision ||
		!strings.EqualFold(filepath.Clean(capability.CertificatePath), filepath.Clean(request.localTestCertificatePath)) ||
		capability.CertificateSHA256 != request.expectedLocalTestCertificateSHA256 ||
		capability.PackageLockSHA256 != request.expectedLocalTestPackageLockSHA256 {
		return errors.New("local-test trust capability does not bind the exact source, certificate path/hash, and package lock")
	}
	if capability.TrustJournalSchema != nativePackageLocalTestTrustOwnershipSchema ||
		!strings.EqualFold(filepath.Clean(capability.TrustJournalDirectory), expectedJournalDirectory) {
		return errors.New("local-test trust capability does not bind the native fixed ownership journal")
	}
	if capability.ParentPID != parentPID ||
		capability.ParentCreationFileTime != parentCreationFileTime {
		return errors.New("local-test trust capability was not issued by this broker process parent")
	}
	return nil
}

func nativePackageParentIdentity() (uint32, uint64, error) {
	parent := os.Getppid()
	if parent <= 0 || uint64(parent) > uint64(^uint32(0)) {
		return 0, 0, errors.New("native package parent process ID is invalid")
	}
	pid := uint32(parent)
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, 0, fmt.Errorf("open native package parent process %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	creation := windows.Filetime{}
	exit := windows.Filetime{}
	kernel := windows.Filetime{}
	user := windows.Filetime{}
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, 0, fmt.Errorf("query native package parent creation time: %w", err)
	}
	creationFileTime := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	if creationFileTime == 0 {
		return 0, 0, errors.New("native package parent creation time is zero")
	}
	return pid, creationFileTime, nil
}

func readNativePackageCapabilityHandle(handle windows.Handle, maximum int) ([]byte, error) {
	information := windows.ByHandleFileInformation{}
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return nil, err
	}
	size := uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow)
	if size == 0 || size > uint64(maximum) {
		return nil, errors.New("local-test trust capability length is outside its exact bound")
	}
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return nil, err
	}
	contents := make([]byte, int(size))
	offset := 0
	for offset < len(contents) {
		var read uint32
		if err := windows.ReadFile(handle, contents[offset:], &read, nil); err != nil {
			return nil, err
		}
		if read == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		offset += int(read)
	}
	extra := []byte{0}
	var read uint32
	if err := windows.ReadFile(handle, extra, &read, nil); err != nil {
		return nil, err
	}
	if read != 0 {
		return nil, errors.New("local-test trust capability changed length while locked")
	}
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return nil, err
	}
	return contents, nil
}

// initializeNativePackageRecoveryTrustLease is the sole initializer for the
// fixed machine-wide trust lock. Both install and recovery call it before
// acquiring byte zero; PowerShell never creates, repairs, or owns this object.
func initializeNativePackageRecoveryTrustLease() error {
	paths, err := resolveNativePackageRecoveryMarkerPaths()
	if err != nil {
		return err
	}
	parentHandles, err := lockNativePackageDirectoryChain(filepath.Dir(paths.directory))
	if err != nil {
		return fmt.Errorf("lock fixed trust directory parent: %w", err)
	}
	defer closeNativePackageUninstallHandles(parentHandles)
	directorySecurity, err := nativeSecurityAttributes(
		nativePackageRecoveryTrustLeaseDirectorySDDL,
	)
	if err != nil {
		return err
	}
	directoryPointer, err := windows.UTF16PtrFromString(paths.directory)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(directoryPointer, directorySecurity); err != nil &&
		!errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("create fixed trust directory: %w", err)
	}
	directoryHandle, err := openNativePathWithoutReparse(
		paths.directory, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, true,
	)
	if err != nil {
		return fmt.Errorf("open fixed trust directory: %w", err)
	}
	defer windows.CloseHandle(directoryHandle) //nolint:errcheck
	if err := validateNativeSecurityDescriptor(
		directoryHandle, nativePackageRecoveryTrustLeaseDirectorySDDL,
	); err != nil {
		return fmt.Errorf("validate fixed trust directory: %w", err)
	}

	fileSecurity, err := nativeSecurityAttributes(nativePackageRecoveryTrustLeaseFileSDDL)
	if err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate fixed trust lease preparation identity: %w", err)
	}
	temporary := filepath.Join(
		paths.directory,
		nativePackageRecoveryTrustLeaseFileName+"."+hex.EncodeToString(nonce[:])+".preparing",
	)
	if !strings.EqualFold(filepath.Dir(temporary), paths.directory) {
		return errors.New("fixed trust lease preparation escaped its directory")
	}
	leasePointer, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	lease, err := windows.CreateFile(
		leasePointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		fileSecurity,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_WRITE_THROUGH|
			windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err == nil {
		removeTemporary := true
		defer func() {
			if removeTemporary {
				deleteNativePackageFile(temporary) //nolint:errcheck
			}
		}()
		marker := []byte{1}
		var written uint32
		if writeErr := windows.WriteFile(lease, marker, &written, nil); writeErr != nil || written != 1 {
			windows.CloseHandle(lease) //nolint:errcheck
			if writeErr == nil {
				writeErr = io.ErrShortWrite
			}
			return fmt.Errorf("initialize fixed trust lease marker: %w", writeErr)
		}
		if flushErr := windows.FlushFileBuffers(lease); flushErr != nil {
			windows.CloseHandle(lease) //nolint:errcheck
			return fmt.Errorf("flush fixed trust lease marker: %w", flushErr)
		}
		if closeErr := windows.CloseHandle(lease); closeErr != nil {
			return fmt.Errorf("close initialized fixed trust lease: %w", closeErr)
		}
		prepublish, openErr := lockNativePackageInput(temporary)
		if openErr != nil {
			return fmt.Errorf("reopen fixed trust lease preparation: %w", openErr)
		}
		information := windows.ByHandleFileInformation{}
		validateErr := windows.GetFileInformationByHandle(prepublish, &information)
		if validateErr == nil && (information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
			information.FileSizeHigh != 0 || information.FileSizeLow != 1) {
			validateErr = errors.New("fixed trust lease preparation is not an exact one-byte regular file")
		}
		if validateErr == nil {
			validateErr = validateNativeFileLinkCount(information.NumberOfLinks)
		}
		if validateErr == nil {
			validateErr = validateNativeSecurityDescriptor(
				prepublish, nativePackageRecoveryTrustLeaseFileSDDL,
			)
		}
		var readback []byte
		if validateErr == nil {
			readback, validateErr = readNativePackageRecoveryFile(prepublish, 1)
		}
		closeErr := windows.CloseHandle(prepublish)
		if validateErr != nil || closeErr != nil || !bytes.Equal(readback, []byte{1}) {
			if validateErr == nil && closeErr == nil {
				validateErr = errors.New("fixed trust lease preparation readback changed")
			}
			return errors.Join(validateErr, closeErr)
		}
		if moveErr := moveNativePackageFile(temporary, paths.lease, false); moveErr == nil {
			removeTemporary = false
		} else if errors.Is(moveErr, windows.ERROR_FILE_EXISTS) ||
			errors.Is(moveErr, windows.ERROR_ALREADY_EXISTS) {
			if deleteErr := deleteNativePackageFile(temporary); deleteErr != nil {
				return fmt.Errorf("discard losing fixed trust lease preparation: %w", deleteErr)
			}
			removeTemporary = false
		} else {
			return fmt.Errorf("publish fixed trust lease: %w", moveErr)
		}
	} else if !errors.Is(err, windows.ERROR_FILE_EXISTS) &&
		!errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("create fixed trust lease: %w", err)
	}
	validated, validatedDirectories, err := openNativePackageRecoveryTrustLease()
	if err != nil {
		return fmt.Errorf("validate fixed trust lease: %w", err)
	}
	closeErr := windows.CloseHandle(validated)
	closeNativePackageUninstallHandles(validatedDirectories)
	if closeErr != nil {
		return fmt.Errorf("close validated fixed trust lease: %w", closeErr)
	}
	return nil
}

func resolveNativePackageLocalTestTrustPaths() (nativePackageLocalTestTrustPaths, error) {
	markerPaths, err := resolveNativePackageRecoveryMarkerPaths()
	if err != nil {
		return nativePackageLocalTestTrustPaths{}, err
	}
	paths := nativePackageLocalTestTrustPaths{
		directory:    markerPaths.directory,
		preparing:    filepath.Join(markerPaths.directory, nativePackageLocalTrustPreparingName),
		pending:      filepath.Join(markerPaths.directory, nativePackageLocalTrustPendingName),
		owned:        filepath.Join(markerPaths.directory, nativePackageLocalTrustOwnedName),
		uninstalling: filepath.Join(markerPaths.directory, nativePackageLocalTrustUninstallingName),
		cleared:      filepath.Join(markerPaths.directory, nativePackageLocalTrustClearedName),
	}
	for _, path := range []string{
		paths.preparing, paths.pending, paths.owned, paths.uninstalling, paths.cleared,
	} {
		if !strings.EqualFold(filepath.Dir(path), paths.directory) {
			return nativePackageLocalTestTrustPaths{}, errors.New(
				"local-test trust journal escaped its fixed protected directory",
			)
		}
	}
	return paths, nil
}

func canonicalNativePackageLocalTestTrustOwnership(
	record nativePackageLocalTestTrustOwnership,
) ([]byte, error) {
	if record.Schema != nativePackageLocalTestTrustOwnershipSchema ||
		!nativePackageHexRevision.MatchString(record.SourceRevision) ||
		!nativePackageSHA256.MatchString(record.CertificateSHA256) ||
		!nativePackageSHA256.MatchString(record.PackageLockSHA256) ||
		record.BaselineRoot < 0 || record.BaselineRoot > 1 ||
		record.BaselineTrustedPublisher < 0 || record.BaselineTrustedPublisher > 1 {
		return nil, errors.New("local-test trust ownership identity or baseline is invalid")
	}
	contents, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append(contents, '\n'), nil
}

func readNativePackageLocalTestTrustRecord(
	path string,
) (nativePackageLocalTestTrustOwnership, []byte, bool, error) {
	value := nativePackageLocalTestTrustOwnership{}
	handle, err := lockNativePackageInput(path)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return value, nil, false, nil
		}
		return value, nil, false, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	if err := validateNativeSecurityDescriptor(
		handle, nativePackageRecoveryTrustLeaseFileSDDL,
	); err != nil {
		return value, nil, false, fmt.Errorf("validate local-test trust record security: %w", err)
	}
	contents, err := readNativePackageRecoveryFile(
		handle, nativePackageLocalTestTrustCapabilityMaximumBytes,
	)
	if err != nil {
		return value, nil, false, err
	}
	value, err = decodeCanonicalNativePackageLocalTestTrustOwnership(contents)
	if err != nil {
		return value, nil, false, err
	}
	return value, contents, true, nil
}

func nativePackageLocalTestTrustRecordMatches(
	record nativePackageLocalTestTrustOwnership,
	request nativePackageRequest,
) bool {
	return record.Schema == nativePackageLocalTestTrustOwnershipSchema &&
		record.SourceRevision == request.sourceRevision &&
		record.CertificateSHA256 == request.expectedLocalTestCertificateSHA256 &&
		record.PackageLockSHA256 == request.expectedLocalTestPackageLockSHA256
}

// publishNativePackageLocalTestTrustPreparing never exposes partially written
// bytes at the canonical state name. A process cut can leave only a random
// protected scratch file, which no transaction interprets as authority.
func publishNativePackageLocalTestTrustPreparing(path string, contents []byte) error {
	directory := filepath.Dir(path)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := filepath.Join(
		directory, filepath.Base(path)+"."+hex.EncodeToString(nonce[:])+".scratch",
	)
	if !strings.EqualFold(filepath.Dir(temporary), directory) {
		return errors.New("local-test trust scratch escaped its protected directory")
	}
	if err := createExactNativePackageRecoveryPreparation(temporary, contents); err != nil {
		return fmt.Errorf("write protected local-test trust scratch: %w", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			deleteNativePackageFile(temporary) //nolint:errcheck
		}
	}()
	if err := moveNativePackageFile(temporary, path, false); err != nil {
		return fmt.Errorf("publish local-test trust preparing record: %w", err)
	}
	removeTemporary = false
	_, observed, exists, err := readNativePackageLocalTestTrustRecord(path)
	if err != nil {
		return err
	}
	if !exists || !bytes.Equal(observed, contents) {
		return errors.New("published local-test trust preparing record failed exact readback")
	}
	return nil
}

func transitionNativePackageLocalTestTrustRecord(
	source, destination string,
	expected []byte,
) error {
	_, sourceBytes, exists, err := readNativePackageLocalTestTrustRecord(source)
	if err != nil {
		return err
	}
	if !exists || !bytes.Equal(sourceBytes, expected) {
		return errors.New("local-test trust source record is missing or changed")
	}
	if _, _, destinationExists, err := readNativePackageLocalTestTrustRecord(destination); err != nil {
		return err
	} else if destinationExists {
		return errors.New("local-test trust destination record already exists")
	}
	if err := moveNativePackageFile(source, destination, false); err != nil {
		return err
	}
	_, destinationBytes, exists, err := readNativePackageLocalTestTrustRecord(destination)
	if err != nil {
		return err
	}
	if !exists || !bytes.Equal(destinationBytes, expected) {
		return errors.New("local-test trust destination record failed exact readback")
	}
	if _, _, sourceExists, err := readNativePackageLocalTestTrustRecord(source); err != nil {
		return err
	} else if sourceExists {
		return errors.New("local-test trust source remained after write-through transition")
	}
	return nil
}

func retireNativePackageLocalTestTrustRecord(path string, expected []byte) error {
	pointer, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return err
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
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	closed := false
	defer func() {
		if !closed {
			windows.CloseHandle(handle) //nolint:errcheck
		}
	}()
	if err := validateNativeSecurityDescriptor(
		handle, nativePackageRecoveryTrustLeaseFileSDDL,
	); err != nil {
		return err
	}
	information := windows.ByHandleFileInformation{}
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return err
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return errors.New("local-test trust record is not a regular non-reparse file")
	}
	if err := validateNativeFileLinkCount(information.NumberOfLinks); err != nil {
		return err
	}
	contents, err := readNativePackageRecoveryFile(
		handle, nativePackageLocalTestTrustCapabilityMaximumBytes,
	)
	if err != nil {
		return err
	}
	if _, err := decodeCanonicalNativePackageLocalTestTrustOwnership(contents); err != nil {
		return err
	}
	if expected != nil && !bytes.Equal(contents, expected) {
		return errors.New("local-test trust record changed before retirement")
	}
	if err := deleteNativePackageUninstallFileHandle(handle); err != nil {
		return err
	}
	if err := windows.CloseHandle(handle); err != nil {
		return err
	}
	closed = true
	return nil
}

func addExactNativePackageLocalTestCertificate(
	store windows.Handle,
	certificateDER []byte,
) error {
	if len(certificateDER) == 0 || len(certificateDER) > nativePackageRecoveryMaximumCertificateBytes {
		return errors.New("local-test certificate length is invalid")
	}
	certificate, err := windows.CertCreateCertificateContext(
		windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING,
		&certificateDER[0],
		uint32(len(certificateDER)),
	)
	if err != nil {
		return err
	}
	defer windows.CertFreeCertificateContext(certificate) //nolint:errcheck
	var added *windows.CertContext
	if err := windows.CertAddCertificateContextToStore(
		store, certificate, windows.CERT_STORE_ADD_NEW, &added,
	); err != nil {
		return err
	}
	if added != nil {
		windows.CertFreeCertificateContext(added) //nolint:errcheck
	}
	return nil
}

func countExactNativePackageLocalTestCertificateRejectingThumbprintCollisions(
	store windows.Handle,
	expectedDER []byte,
) (int, error) {
	if len(expectedDER) == 0 || len(expectedDER) > nativePackageRecoveryMaximumCertificateBytes {
		return 0, errors.New("local-test certificate length is invalid")
	}
	expectedThumbprint := sha1.Sum(expectedDER) // #nosec G401 -- Windows store identity collision check.
	count := 0
	var previous *windows.CertContext
	for enumerated := 0; enumerated < 65536; enumerated++ {
		certificate, err := windows.CertEnumCertificatesInStore(store, previous)
		if err != nil {
			if errors.Is(err, syscall.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return count, nil
			}
			return 0, err
		}
		previous = certificate
		if certificate == nil || certificate.EncodedCert == nil {
			continue
		}
		encoded := unsafe.Slice(certificate.EncodedCert, int(certificate.Length))
		if bytes.Equal(encoded, expectedDER) {
			count++
			continue
		}
		observedThumbprint := sha1.Sum(encoded) // #nosec G401 -- collision rejection only.
		if observedThumbprint == expectedThumbprint {
			if previous != nil {
				windows.CertFreeCertificateContext(previous) //nolint:errcheck
				previous = nil
			}
			return 0, errors.New(
				"LocalMachine certificate store contains a different certificate with the same Windows SHA-1 thumbprint",
			)
		}
	}
	if previous != nil {
		windows.CertFreeCertificateContext(previous) //nolint:errcheck
	}
	return 0, errors.New("local-test certificate store enumeration exceeded its safety bound")
}

func inspectNativePackageLocalTestTrust(
	root windows.Handle,
	publisher windows.Handle,
	expectedDER []byte,
) (nativePackageRecoveryTrustCounts, error) {
	rootCount, err := countExactNativePackageLocalTestCertificateRejectingThumbprintCollisions(
		root, expectedDER,
	)
	if err != nil {
		return nativePackageRecoveryTrustCounts{}, fmt.Errorf("inspect LocalMachine Root: %w", err)
	}
	publisherCount, err := countExactNativePackageLocalTestCertificateRejectingThumbprintCollisions(
		publisher, expectedDER,
	)
	if err != nil {
		return nativePackageRecoveryTrustCounts{}, fmt.Errorf(
			"inspect LocalMachine TrustedPublisher: %w", err,
		)
	}
	return nativePackageRecoveryTrustCounts{
		root: rootCount, trustedPublisher: publisherCount,
	}, nil
}

func restoreNativePackageLocalTestTrustStores(
	rootStore, publisherStore windows.Handle,
	certificateDER []byte,
	record nativePackageLocalTestTrustOwnership,
	cutpoint func(string) error,
) error {
	stores := []struct {
		name     string
		handle   windows.Handle
		baseline int
		cut      string
	}{
		{name: "Root", handle: rootStore, baseline: record.BaselineRoot, cut: "root-restored"},
		{name: "TrustedPublisher", handle: publisherStore, baseline: record.BaselineTrustedPublisher, cut: "trusted-publisher-restored"},
	}
	for _, store := range stores {
		if err := executeNativePackageLocalTestTrustStep(
			store.cut, false,
			func() error {
				count, err := countExactNativePackageLocalTestCertificateRejectingThumbprintCollisions(
					store.handle, certificateDER,
				)
				if err != nil {
					return fmt.Errorf("inspect LocalMachine %s before baseline restore: %w", store.name, err)
				}
				if count < 0 || count > 1 {
					return fmt.Errorf("LocalMachine %s contains %d exact local-test certificates", store.name, count)
				}
				if count == 1 && store.baseline == 0 {
					if err := deleteExactNativePackageRecoveryCertificate(store.handle, certificateDER); err != nil {
						return fmt.Errorf("restore absent LocalMachine %s baseline: %w", store.name, err)
					}
				} else if count == 0 && store.baseline == 1 {
					if err := addExactNativePackageLocalTestCertificate(store.handle, certificateDER); err != nil {
						return fmt.Errorf("restore present LocalMachine %s baseline: %w", store.name, err)
					}
				}
				count, err = countExactNativePackageLocalTestCertificateRejectingThumbprintCollisions(
					store.handle, certificateDER,
				)
				if err != nil || count != store.baseline {
					if err == nil {
						err = fmt.Errorf("observed exact count %d expected %d", count, store.baseline)
					}
					return fmt.Errorf("verify LocalMachine %s baseline restore: %w", store.name, err)
				}
				return nil
			},
			cutpoint,
		); err != nil {
			return err
		}
	}
	return nil
}

func proveNativePackageLocalTestTopologyAbsent(
	ctx context.Context,
	logger *slog.Logger,
	helperPath string,
	targetUserSID string,
) error {
	deadline := time.Now().Add(nativePackageRollbackTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if !deadline.After(time.Now()) {
		return context.DeadlineExceeded
	}
	output, exitCode, waitErr := executeNativePackageRecoveryHelper(
		helperPath,
		[]string{
			"recover-failed-install-recordless",
			"--transaction-deadline-unix-ms", strconv.FormatInt(deadline.UnixMilli(), 10),
		},
	)
	if waitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) {
			return fmt.Errorf("join recordless topology proof: %w", waitErr)
		}
	}
	if _, err := parseNativePackageRecoverProof(output, exitCode); err != nil {
		return fmt.Errorf("validate recordless topology proof: %w", err)
	}
	statusOutput, statusExitCode, statusWaitErr := executeNativePackageRecoveryHelper(
		helperPath, []string{"status"},
	)
	if statusWaitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(statusWaitErr, &exitError) {
			return fmt.Errorf("join topology status proof: %w", statusWaitErr)
		}
	}
	if err := validateNativePackageRecoverEmptyStatus(statusOutput, statusExitCode); err != nil {
		return fmt.Errorf("validate empty topology status: %w", err)
	}
	if err := proveNativePackageNormalBrokerJournalsQuiescent(
		logger, targetUserSID,
	); err != nil {
		return err
	}
	for _, serviceName := range []string{
		NativeBrokerServiceName, nativePackageRecoveryDriverServiceName,
	} {
		if err := requireNativePackageRecoveryServiceAbsent(serviceName); err != nil {
			return err
		}
	}
	return nil
}

// proveNativePackageSettledLocalTestTopologyAbsentReadOnly is the narrow
// admission proof for replaying an already-cleared or recordless local-test
// Uninstall. Unlike the recovery/rollback proof above, it never invokes
// recordless recovery and never reconciles or retires a broker journal. Any
// journal entry can belong to a production successor, so even a validated
// settled tombstone is a hard stop on this source-bound no-op path.
func proveNativePackageSettledLocalTestTopologyAbsentReadOnly(
	ctx context.Context,
	helperPath string,
	targetUserSID string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, serviceName := range []string{
		NativeBrokerServiceName, nativePackageRecoveryDriverServiceName,
	} {
		if err := requireNativePackageRecoveryServiceAbsent(serviceName); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, _, err := nativeBrokerJournalPaths(targetUserSID)
	if err != nil {
		return err
	}
	rootHandle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(root, false)
	if err != nil {
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
			!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return fmt.Errorf("open normal broker journal root read-only: %w", err)
		}
	} else {
		defer windows.CloseHandle(rootHandle) //nolint:errcheck
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			return fmt.Errorf("enumerate normal broker journal root read-only: %w", readErr)
		}
		if len(entries) != 0 {
			return fmt.Errorf(
				"broker journal artifact %q blocks settled local-test uninstall admission",
				entries[0].Name(),
			)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	statusOutput, statusExitCode, statusWaitErr := executeNativePackageRecoveryHelper(
		helperPath, []string{"status"},
	)
	if statusWaitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(statusWaitErr, &exitError) {
			return fmt.Errorf("join read-only topology status proof: %w", statusWaitErr)
		}
	}
	if err := validateNativePackageRecoverEmptyStatus(statusOutput, statusExitCode); err != nil {
		return fmt.Errorf("validate read-only empty topology status: %w", err)
	}
	return ctx.Err()
}

func proveNativePackageNormalBrokerJournalsQuiescent(
	logger *slog.Logger,
	targetUserSID string,
) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := reconcileNativeBrokerJournalInactiveDirectories(logger, targetUserSID); err != nil {
		return fmt.Errorf("reconcile inactive broker journals: %w", err)
	}
	root, _, err := nativeBrokerJournalPaths(targetUserSID)
	if err != nil {
		return err
	}
	rootHandle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(root, false)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return fmt.Errorf("open normal broker journal root: %w", err)
	}
	defer windows.CloseHandle(rootHandle) //nolint:errcheck
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == nativeBrokerJournalActiveName ||
			isNativeBrokerJournalInactiveDirectoryName(name, nativeBrokerJournalPreparingPrefix) {
			return fmt.Errorf("active or preparing broker journal blocks trust cleanup: %s", name)
		}
		if !entry.IsDir() ||
			!isNativeBrokerJournalInactiveDirectoryName(name, nativeBrokerJournalSettledPrefix) {
			return fmt.Errorf("unknown broker journal artifact blocks trust cleanup: %s", name)
		}
		path := filepath.Join(root, name)
		if !strings.EqualFold(filepath.Dir(path), root) {
			return errors.New("settled broker journal escaped its protected root")
		}
		handle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(path, false)
		if err != nil {
			return fmt.Errorf("validate settled broker journal tombstone %s: %w", name, err)
		}
		windows.CloseHandle(handle) //nolint:errcheck
	}
	return nil
}

func (t *windowsNativePackageTransaction) localTestTrustCut(name string) error {
	if t.localTestTrustCutpoint == nil {
		return nil
	}
	return t.localTestTrustCutpoint(name)
}

func (t *windowsNativePackageTransaction) requireLocalTestTrustLocks() error {
	if t.releaseTrustLease == nil || t.releaseMutex == nil || t.releaseServiceMutex == nil {
		return errors.New("local-test trust mutation requires Trust -> Package -> Service locks")
	}
	return nil
}

func (t *windowsNativePackageTransaction) admitProductionLocalTestTrust() error {
	if t.nestedBrokerCommit || t.request.driverValidationMode != "production" {
		return nil
	}
	if t.releaseTrustLease == nil || t.releaseMutex == nil {
		return errors.New("production trust admission requires Trust -> Package locks")
	}
	if t.releaseServiceMutex != nil {
		return errors.New("production trust admission must precede the Service lock")
	}
	paths, err := resolveNativePackageLocalTestTrustPaths()
	if err != nil {
		return err
	}
	type observedRecord struct {
		state string
		path  string
		bytes []byte
	}
	var observed []observedRecord
	for _, candidate := range []struct {
		state string
		path  string
	}{
		{state: "preparing", path: paths.preparing},
		{state: "pending", path: paths.pending},
		{state: "owned", path: paths.owned},
		{state: "uninstalling", path: paths.uninstalling},
		{state: "cleared", path: paths.cleared},
	} {
		_, contents, exists, readErr := readNativePackageLocalTestTrustRecord(candidate.path)
		if readErr != nil {
			return fmt.Errorf("read local-test trust %s record for production admission: %w",
				candidate.state, readErr)
		}
		if exists {
			observed = append(observed, observedRecord{
				state: candidate.state, path: candidate.path, bytes: contents,
			})
		}
	}
	states := make([]string, len(observed))
	for index := range observed {
		states[index] = observed[index].state
	}
	retireCleared, err := nativePackageProductionLocalTrustAdmission(states)
	if err != nil {
		return err
	}
	if !retireCleared {
		return nil
	}
	if err := retireNativePackageLocalTestTrustRecord(
		observed[0].path, observed[0].bytes,
	); err != nil {
		return fmt.Errorf("retire validated terminal local-test trust settlement: %w", err)
	}
	return nil
}

func (t *windowsNativePackageTransaction) openLocalTestTrustStores() (
	windows.Handle, windows.Handle, error,
) {
	root, err := openNativePackageRecoveryCertificateStore("Root")
	if err != nil {
		return 0, 0, fmt.Errorf("open LocalMachine Root for local-test trust: %w", err)
	}
	publisher, err := openNativePackageRecoveryCertificateStore("TrustedPublisher")
	if err != nil {
		windows.CertCloseStore(root, 0) //nolint:errcheck
		return 0, 0, fmt.Errorf("open LocalMachine TrustedPublisher for local-test trust: %w", err)
	}
	return root, publisher, nil
}

func (t *windowsNativePackageTransaction) prepareLocalTestTrust(ctx context.Context) error {
	if t.request.driverValidationMode != "local-test" || t.nestedBrokerCommit {
		return nil
	}
	if t.localTestTrust != nil {
		return nil
	}
	if err := t.requireLocalTestTrustLocks(); err != nil {
		return err
	}
	if err := requireNativePackageTrustRecoveryClear(); err != nil {
		return fmt.Errorf("revalidate failed-install recovery admission: %w", err)
	}
	paths, err := resolveNativePackageLocalTestTrustPaths()
	if err != nil {
		return err
	}
	type observedRecord struct {
		state  string
		path   string
		record nativePackageLocalTestTrustOwnership
		bytes  []byte
	}
	var observed []observedRecord
	for _, candidate := range []struct {
		state string
		path  string
	}{
		{state: "preparing", path: paths.preparing},
		{state: "pending", path: paths.pending},
		{state: "owned", path: paths.owned},
		{state: "uninstalling", path: paths.uninstalling},
		{state: "cleared", path: paths.cleared},
	} {
		record, contents, exists, readErr := readNativePackageLocalTestTrustRecord(candidate.path)
		if readErr != nil {
			return fmt.Errorf("read local-test trust %s record: %w", candidate.state, readErr)
		}
		if exists {
			observed = append(observed, observedRecord{
				state: candidate.state, path: candidate.path, record: record, bytes: contents,
			})
		}
	}
	if len(observed) > 1 {
		return errors.New("multiple local-test trust ownership states exist")
	}
	rootStore, publisherStore, err := t.openLocalTestTrustStores()
	if err != nil {
		return err
	}
	state := &nativePackageLocalTestTrustState{
		paths: paths, certificateDER: append([]byte(nil), t.localTestCertificateDER...),
		rootStore: rootStore, publisherStore: publisherStore,
	}
	t.localTestTrust = state

	if len(observed) == 1 && observed[0].state == "cleared" {
		if err := retireNativePackageLocalTestTrustRecord(
			observed[0].path, observed[0].bytes,
		); err != nil {
			return fmt.Errorf("retire settled local-test trust record: %w", err)
		}
		observed = nil
	}
	if len(observed) == 1 {
		current := observed[0]
		if !nativePackageLocalTestTrustRecordMatches(current.record, t.request) {
			return errors.New("active local-test trust ownership belongs to a different source-bound package")
		}
		state.record = current.record
		state.bytes = append([]byte(nil), current.bytes...)
		state.state = current.state
		state.resumed = true
		switch current.state {
		case "preparing":
			if err := transitionNativePackageLocalTestTrustRecord(
				paths.preparing, paths.pending, state.bytes,
			); err != nil {
				return fmt.Errorf("resume local-test trust preparing record: %w", err)
			}
			state.state = "pending"
		case "pending":
		case "owned":
			state.alreadyOwned = true
		case "uninstalling":
			if err := proveNativePackageLocalTestTopologyAbsent(
				ctx, t.logger, t.request.driverHelper, t.request.targetUserSID,
			); err != nil {
				return fmt.Errorf("resume local-test trust baseline restore only after topology absence: %w", err)
			}
			if err := restoreNativePackageLocalTestTrustStores(
				rootStore, publisherStore, state.certificateDER, state.record,
				t.localTestTrustCut,
			); err != nil {
				return err
			}
			if err := executeNativePackageLocalTestTrustStep(
				"cleared-published", false,
				func() error {
					return transitionNativePackageLocalTestTrustRecord(
						paths.uninstalling, paths.cleared, state.bytes,
					)
				},
				t.localTestTrustCut,
			); err != nil {
				return fmt.Errorf("settle resumed local-test trust baseline restore: %w", err)
			}
			if err := retireNativePackageLocalTestTrustRecord(paths.cleared, state.bytes); err != nil {
				return fmt.Errorf("retire resumed local-test trust settlement: %w", err)
			}
			t.localTestTrust = nil
			windows.CertCloseStore(rootStore, 0)      //nolint:errcheck
			windows.CertCloseStore(publisherStore, 0) //nolint:errcheck
			return t.prepareLocalTestTrust(ctx)
		default:
			return errors.New("unknown local-test trust ownership state")
		}
	} else {
		counts, err := inspectNativePackageLocalTestTrust(
			rootStore, publisherStore, state.certificateDER,
		)
		if err != nil {
			return err
		}
		if counts.root < 0 || counts.root > 1 ||
			counts.trustedPublisher < 0 || counts.trustedPublisher > 1 {
			return fmt.Errorf(
				"local-test trust baseline must contain at most one exact certificate per store; observed Root=%d TrustedPublisher=%d",
				counts.root, counts.trustedPublisher,
			)
		}
		state.record = nativePackageLocalTestTrustOwnership{
			Schema:                   nativePackageLocalTestTrustOwnershipSchema,
			SourceRevision:           t.request.sourceRevision,
			CertificateSHA256:        t.request.expectedLocalTestCertificateSHA256,
			PackageLockSHA256:        t.request.expectedLocalTestPackageLockSHA256,
			BaselineRoot:             counts.root,
			BaselineTrustedPublisher: counts.trustedPublisher,
		}
		state.bytes, err = canonicalNativePackageLocalTestTrustOwnership(state.record)
		if err != nil {
			return err
		}
		if err := executeNativePackageLocalTestTrustStep(
			"preparing-published", false,
			func() error {
				return publishNativePackageLocalTestTrustPreparing(paths.preparing, state.bytes)
			},
			t.localTestTrustCut,
		); err != nil {
			return err
		}
		state.state = "preparing"
		state.createdCurrent = true
		if err := executeNativePackageLocalTestTrustStep(
			"pending-published", false,
			func() error {
				return transitionNativePackageLocalTestTrustRecord(
					paths.preparing, paths.pending, state.bytes,
				)
			},
			t.localTestTrustCut,
		); err != nil {
			return fmt.Errorf("publish local-test trust pending authority: %w", err)
		}
		state.state = "pending"
	}

	counts, err := inspectNativePackageLocalTestTrust(
		rootStore, publisherStore, state.certificateDER,
	)
	if err != nil {
		return err
	}
	if counts.root < 0 || counts.root > 1 ||
		counts.trustedPublisher < 0 || counts.trustedPublisher > 1 {
		return errors.New("local-test trust contains duplicate exact certificate entries")
	}
	if state.alreadyOwned && (counts.root != 1 || counts.trustedPublisher != 1) {
		return errors.New("owned local-test trust is missing from Root or TrustedPublisher")
	}
	stores := []struct {
		name   string
		handle windows.Handle
		count  int
		cut    string
	}{
		{name: "Root", handle: rootStore, count: counts.root, cut: "root-added"},
		{name: "TrustedPublisher", handle: publisherStore, count: counts.trustedPublisher, cut: "trusted-publisher-added"},
	}
	for _, store := range stores {
		if store.count == 0 {
			if err := executeNativePackageLocalTestTrustStep(
				store.cut, false,
				func() error {
					if err := addExactNativePackageLocalTestCertificate(store.handle, state.certificateDER); err != nil {
						return fmt.Errorf("add exact local-test certificate to LocalMachine %s: %w", store.name, err)
					}
					count, err := countExactNativePackageLocalTestCertificateRejectingThumbprintCollisions(
						store.handle, state.certificateDER,
					)
					if err != nil || count != 1 {
						if err == nil {
							err = fmt.Errorf("observed exact count %d expected 1", count)
						}
						return fmt.Errorf("verify LocalMachine %s local-test trust: %w", store.name, err)
					}
					return nil
				},
				t.localTestTrustCut,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *windowsNativePackageTransaction) ensureLocalTestServiceLock(ctx context.Context) error {
	if t.request.driverValidationMode != "local-test" || t.nestedBrokerCommit ||
		t.releaseServiceMutex != nil {
		return nil
	}
	budget := nativePackageRollbackTimeout
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
		if budget <= 0 {
			return context.DeadlineExceeded
		}
	}
	release, err := acquireNativeInstallMutex(budget)
	if err != nil {
		return fmt.Errorf("reacquire native broker service mutex for trust settlement: %w", err)
	}
	t.releaseServiceMutex = release
	return nil
}

func (t *windowsNativePackageTransaction) commitLocalTestTrust(ctx context.Context) error {
	if t.request.driverValidationMode != "local-test" || t.nestedBrokerCommit {
		return nil
	}
	if err := t.ensureLocalTestServiceLock(ctx); err != nil {
		return err
	}
	if err := t.requireLocalTestTrustLocks(); err != nil {
		return err
	}
	state := t.localTestTrust
	if state == nil {
		return errors.New("local-test package success has no native trust ownership state")
	}
	if !t.installProof || !t.driverInstallProofPresent ||
		!t.driverInstallProof.success || !t.driverHelperSettled {
		return errors.New("local-test trust cannot commit without settled authenticated topology success")
	}
	counts, err := inspectNativePackageLocalTestTrust(
		state.rootStore, state.publisherStore, state.certificateDER,
	)
	if err != nil {
		return err
	}
	if counts.root != 1 || counts.trustedPublisher != 1 {
		return fmt.Errorf(
			"local-test trust cannot commit without exact Root=1 TrustedPublisher=1; observed Root=%d TrustedPublisher=%d",
			counts.root, counts.trustedPublisher,
		)
	}
	if state.state == "owned" {
		return nil
	}
	if state.state != "pending" {
		return fmt.Errorf("local-test trust success cannot settle state %s", state.state)
	}
	if err := executeNativePackageLocalTestTrustStep(
		"topology-success-before-owned", true,
		func() error {
			return transitionNativePackageLocalTestTrustRecord(
				state.paths.pending, state.paths.owned, state.bytes,
			)
		},
		t.localTestTrustCut,
	); err != nil {
		return fmt.Errorf("publish local-test trust ownership after topology success: %w", err)
	}
	state.state = "owned"
	state.alreadyOwned = true
	return nil
}

func nativePackageLocalTestTrustMayRestoreAfterFailure(
	state *nativePackageLocalTestTrustState,
	proof nativePackageInstallProof,
	proofPresent bool,
	driverHelperSettled bool,
	pendingBrokerOuterSettlement bool,
	driverBrokerHandoff bool,
) bool {
	if state == nil || state.state != "pending" || !state.createdCurrent ||
		state.resumed || state.alreadyOwned || !proofPresent || !driverHelperSettled {
		return false
	}
	return !proof.success && !proof.changed && !proof.rebootRequired &&
		proof.rollback == "not-needed" && (proof.exitCode == 1 || proof.exitCode == 4) &&
		!pendingBrokerOuterSettlement && !driverBrokerHandoff
}

func (t *windowsNativePackageTransaction) maybeRestoreLocalTestTrustAfterFailure(
	ctx context.Context,
) error {
	if t.request.driverValidationMode != "local-test" || t.nestedBrokerCommit {
		return nil
	}
	state := t.localTestTrust
	if !nativePackageLocalTestTrustMayRestoreAfterFailure(
		state, t.driverInstallProof, t.driverInstallProofPresent, t.driverHelperSettled,
		t.pendingBrokerOuterSettlement, t.driverBrokerHandoff,
	) {
		return nil
	}
	settleCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), nativePackageRollbackTimeout,
	)
	defer cancel()
	if err := t.ensureLocalTestServiceLock(settleCtx); err != nil {
		return err
	}
	if err := t.requireLocalTestTrustLocks(); err != nil {
		return err
	}
	if err := proveNativePackageLocalTestTopologyAbsent(
		settleCtx, t.logger, t.request.driverHelper, t.request.targetUserSID,
	); err != nil {
		return fmt.Errorf("retain pending trust because topology absence is unproven: %w", err)
	}
	if err := transitionNativePackageLocalTestTrustRecord(
		state.paths.pending, state.paths.uninstalling, state.bytes,
	); err != nil {
		return fmt.Errorf("arm local-test trust baseline restore: %w", err)
	}
	state.state = "uninstalling"
	if err := restoreNativePackageLocalTestTrustStores(
		state.rootStore, state.publisherStore, state.certificateDER, state.record,
		t.localTestTrustCut,
	); err != nil {
		return err
	}
	if err := executeNativePackageLocalTestTrustStep(
		"cleared-published", false,
		func() error {
			return transitionNativePackageLocalTestTrustRecord(
				state.paths.uninstalling, state.paths.cleared, state.bytes,
			)
		},
		t.localTestTrustCut,
	); err != nil {
		return fmt.Errorf("publish local-test trust baseline settlement: %w", err)
	}
	state.state = "cleared"
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
	budget := nativePackageTransactionTimeout
	if deadline, ok := ctx.Deadline(); ok {
		budget = time.Until(deadline)
		if budget <= 0 {
			return nativePackageServiceSnapshot{}, context.DeadlineExceeded
		}
	}
	release, err := acquireNativeInstallMutex(budget)
	if err != nil {
		return nativePackageServiceSnapshot{}, fmt.Errorf("lock native broker service transaction: %w", err)
	}
	t.releaseServiceMutex = release
	if t.nestedBrokerCommit {
		if err := reconcileNativeBrokerJournalBeforeAdmission(
			ctx, t.logger, t.request.targetUserSID,
		); err != nil {
			return nativePackageServiceSnapshot{}, err
		}
	} else {
		pending, err := reconcileNativeBrokerJournalBeforeOuterPackage(
			ctx, t.logger, t.request.targetUserSID,
		)
		if err != nil {
			return nativePackageServiceSnapshot{}, err
		}
		t.pendingBrokerOuterSettlement = pending
	}
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
			handle, openErr := lockNativePackageInput(priorExecutable)
			if openErr == nil {
				priorHash, hashErr := hashNativePackageHandle(handle)
				closeErr := windows.CloseHandle(handle)
				if hashErr == nil && closeErr == nil {
					disposition = nativePackageServiceTrusted
					t.priorExecutableRelease = releaseExecutable
					t.priorExecutableSHA256 = priorHash
				} else {
					releaseExecutable()
					lockErr = errors.Join(hashErr, closeErr)
				}
			} else {
				releaseExecutable()
				lockErr = openErr
			}
		}
		if lockErr != nil {
			// An exact service name/path with weak image ACLs is stale package
			// ownership, not a trustworthy rollback source. It is removed and
			// recreated; never "repair" its ACL while old handles may exist.
			t.logger.Warn("Replacing weak exact-owned native broker service image",
				"path", priorExecutable, "error", lockErr)
		} else {
			t.priorServiceConfig = config
			t.priorServiceDACL = securityDescriptor
			t.priorServiceRecovery = append([]mgr.RecoveryAction(nil), recovery...)
			t.priorServiceReset = reset
			t.priorServiceNonCrash = nonCrash
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
	if t.nestedBrokerCommit && snapshot.disposition == nativePackageServiceTrusted && snapshot.wasRunning {
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
		if err := t.prepareLocalTestTrust(ctx); err != nil {
			return fmt.Errorf("prepare native local-test trust transaction: %w", err)
		}
		return t.preparePackageCoordination()
	}
	if t.nestedBrokerHealthy {
		return nil
	}
	journal, err := beginNativeBrokerJournal(ctx, t)
	if err != nil {
		return fmt.Errorf("arm durable native broker recovery: %w", err)
	}
	t.brokerJournal = journal
	t.brokerJournalProof = journal.proof()
	// From this point onward the nested callback may stop/delete SCM state or
	// publish the canonical broker image. Any failure must prove rollback before
	// the still-running helper may touch its captured driver snapshot again.
	t.nestedMutationStarted = true
	if snapshot.disposition == nativePackageServiceWeakExactOwned {
		if err := t.removeWeakExactOwnedService(ctx); err != nil {
			return err
		}
	}
	if t.service != nil && snapshot.disposition == nativePackageServiceTrusted &&
		strings.EqualFold(t.priorServiceExecutable, t.destination) {
		if snapshot.wasRunning {
			// STOP is itself the mutation. Arm reconciliation before sending it so
			// a timeout while StopPending cannot strand a formerly-running service.
			t.stoppedTrustedService = true
			if err := t.brokerJournal.appendPhase(nativeBrokerPhaseServiceStopIntent, ""); err != nil {
				return fmt.Errorf("journal prior broker stop intent: %w", err)
			}
			if err := stopNativeService(ctx, t.service, waitContext); err != nil {
				return fmt.Errorf("quiesce trusted %s for atomic image replacement: %w",
					NativeBrokerServiceName, err)
			}
			if err := t.brokerJournal.appendPhase(nativeBrokerPhaseServiceStopped, ""); err != nil {
				return fmt.Errorf("journal prior broker stopped state: %w", err)
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
				productionNativeInstallDependenciesWithJournal(
					t.request.targetUserSID, t.brokerJournal,
				),
				&evidence,
			); err != nil {
				t.nestedServiceRollbackSettled =
					!evidence.mutationStarted || evidence.rollbackSucceeded
				return fmt.Errorf("repair native broker transaction: %w", err)
			}
		}
	} else {
		if t.releaseServiceMutex == nil {
			return errors.New("outer native package transaction does not hold the service mutex")
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

func (t *windowsNativePackageTransaction) Commit(ctx context.Context) error {
	if err := t.commitLocalTestTrust(ctx); err != nil {
		return err
	}
	if t.nestedBrokerCommit && t.brokerJournal != nil {
		if err := t.brokerJournal.appendPhase(nativeBrokerPhaseNestedReady, ""); err != nil {
			return fmt.Errorf("persist nested broker readiness: %w", err)
		}
		t.brokerJournalProof = t.brokerJournal.proof()
	}
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
	if t.brokerJournal != nil && t.brokerJournal.lastPhase() != nativeBrokerPhaseRollbackSettled &&
		t.brokerJournal.lastPhase() != nativeBrokerPhaseOuterSettled &&
		t.brokerJournal.lastPhase() != nativeBrokerPhaseManual &&
		nativeBrokerJournalPhaseIndex(
			nativeBrokerForwardPhaseOrder, t.brokerJournal.lastPhase(),
		) >= 0 {
		if err := t.brokerJournal.appendPhase(nativeBrokerPhaseRollbackIntent, ""); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("persist broker rollback intent: %w", err))
		}
	}
	if t.destinationRelease != nil {
		t.destinationRelease()
		t.destinationRelease = nil
	}
	retainTokenForRecovery := !t.nestedBrokerCommit && t.driverBrokerHandoff &&
		!t.driverHelperSettled
	if retainTokenForRecovery {
		if t.tokenHandle != 0 {
			if err := windows.CloseHandle(t.tokenHandle); err != nil {
				rollbackErrors = append(rollbackErrors,
					fmt.Errorf("close retained package transaction token: %w", err))
			}
			t.tokenHandle = 0
		}
		t.logger.Warn("Retaining protected package transaction token for authoritative broker replay",
			"path", t.tokenPath)
	} else if err := t.releaseCoordinationToken(); err != nil {
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
		if t.brokerJournal != nil {
			if err := t.brokerJournal.appendPhase(nativeBrokerPhaseManual, ""); err != nil {
				rollbackErrors = append(rollbackErrors, err)
			}
			t.brokerJournalProof = t.brokerJournal.proof()
		}
		return errors.Join(rollbackErrors...)
	}
	if !t.nestedBrokerCommit && t.stoppedTrustedService && !t.driverHelperSettled {
		rollbackErrors = append(rollbackErrors, errors.New(
			"driver-helper or handoff proof is unsettled; leaving the prior trusted broker stopped for external reconciliation"))
		return errors.Join(rollbackErrors...)
	}
	if !t.nestedBrokerCommit && t.stoppedTrustedService {
		if err := t.restoreQuiescedPriorService(ctx); err != nil {
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("restore quiesced prior broker during outer rollback: %w", err))
		}
	}
	restored := true
	if err := t.restoreBrokerExecutable(); err != nil {
		restored = false
		rollbackErrors = append(rollbackErrors, err)
	}
	if t.brokerJournal != nil && restored {
		if err := t.brokerJournal.appendPhase(nativeBrokerPhaseRollbackImage, ""); err != nil {
			rollbackErrors = append(rollbackErrors, err)
			resultErr = errors.Join(rollbackErrors...)
			return resultErr
		}
		_, priorLegacy, err := t.brokerJournal.loadProtectedArtifacts()
		if err != nil {
			rollbackErrors = append(rollbackErrors, err)
		} else if err := restoreNativeBrokerJournalLegacy(ctx, t.brokerJournal, priorLegacy); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		} else if err := t.brokerJournal.appendPhase(nativeBrokerPhaseRollbackLegacy, ""); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
	}
	if t.nestedBrokerCommit && t.stoppedTrustedService && t.service != nil &&
		t.serviceSnapshot.wasRunning {
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
	if t.brokerJournal != nil {
		if len(rollbackErrors) != 0 {
			if err := t.brokerJournal.appendPhase(nativeBrokerPhaseManual, ""); err != nil {
				rollbackErrors = append(rollbackErrors, err)
			}
			t.brokerJournalProof = t.brokerJournal.proof()
		} else if err := t.brokerJournal.appendPhase(nativeBrokerPhaseRollbackSettled, ""); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		} else {
			t.brokerJournalProof = t.brokerJournal.proof()
			if err := retireNativeBrokerJournal(t.brokerJournal); err != nil {
				t.logger.Warn("Retaining settled native broker rollback journal for later cleanup",
					"transactionId", t.brokerJournalProof.TransactionID, "error", err)
			} else {
				t.brokerJournal = nil
			}
		}
	}
	if trustErr := t.maybeRestoreLocalTestTrustAfterFailure(ctx); trustErr != nil {
		rollbackErrors = append(rollbackErrors,
			fmt.Errorf("settle native local-test trust failure: %w", trustErr))
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
	if t.localTestTrust != nil {
		if t.localTestTrust.rootStore != 0 {
			windows.CertCloseStore(t.localTestTrust.rootStore, 0) //nolint:errcheck
			t.localTestTrust.rootStore = 0
		}
		if t.localTestTrust.publisherStore != 0 {
			windows.CertCloseStore(t.localTestTrust.publisherStore, 0) //nolint:errcheck
			t.localTestTrust.publisherStore = 0
		}
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
	if t.releaseTrustLease != nil {
		if err := t.releaseTrustLease(); err != nil {
			return fmt.Errorf("release package trust transaction: %w", err)
		}
		t.releaseTrustLease = nil
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
	t.driverInstallProof = proof
	t.driverInstallProofPresent = true
	if proof.journalRecovery == "replayed" && !t.pendingBrokerOuterSettlement {
		return errors.New("driver helper replayed a broker journal without a preexisting pending outer settlement")
	}
	if proof.journalRecovery != "replayed" && t.pendingBrokerOuterSettlement {
		return errors.New("driver helper did not replay the preexisting pending broker settlement")
	}
	settleCtx, cancelSettle := context.WithTimeout(
		context.WithoutCancel(ctx), nativePackageRollbackTimeout,
	)
	defer cancelSettle()
	if proof.success && proof.journal.TransactionID != "" {
		var activeJournal *nativeBrokerJournal
		var prepared nativeBrokerOuterSettlementPrepared
		var receipt nativePackageBrokerSettlementReceipt
		var settledJournal *nativeBrokerJournal
		var finalReceipt nativeBrokerOuterSettlementFinalPrepared
		journalErr := executeNativeBrokerOuterSettlement(nativeBrokerOuterSettlementOperations{
			recordPending: func() error {
				var err error
				activeJournal, prepared, err = armNativeBrokerOuterSettlement(
					settleCtx, t.request.targetUserSID, t.tokenSHA256,
					t.request.expectedBrokerSHA256, proof,
				)
				if activeJournal != nil {
					t.brokerJournalProof = activeJournal.proof()
				}
				return err
			},
			publishRequest: func() error {
				return publishNativeBrokerOuterSettlementRequest(activeJournal, prepared)
			},
			acknowledgeDriver: func() error {
				if activeJournal != nil &&
					activeJournal.lastPhase() == nativeBrokerPhaseOuterSettled {
					existingFinal, err := loadNativeBrokerOuterSettlementFinalForReconciliation(
						activeJournal,
					)
					if err == nil {
						finalReceipt = existingFinal
						receipt = nativeBrokerDriverReceiptFromFinal(existingFinal.Receipt)
						return validateNativeBrokerOuterSettlementReceipt(prepared, receipt)
					}
					if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
						!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
						return err
					}
				}
				var err error
				receipt, err = t.executeDriverBrokerSettlementAck(settleCtx, prepared)
				return err
			},
			recordBrokerSettled: func() error {
				var err error
				settledJournal, finalReceipt, err = recordNativeBrokerOuterSettlement(
					settleCtx, t.request.targetUserSID, receipt,
				)
				if settledJournal != nil {
					t.brokerJournalProof = settledJournal.proof()
				}
				return err
			},
			retireBrokerJournal: func() error {
				return retireNativeBrokerJournal(settledJournal)
			},
			discardInertState: func() error {
				return t.discardDriverBrokerSettlementTombstone(
					settleCtx, prepared, finalReceipt, receipt, t.brokerJournalProof,
				)
			},
			observeDiscardError: func(err error) {
				t.logger.Warn("Retaining inert settled transaction artifacts for later cleanup",
					"brokerTransactionId", proof.journal.TransactionID, "error", err)
			},
		})
		if journalErr != nil {
			t.driverHelperSettled = false
			return fmt.Errorf("complete durable two-phase broker settlement: %w", journalErr)
		}
		if proof.journalRecovery == "replayed" {
			if err := discardSettledNativeBrokerOuterToken(settledJournal); err != nil {
				t.logger.Warn("Retaining inert settled outer token after cleanup error",
					"brokerTransactionId", proof.journal.TransactionID, "error", err)
			}
		}
		t.logger.Info("Native broker and outer package journals reached exact settlement",
			"brokerTransactionId", t.brokerJournalProof.TransactionID,
			"brokerJournalDigest", t.brokerJournalProof.Digest,
			"driverTransactionId", receipt.DriverTransactionID,
			"driverJournalDigest", receipt.Digest)
	} else {
		journalProof, journalErr := reconcileNativeBrokerJournalAfterOuterFailure(
			settleCtx, t.request.targetUserSID, t.tokenSHA256,
			t.request.expectedBrokerSHA256, proof,
		)
		t.brokerJournalProof = journalProof
		if journalErr != nil {
			t.driverHelperSettled = false
			return fmt.Errorf("reconcile durable native broker ownership after driver proof: %w", journalErr)
		}
	}
	if proof.journalRecovery == "replayed" {
		t.replayedBrokerRecovery = true
	}
	t.driverHelperSettled = proof.exitCode != 3
	if proof.success && !t.driverBrokerHandoff && proof.journalRecovery != "replayed" {
		t.driverHelperSettled = false
		return errors.New("native driver helper reported success without the broker service handoff")
	}
	if !proof.success && t.stoppedTrustedService && t.driverHelperSettled {
		rollbackCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), nativePackageRollbackTimeout,
		)
		restoreErr := t.restoreQuiescedPriorService(rollbackCtx)
		cancel()
		if restoreErr != nil {
			return fmt.Errorf("restore trusted native broker after settled helper failure: %w", restoreErr)
		}
	}
	if proof.success {
		// The authenticated nested broker commit now owns the service run state.
		t.stoppedTrustedService = false
	}
	if t.driverCoordinationErr != nil {
		return fmt.Errorf("coordinate native broker quiescence: %w", t.driverCoordinationErr)
	}
	if t.request.driverValidationMode == "local-test" {
		lockCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), nativePackageRollbackTimeout,
		)
		lockErr := t.ensureLocalTestServiceLock(lockCtx)
		cancel()
		if lockErr != nil {
			return lockErr
		}
	}
	if proof.exitCode == nativePackageRebootRequiredCode {
		return &nativePackageRebootRequiredError{cause: fmt.Errorf("%w: %s", err, text)}
	}
	if !proof.success {
		return &nativePackageInstallExitError{
			cause: fmt.Errorf("native driver helper failed with exit %d: %w: %s",
				proof.exitCode, err, text),
			exitCode: proof.exitCode,
		}
	}
	return nil
}

func (t *windowsNativePackageTransaction) executePinnedDriverHelper(
	arguments []string,
) (string, int, error) {
	if t.releaseMutex == nil || t.helperHandle == 0 {
		return "", 0, errors.New("driver helper replay requires the held package mutex and pinned helper")
	}
	helperHash, err := hashNativePackageHandle(t.helperHandle)
	if err != nil {
		return "", 0, fmt.Errorf("rehash pinned driver helper: %w", err)
	}
	if !strings.EqualFold(helperHash, t.request.expectedHelperSHA256) {
		return "", 0, errors.New("pinned driver helper identity changed before journal replay")
	}
	// The helper owns its write-through journal transition. Its propagated
	// deadline is cooperative; killing it between FlushFileBuffers and readback
	// would manufacture the very indeterminate boundary this handshake closes.
	command := exec.Command(t.request.driverHelper, arguments...)
	command.Dir = filepath.Dir(t.request.driverHelper)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err = command.Run()
	processExitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return output.String(), 0, err
		}
		processExitCode = exitError.ExitCode()
	}
	return output.String(), processExitCode, err
}

func (t *windowsNativePackageTransaction) executeDriverBrokerSettlementAck(
	ctx context.Context,
	prepared nativeBrokerOuterSettlementPrepared,
) (nativePackageBrokerSettlementReceipt, error) {
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) {
		return nativePackageBrokerSettlementReceipt{}, context.DeadlineExceeded
	}
	output, processExitCode, processErr := t.executePinnedDriverHelper([]string{
		"broker-settlement-ack",
		"--request", prepared.RequestPath,
		"--request-sha256", prepared.RequestSHA256,
		"--transaction-deadline-unix-ms", strconv.FormatInt(deadline.UnixMilli(), 10),
	})
	if processErr != nil && processExitCode == 0 {
		return nativePackageBrokerSettlementReceipt{}, fmt.Errorf(
			"run driver settlement acknowledgement: %w", processErr,
		)
	}
	receipt, err := parseNativePackageBrokerSettlementReceipt(output, processExitCode)
	if err != nil {
		return nativePackageBrokerSettlementReceipt{}, fmt.Errorf(
			"validate driver settlement acknowledgement: %w: %s", err, output,
		)
	}
	if err := validateNativeBrokerOuterSettlementReceipt(prepared, receipt); err != nil {
		return nativePackageBrokerSettlementReceipt{}, err
	}
	return receipt, nil
}

func (t *windowsNativePackageTransaction) discardDriverBrokerSettlementTombstone(
	ctx context.Context,
	prepared nativeBrokerOuterSettlementPrepared,
	finalReceipt nativeBrokerOuterSettlementFinalPrepared,
	receipt nativePackageBrokerSettlementReceipt,
	brokerProof nativeBrokerJournalProof,
) error {
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) {
		return context.DeadlineExceeded
	}
	output, processExitCode, processErr := t.executePinnedDriverHelper([]string{
		"broker-settlement-discard",
		"--broker-transaction-id", brokerProof.TransactionID,
		"--broker-settled-digest", brokerProof.Digest,
		"--driver-transaction-id", receipt.DriverTransactionID,
		"--driver-settled-digest", receipt.Digest,
		"--settlement-nonce", receipt.SettlementNonce,
		"--request-sha256", receipt.RequestSHA256,
		"--broker-final-receipt", finalReceipt.ReceiptPath,
		"--broker-final-receipt-sha256", finalReceipt.ReceiptSHA256,
		"--transaction-deadline-unix-ms", strconv.FormatInt(deadline.UnixMilli(), 10),
	})
	if processErr != nil && processExitCode == 0 {
		return fmt.Errorf("discard settled driver journal tombstone: %w", processErr)
	}
	discard, err := parseNativePackageBrokerSettlementDiscardReceipt(output, processExitCode)
	if err != nil {
		return fmt.Errorf("validate settled driver journal discard receipt: %w: %s", err, output)
	}
	if discard.BrokerTransactionID != brokerProof.TransactionID ||
		discard.BrokerDigest != brokerProof.Digest ||
		discard.DriverTransactionID != receipt.DriverTransactionID ||
		discard.DriverDigest != receipt.Digest ||
		discard.SettlementNonce != receipt.SettlementNonce ||
		discard.RequestSHA256 != receipt.RequestSHA256 {
		return errors.New("settled driver journal discard receipt mismatched the exact two-phase transaction")
	}
	if discard.Retained {
		t.logger.Warn("Retaining inert driver settlement cleanup tombstone",
			"driverTransactionId", discard.DriverTransactionID,
			"brokerTransactionId", discard.BrokerTransactionID)
	}
	return nil
}

type nativePackageDriverCoordination struct {
	quiesceRequest windows.Handle
	quiesceReady   windows.Handle
	quiesceAbort   windows.Handle
	brokerHandoff  windows.Handle
}

func newNativePackageDriverCoordination() (*nativePackageDriverCoordination, error) {
	attributes := &windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	coordination := &nativePackageDriverCoordination{}
	create := func(target *windows.Handle, name string) error {
		handle, err := windows.CreateEvent(attributes, 1, 0, nil)
		if err != nil {
			return fmt.Errorf("create inherited %s event: %w", name, err)
		}
		if handle == 0 {
			return fmt.Errorf("create inherited %s event returned a null handle", name)
		}
		*target = handle
		return nil
	}
	if err := create(&coordination.quiesceRequest, "broker quiesce request"); err != nil {
		coordination.close()
		return nil, err
	}
	if err := create(&coordination.quiesceReady, "broker quiesce ready"); err != nil {
		coordination.close()
		return nil, err
	}
	if err := create(&coordination.quiesceAbort, "broker quiesce abort"); err != nil {
		coordination.close()
		return nil, err
	}
	if err := create(&coordination.brokerHandoff, "broker handoff"); err != nil {
		coordination.close()
		return nil, err
	}
	return coordination, nil
}

func (c *nativePackageDriverCoordination) close() {
	if c == nil {
		return
	}
	for _, handle := range []windows.Handle{
		c.brokerHandoff, c.quiesceAbort, c.quiesceReady, c.quiesceRequest,
	} {
		if handle != 0 {
			windows.CloseHandle(handle) //nolint:errcheck
		}
	}
	c.quiesceRequest = 0
	c.quiesceReady = 0
	c.quiesceAbort = 0
	c.brokerHandoff = 0
}

func (c *nativePackageDriverCoordination) inheritedHandles() []syscall.Handle {
	return []syscall.Handle{
		syscall.Handle(c.quiesceRequest), syscall.Handle(c.quiesceReady),
		syscall.Handle(c.quiesceAbort), syscall.Handle(c.brokerHandoff),
	}
}

func (c *nativePackageDriverCoordination) arguments() []string {
	return []string{
		"--broker-quiesce-request-handle", strconv.FormatUint(uint64(c.quiesceRequest), 10),
		"--broker-quiesce-ready-handle", strconv.FormatUint(uint64(c.quiesceReady), 10),
		"--broker-quiesce-abort-handle", strconv.FormatUint(uint64(c.quiesceAbort), 10),
		"--broker-handoff-handle", strconv.FormatUint(uint64(c.brokerHandoff), 10),
	}
}

func (t *windowsNativePackageTransaction) coordinateDriverHelper(
	ctx context.Context,
	process windows.Handle,
	coordination *nativePackageDriverCoordination,
) error {
	requestPending := true
	handoffPending := true
	for {
		handles := []windows.Handle{process}
		requestIndex := -1
		handoffIndex := -1
		if requestPending {
			requestIndex = len(handles)
			handles = append(handles, coordination.quiesceRequest)
		}
		if handoffPending {
			handoffIndex = len(handles)
			handles = append(handles, coordination.brokerHandoff)
		}
		status, err := windows.WaitForMultipleObjects(handles, false, windows.INFINITE)
		if err != nil {
			windows.SetEvent(coordination.quiesceAbort) //nolint:errcheck
			return fmt.Errorf("wait for driver-helper coordination event: %w", err)
		}
		index := int(status - windows.WAIT_OBJECT_0)
		switch index {
		case 0:
			return nil
		case requestIndex:
			requestPending = false
			t.driverQuiesceRequested = true
			if quiesceErr := t.quiescePriorServiceForDriver(ctx); quiesceErr != nil {
				t.driverCoordinationErr = quiesceErr
				if signalErr := windows.SetEvent(coordination.quiesceAbort); signalErr != nil {
					return errors.Join(quiesceErr,
						fmt.Errorf("signal broker quiescence abort: %w", signalErr))
				}
				continue
			}
			if signalErr := windows.SetEvent(coordination.quiesceReady); signalErr != nil {
				windows.SetEvent(coordination.quiesceAbort) //nolint:errcheck
				return fmt.Errorf("signal broker quiescence readiness: %w", signalErr)
			}
		case handoffIndex:
			handoffPending = false
			if t.driverCoordinationErr != nil {
				return errors.New("driver helper requested broker handoff after quiescence was aborted")
			}
			if handoffErr := t.releaseServiceForBrokerHandoff(); handoffErr != nil {
				return handoffErr
			}
		default:
			windows.SetEvent(coordination.quiesceAbort) //nolint:errcheck
			return fmt.Errorf("unexpected driver-helper coordination wait status 0x%08x", status)
		}
	}
}

func (t *windowsNativePackageTransaction) quiescePriorServiceForDriver(ctx context.Context) error {
	if t.releaseServiceMutex == nil {
		return errors.New("broker quiescence requires the held service mutex")
	}
	switch t.serviceSnapshot.disposition {
	case nativePackageServiceAbsent:
		return nil
	case nativePackageServiceWeakExactOwned:
		// A noncanonical exact-owned service is not rollback material. Remove it
		// under the held service mutex before the helper mutates the root bus; the
		// nested broker transaction will recreate the canonical service only after
		// the driver has reached its protected handoff point.
		return t.removeWeakExactOwnedService(ctx)
	case nativePackageServiceTrusted:
		if t.service == nil {
			return errors.New("trusted broker service snapshot has no live SCM handle")
		}
		if !t.serviceSnapshot.wasRunning {
			return nil
		}
		// STOP is the parent transaction's mutation. Arm restoration before the
		// control request so StopPending/timeouts cannot strand the prior broker.
		t.stoppedTrustedService = true
		if err := stopNativeService(ctx, t.service, waitContext); err != nil {
			return fmt.Errorf("quiesce trusted %s before root-bus mutation: %w",
				NativeBrokerServiceName, err)
		}
		return nil
	default:
		return errors.New("broker quiescence received an unknown service disposition")
	}
}

func (t *windowsNativePackageTransaction) removeWeakExactOwnedService(ctx context.Context) error {
	if t.releaseServiceMutex == nil {
		return errors.New("weak broker removal requires the held service mutex")
	}
	if t.serviceSnapshot.disposition != nativePackageServiceWeakExactOwned {
		return errors.New("weak broker removal received a non-weak service snapshot")
	}
	if t.weakServiceRemoved {
		return nil
	}
	if t.service == nil {
		return errors.New("weak exact-owned broker snapshot has no live SCM handle")
	}
	if t.manager == nil {
		return errors.New("weak exact-owned broker snapshot has no live SCM manager")
	}

	// The snapshot is deliberately excluded from rollback trust. Arm the
	// fail-closed state before the first STOP/DELETE mutation: a partial failure
	// must never restart or restore an image/configuration that was not proven.
	t.weakServiceMutation = true
	if t.serviceSnapshot.wasRunning {
		if err := stopNativeService(ctx, t.service, waitContext); err != nil {
			return fmt.Errorf("stop weak exact-owned %s: %w", NativeBrokerServiceName, err)
		}
	}
	if err := t.service.Delete(); err != nil &&
		!errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return fmt.Errorf("delete weak exact-owned %s: %w", NativeBrokerServiceName, err)
	}
	if err := t.service.Close(); err != nil {
		return fmt.Errorf("close weak exact-owned %s after deletion: %w", NativeBrokerServiceName, err)
	}
	t.service = nil
	if err := waitForNativePackageServiceDeletion(ctx, t.manager); err != nil {
		return err
	}
	t.weakServiceRemoved = true
	return nil
}

func (t *windowsNativePackageTransaction) releaseServiceForBrokerHandoff() error {
	if t.releaseServiceMutex == nil {
		return errors.New("broker handoff requires the held service mutex")
	}
	if t.priorExecutableRelease != nil {
		t.priorExecutableRelease()
		t.priorExecutableRelease = nil
	}
	if t.service != nil {
		if err := t.service.Close(); err != nil {
			return fmt.Errorf("close prior broker service handle before handoff: %w", err)
		}
		t.service = nil
	}
	if t.manager != nil {
		if err := t.manager.Close(); err != nil {
			return fmt.Errorf("close prior SCM handle before handoff: %w", err)
		}
		t.manager = nil
	}
	t.releaseServiceMutex()
	t.releaseServiceMutex = nil
	t.driverBrokerHandoff = true
	return nil
}

func (t *windowsNativePackageTransaction) ensurePriorServiceForRestore(
	ctx context.Context,
) error {
	if t.releaseServiceMutex == nil {
		budget := nativePackageRollbackTimeout
		if deadline, ok := ctx.Deadline(); ok {
			budget = time.Until(deadline)
			if budget <= 0 {
				return context.DeadlineExceeded
			}
		}
		release, err := acquireNativeInstallMutex(budget)
		if err != nil {
			return fmt.Errorf("reacquire native broker service mutex for rollback: %w", err)
		}
		t.releaseServiceMutex = release
	}
	if t.manager == nil {
		manager, err := mgr.Connect()
		if err != nil {
			return fmt.Errorf("reconnect to SCM for broker rollback: %w", err)
		}
		t.manager = &windowsNativeSCM{manager: manager}
	}
	if t.service == nil {
		service, err := t.manager.OpenService(NativeBrokerServiceName)
		if err != nil {
			return fmt.Errorf("reopen prior %s for rollback: %w", NativeBrokerServiceName, err)
		}
		t.service = service
	}
	return nil
}

func (t *windowsNativePackageTransaction) validatePriorServiceForRestart() error {
	if t.serviceSnapshot.disposition != nativePackageServiceTrusted ||
		t.priorServiceExecutable == "" || t.priorExecutableSHA256 == "" {
		return errors.New("prior broker snapshot is not a trusted restart source")
	}
	config, err := t.service.Config()
	if err != nil {
		return fmt.Errorf("query prior broker config for restart: %w", err)
	}
	if !nativeServiceConfigsEqual(config, t.priorServiceConfig) {
		return errors.New("prior broker config changed before rollback restart")
	}
	executable, err := nativeServiceExecutableFromCommandLine(config.BinaryPathName)
	if err != nil || !strings.EqualFold(executable, t.priorServiceExecutable) {
		return errors.New("prior broker executable changed before rollback restart")
	}
	dacl, err := t.service.SecurityDescriptor()
	if err != nil {
		return fmt.Errorf("query prior broker DACL for restart: %w", err)
	}
	if compareNativeSecurityDescriptorStrings(dacl, t.priorServiceDACL) != nil {
		return errors.New("prior broker DACL changed before rollback restart")
	}
	recovery, err := t.service.RecoveryActions()
	if err != nil {
		return fmt.Errorf("query prior broker recovery actions for restart: %w", err)
	}
	reset, err := t.service.ResetPeriod()
	if err != nil {
		return fmt.Errorf("query prior broker recovery reset for restart: %w", err)
	}
	nonCrash, err := t.service.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return fmt.Errorf("query prior broker recovery mode for restart: %w", err)
	}
	if !slices.Equal(recovery, t.priorServiceRecovery) || reset != t.priorServiceReset ||
		nonCrash != t.priorServiceNonCrash {
		return errors.New("prior broker recovery policy changed before rollback restart")
	}
	if t.priorExecutableRelease == nil {
		release, lockErr := lockNativePriorServiceExecutable(t.priorServiceExecutable)
		if lockErr != nil {
			return fmt.Errorf("relock protected prior broker executable: %w", lockErr)
		}
		t.priorExecutableRelease = release
	}
	handle, err := lockNativePackageInput(t.priorServiceExecutable)
	if err != nil {
		return fmt.Errorf("reopen protected prior broker executable: %w", err)
	}
	hash, hashErr := hashNativePackageHandle(handle)
	closeErr := windows.CloseHandle(handle)
	if hashErr != nil || closeErr != nil {
		return fmt.Errorf("rehash protected prior broker executable: %w",
			errors.Join(hashErr, closeErr))
	}
	if !strings.EqualFold(hash, t.priorExecutableSHA256) {
		return fmt.Errorf("prior broker executable SHA-256=%s expected=%s",
			hash, t.priorExecutableSHA256)
	}
	return nil
}

func (t *windowsNativePackageTransaction) restoreQuiescedPriorService(ctx context.Context) error {
	if !t.stoppedTrustedService || !t.serviceSnapshot.wasRunning {
		return nil
	}
	if err := t.ensurePriorServiceForRestore(ctx); err != nil {
		return err
	}
	if err := t.validatePriorServiceForRestart(); err != nil {
		return err
	}
	if err := reconcileNativePackageServiceRunning(ctx, t.service); err != nil {
		return fmt.Errorf("restore prior trusted %s run state: %w", NativeBrokerServiceName, err)
	}
	if err := t.validatePriorServiceForRestart(); err != nil {
		return fmt.Errorf("revalidate restarted prior broker: %w", err)
	}
	t.stoppedTrustedService = false
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
	coordination, err := newNativePackageDriverCoordination()
	if err != nil {
		return "", err
	}
	defer coordination.close()
	arguments = append(arguments, coordination.arguments()...)
	// Do not use CommandContext: killing ViiperUdeCtl could interrupt its in-memory
	// DriverStore rollback or the broker's deferred SCM/credential rollback.
	command := exec.Command(t.request.driverHelper, arguments...)
	command.Dir = filepath.Dir(t.request.driverHelper)
	command.SysProcAttr = &syscall.SysProcAttr{
		AdditionalInheritedHandles: coordination.inheritedHandles(),
	}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return "", err
	}
	// The helper owns the driver snapshot and nested broker rollback. Its
	// propagated absolute deadline is cooperative; never terminate it here.
	err = waitNativePackageHelperCoordinated(command, func(process windows.Handle) error {
		return t.coordinateDriverHelper(ctx, process, coordination)
	})
	// Preserve exact record framing. The authenticated journal binding is valid
	// only as one canonical newline-terminated line; trimming helper output here
	// would erase that boundary before the strict parser can verify it.
	return output.String(), err
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
	fail := func(owned windows.Handle, failErr error) error {
		if owned != 0 {
			windows.CloseHandle(owned) //nolint:errcheck
		}
		_ = deleteNativePackageFile(path)
		return failErr
	}
	var written uint32
	if err := windows.WriteFile(handle, content, &written, nil); err != nil {
		return fail(handle, err)
	}
	if written != uint32(len(content)) {
		return fail(handle, io.ErrShortWrite)
	}
	if err := windows.FlushFileBuffers(handle); err != nil {
		return fail(handle, err)
	}
	if err := validateNativeSecurityDescriptor(handle, nativePackageTokenSDDL); err != nil {
		return fail(handle, err)
	}
	if err := requireSingleNativeFileLink(handle); err != nil {
		return fail(handle, err)
	}
	sum := sha256.Sum256(content)
	// Seal the token before another process opens it. Windows share checks are
	// symmetric: a new read-only open that specifies FILE_SHARE_READ still
	// conflicts with this handle's existing GENERIC_WRITE access. Close the
	// write-capable handle, reopen through the ordinary immutable-input path,
	// and revalidate the exact bytes before publishing the path to the helper.
	// The protected parent and token DACL exclude the unelevated race boundary;
	// the retained read handle then prevents replacement for the transaction.
	if err := windows.CloseHandle(handle); err != nil {
		return fail(handle, fmt.Errorf("seal protected package transaction token: %w", err))
	}
	handle = 0
	sealed, err := lockNativePackageInput(path)
	if err != nil {
		return fail(0, fmt.Errorf("reopen sealed package transaction token: %w", err))
	}
	if err := validateNativeSecurityDescriptor(sealed, nativePackageTokenSDDL); err != nil {
		return fail(sealed, fmt.Errorf("revalidate sealed package transaction token ACL: %w", err))
	}
	sealedHash, err := hashNativePackageHandle(sealed)
	if err != nil {
		return fail(sealed, fmt.Errorf("rehash sealed package transaction token: %w", err))
	}
	expectedHash := hex.EncodeToString(sum[:])
	if !strings.EqualFold(sealedHash, expectedHash) {
		return fail(sealed, errors.New("sealed package transaction token changed during publication"))
	}
	t.tokenPath = path
	t.tokenSHA256 = expectedHash
	t.tokenHandle = sealed
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
	priorExists := false
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
		priorExists = true
	} else if !errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) &&
		!errors.Is(openErr, windows.ERROR_PATH_NOT_FOUND) {
		return fmt.Errorf("inspect existing broker: %w", openErr)
	}

	if t.brokerJournal != nil {
		t.temporaryPath = filepath.Join(
			t.parent, ".viiper.staging."+t.brokerJournal.snapshot.TransactionID+".tmp",
		)
	} else {
		t.temporaryPath, err = t.uniqueManagedPath("staging")
		if err != nil {
			return err
		}
	}
	if err := copyNativePackageHandleAtomically(
		t.sourceHandle, t.temporaryPath, t.request.expectedBrokerSHA256,
	); err != nil {
		return err
	}
	if t.brokerJournal != nil {
		if err := t.brokerJournal.appendPhase(
			nativeBrokerPhaseImageSwitchIntent, t.request.expectedBrokerSHA256,
		); err != nil {
			return fmt.Errorf("journal broker image switch intent: %w", err)
		}
	}
	if err := replaceNativePackageFileAtomically(t.temporaryPath, t.destination, priorExists); err != nil {
		return fmt.Errorf("publish staged broker: %w", err)
	}
	t.temporaryPath = ""
	t.destinationPublished = true
	if t.brokerJournal != nil {
		if err := t.brokerJournal.appendPhase(
			nativeBrokerPhaseImageSwitched, t.request.expectedBrokerSHA256,
		); err != nil {
			return fmt.Errorf("journal published broker image: %w", err)
		}
	}
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
	if t.destinationPublished && t.brokerJournal != nil {
		if err := restoreNativeBrokerJournalImage(t.brokerJournal); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore durable prior broker image: %w", err))
		} else {
			t.destinationPublished = false
		}
	} else if t.destinationPublished {
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

var replaceNativePackageFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func replaceNativePackageFileAtomically(source, destination string, destinationExists bool) error {
	if !destinationExists {
		return moveNativePackageFile(source, destination, false)
	}
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	result, _, callErr := replaceNativePackageFileW.Call(
		uintptr(unsafe.Pointer(destinationPointer)),
		uintptr(unsafe.Pointer(sourcePointer)),
		0,
		1, // REPLACEFILE_WRITE_THROUGH
		0,
		0,
	)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = errors.New("ReplaceFileW returned false")
		}
		return callErr
	}
	return nil
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
