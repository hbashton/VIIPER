package cmd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var nativePackageHexRevision = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
var nativePackageSHA256 = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
var nativePackageInstallProofPattern = regexp.MustCompile(
	`(?m)^result=(success|error) operation=install changed=([01]) rebootRequired=([01]) rollback=(not-needed|succeeded|failed) exitCode=([0-9]+)(?: .*)?\r?$`,
)
var nativePackageInstallJournalBindingPattern = regexp.MustCompile(
	`(?m)^journal-binding operation=install transactionId=([0-9a-f]{32}) outerTransactionId=([0-9a-f]{64}) candidateSha256=([0-9a-f]{64}) state=(nested-ready) digest=([0-9a-f]{64}) driverTransactionId=([0-9a-f]{64}) driverDigest=([0-9a-f]{64}) settlementNonce=([0-9a-f]{64}) recovery=(fresh|replayed)\r?$`,
)
var nativePackageBrokerSettlementReceiptPattern = regexp.MustCompile(
	`(?m)^journal-settlement operation=broker-settlement-ack brokerTransactionId=([0-9a-f]{32}) brokerPendingDigest=([0-9a-f]{64}) driverTransactionId=([0-9a-f]{64}) driverPendingDigest=([0-9a-f]{64}) settlementNonce=([0-9a-f]{64}) requestSha256=([0-9a-f]{64}) state=(outer-settled) digest=([0-9a-f]{64})\r?$`,
)
var nativePackageBrokerSettlementDiscardPattern = regexp.MustCompile(
	`(?m)^journal-discard operation=broker-settlement-discard brokerTransactionId=([0-9a-f]{32}) brokerDigest=([0-9a-f]{64}) driverTransactionId=([0-9a-f]{64}) driverDigest=([0-9a-f]{64}) settlementNonce=([0-9a-f]{64}) requestSha256=([0-9a-f]{64}) discarded=([01]) retained=([01])\r?$`,
)

const (
	nativePackageTransactionTimeout = 4 * time.Minute
	nativePackageRollbackTimeout    = 2 * time.Minute
	nativePackageRebootRequiredCode = 3010
)

type nativePackageRebootRequiredError struct {
	cause error
}

type nativePackageRecoveryRetryError struct{}

func (*nativePackageRecoveryRetryError) Error() string {
	return "a prior native package transaction was recovered and settled; retry the requested package transaction"
}

func (e *nativePackageRebootRequiredError) Error() string {
	return "native package activation requires a restart after safe rollback: " + e.cause.Error()
}

func (e *nativePackageRebootRequiredError) Unwrap() error { return e.cause }

// ExitCode lets Kong preserve Windows' ERROR_SUCCESS_REBOOT_REQUIRED contract
// for the signed installer instead of flattening the reconciled state to 1.
func (e *nativePackageRebootRequiredError) ExitCode() int {
	return nativePackageRebootRequiredCode
}

// NativePackageInstall is the narrow bootstrapper boundary for the native UDE
// package. Production is the default and normal users enter through the signed
// DS4Windows installer. The explicit local-test route retains the same hashes,
// rollback, service, and authenticated health transaction for disposable
// TESTSIGNING machines without relaxing the production route.
type NativePackageInstall struct {
	PackageDirectory       string `help:"Directory containing the exact Microsoft-returned INF, SYS, and CAT runtime files." required:""`
	SubmissionManifest     string `help:"Source-bound HLK/WHCP submission manifest." required:""`
	SourceRevision         string `help:"Reviewed 40- or 64-character source revision." required:""`
	DriverHelper           string `help:"Path to the packaged ViiperUdeCtl.exe." required:""`
	ExpectedBrokerSHA256   string `help:"Installer-embedded SHA-256 of this VIIPER executable." required:""`
	ExpectedHelperSHA256   string `help:"Installer-embedded SHA-256 of ViiperUdeCtl.exe." required:""`
	ExpectedManifestSHA256 string `help:"Installer-embedded SHA-256 of the reviewed HLK/WHCP manifest." required:""`
	ExpectedInfSHA256      string `help:"Installer-embedded SHA-256 of the Microsoft-returned ViiperUde.inf." required:""`
	ExpectedSysSHA256      string `help:"Installer-embedded SHA-256 of the Microsoft-returned ViiperUde.sys." required:""`
	ExpectedCatSHA256      string `help:"Installer-embedded SHA-256 of the Microsoft-returned ViiperUde.cat." required:""`
	TargetUserSID          string `help:"Interactive Windows user SID that owns legacy startup state." required:""`
	DriverValidationMode   string `help:"Driver signature route: production or local-test." default:"production" enum:"production,local-test" hidden:""`
}

// NativePackageBrokerCommit is invoked only by ViiperUdeCtl while the signed
// outer package transaction holds its machine mutex and protected token.
type NativePackageBrokerCommit struct {
	TokenFile                 string `help:"Protected package-transaction token path." required:""`
	ExpectedTokenSHA256       string `help:"SHA-256 of the protected transaction token." required:""`
	ExpectedBrokerSHA256      string `help:"Installer-bound SHA-256 of the broker being committed." required:""`
	TargetUserSID             string `help:"Interactive Windows user SID that owns legacy startup state." required:""`
	TransactionDeadlineUnixMS string `help:"Outer package transaction deadline as Unix milliseconds." required:""`
	RecoveryOnly              bool   `help:"Replay or reconcile only the exact durable child transaction; never start a new one." hidden:""`
}

type nativePackageBrokerCommitResult struct {
	success  bool
	changed  bool
	rollback string
	exitCode int
	journal  nativeBrokerJournalProof
}

type nativeBrokerJournalProof struct {
	TransactionID      string
	OuterTransactionID string
	CandidateSHA256    string
	State              string
	Digest             string
}

type nativePackageInstallProof struct {
	success             bool
	changed             bool
	rebootRequired      bool
	rollback            string
	exitCode            int
	journal             nativeBrokerJournalProof
	driverTransactionID string
	driverPendingDigest string
	settlementNonce     string
	journalRecovery     string
}

type nativePackageBrokerSettlementReceipt struct {
	BrokerTransactionID string `json:"brokerTransactionId"`
	BrokerPendingDigest string `json:"brokerPendingDigest"`
	DriverTransactionID string `json:"driverTransactionId"`
	DriverPendingDigest string `json:"driverPendingDigest"`
	SettlementNonce     string `json:"settlementNonce"`
	RequestSHA256       string `json:"requestSha256"`
	State               string `json:"state"`
	Digest              string `json:"digest"`
}

type nativePackageBrokerSettlementDiscardReceipt struct {
	BrokerTransactionID string
	BrokerDigest        string
	DriverTransactionID string
	DriverDigest        string
	SettlementNonce     string
	RequestSHA256       string
	Discarded           bool
	Retained            bool
}

func parseNativePackageInstallProof(output string, processExitCode int) (nativePackageInstallProof, error) {
	matches := nativePackageInstallProofPattern.FindAllStringSubmatch(output, -1)
	if len(matches) != 1 {
		return nativePackageInstallProof{}, errors.New("driver helper did not emit exactly one structured install outcome")
	}
	proofExitCode, err := strconv.Atoi(matches[0][5])
	if err != nil {
		return nativePackageInstallProof{}, fmt.Errorf("parse driver helper install exit code: %w", err)
	}
	proof := nativePackageInstallProof{
		success:        matches[0][1] == "success",
		changed:        matches[0][2] == "1",
		rebootRequired: matches[0][3] == "1",
		rollback:       matches[0][4],
		exitCode:       proofExitCode,
	}
	journalBindingSeen := false
	for cursor := 0; cursor < len(output); {
		lineEnd := strings.IndexByte(output[cursor:], '\n')
		terminated := lineEnd >= 0
		if terminated {
			lineEnd += cursor
		} else {
			lineEnd = len(output)
		}
		line := output[cursor:lineEnd]
		if strings.HasPrefix(line, "journal-binding") {
			journalBinding := nativePackageInstallJournalBindingPattern.FindStringSubmatch(line)
			if !terminated || len(journalBinding) != 10 || journalBinding[0] != line {
				return nativePackageInstallProof{}, errors.New(
					"driver helper emitted a noncanonical broker journal binding",
				)
			}
			if journalBindingSeen {
				return nativePackageInstallProof{}, errors.New("driver helper emitted multiple broker journal bindings")
			}
			journalBindingSeen = true
			proof.journal = nativeBrokerJournalProof{
				TransactionID:      journalBinding[1],
				OuterTransactionID: journalBinding[2],
				CandidateSHA256:    journalBinding[3],
				State:              journalBinding[4],
				Digest:             journalBinding[5],
			}
			proof.driverTransactionID = journalBinding[6]
			proof.driverPendingDigest = journalBinding[7]
			proof.settlementNonce = journalBinding[8]
			proof.journalRecovery = journalBinding[9]
		}
		if !terminated {
			break
		}
		cursor = lineEnd + 1
	}
	if proof.exitCode != processExitCode {
		return nativePackageInstallProof{}, fmt.Errorf(
			"driver helper install process exit %d disagreed with structured exit %d",
			processExitCode, proof.exitCode,
		)
	}
	switch proof.exitCode {
	case 0:
		if !proof.success || proof.rebootRequired || proof.rollback != "not-needed" {
			return nativePackageInstallProof{}, errors.New("driver helper emitted an invalid success install outcome")
		}
	case nativePackageRebootRequiredCode:
		settledBeforeMutation := !proof.changed && proof.rollback == "not-needed"
		settledAfterRollback := proof.changed && proof.rollback == "succeeded"
		if proof.success || !proof.rebootRequired ||
			(!settledBeforeMutation && !settledAfterRollback) {
			return nativePackageInstallProof{}, errors.New("driver helper emitted an invalid reboot-boundary install outcome")
		}
	case 4:
		if proof.success || proof.changed || proof.rebootRequired || proof.rollback != "not-needed" {
			return nativePackageInstallProof{}, errors.New("driver helper emitted an invalid preflight install outcome")
		}
	case 1:
		settledMutation := proof.changed && proof.rollback == "succeeded"
		preMutationFailure := !proof.changed && proof.rollback == "not-needed"
		if proof.success || (!settledMutation && !preMutationFailure) {
			return nativePackageInstallProof{}, errors.New("driver helper emitted an invalid failed install outcome")
		}
	case 3:
		if proof.success || !proof.changed || proof.rollback != "failed" {
			return nativePackageInstallProof{}, errors.New("driver helper emitted an invalid indeterminate install outcome")
		}
	default:
		return nativePackageInstallProof{}, fmt.Errorf(
			"driver helper returned unsupported structured install exit %d", proof.exitCode,
		)
	}
	if journalBindingSeen && (!proof.success || !proof.changed || proof.exitCode != 0) {
		return nativePackageInstallProof{}, errors.New(
			"driver helper emitted a broker journal binding for a non-forward-success outcome",
		)
	}
	return proof, nil
}

func parseNativePackageBrokerSettlementReceipt(
	output string,
	processExitCode int,
) (nativePackageBrokerSettlementReceipt, error) {
	if processExitCode != 0 {
		return nativePackageBrokerSettlementReceipt{}, fmt.Errorf(
			"driver helper settlement acknowledgement exited with %d", processExitCode,
		)
	}
	var receipt nativePackageBrokerSettlementReceipt
	seen := false
	for cursor := 0; cursor < len(output); {
		lineEnd := strings.IndexByte(output[cursor:], '\n')
		terminated := lineEnd >= 0
		if terminated {
			lineEnd += cursor
		} else {
			lineEnd = len(output)
		}
		line := output[cursor:lineEnd]
		if strings.HasPrefix(line, "journal-settlement") {
			match := nativePackageBrokerSettlementReceiptPattern.FindStringSubmatch(line)
			if !terminated || len(match) != 9 || match[0] != line {
				return nativePackageBrokerSettlementReceipt{}, errors.New(
					"driver helper emitted a noncanonical broker settlement acknowledgement",
				)
			}
			if seen {
				return nativePackageBrokerSettlementReceipt{}, errors.New(
					"driver helper emitted multiple broker settlement acknowledgements",
				)
			}
			seen = true
			receipt = nativePackageBrokerSettlementReceipt{
				BrokerTransactionID: match[1],
				BrokerPendingDigest: match[2],
				DriverTransactionID: match[3],
				DriverPendingDigest: match[4],
				SettlementNonce:     match[5],
				RequestSHA256:       match[6],
				State:               match[7],
				Digest:              match[8],
			}
		}
		if !terminated {
			break
		}
		cursor = lineEnd + 1
	}
	if !seen {
		return nativePackageBrokerSettlementReceipt{}, errors.New(
			"driver helper emitted no broker settlement acknowledgement",
		)
	}
	return receipt, nil
}

func parseNativePackageBrokerSettlementDiscardReceipt(
	output string,
	processExitCode int,
) (nativePackageBrokerSettlementDiscardReceipt, error) {
	if processExitCode != 0 {
		return nativePackageBrokerSettlementDiscardReceipt{}, fmt.Errorf(
			"driver helper settled-tombstone discard exited with %d", processExitCode,
		)
	}
	var receipt nativePackageBrokerSettlementDiscardReceipt
	seen := false
	for cursor := 0; cursor < len(output); {
		lineEnd := strings.IndexByte(output[cursor:], '\n')
		terminated := lineEnd >= 0
		if terminated {
			lineEnd += cursor
		} else {
			lineEnd = len(output)
		}
		line := output[cursor:lineEnd]
		if strings.HasPrefix(line, "journal-discard") {
			match := nativePackageBrokerSettlementDiscardPattern.FindStringSubmatch(line)
			if !terminated || len(match) != 9 || match[0] != line {
				return nativePackageBrokerSettlementDiscardReceipt{}, errors.New(
					"driver helper emitted a noncanonical settled-tombstone discard receipt",
				)
			}
			if seen {
				return nativePackageBrokerSettlementDiscardReceipt{}, errors.New(
					"driver helper emitted multiple settled-tombstone discard receipts",
				)
			}
			seen = true
			receipt = nativePackageBrokerSettlementDiscardReceipt{
				BrokerTransactionID: match[1],
				BrokerDigest:        match[2],
				DriverTransactionID: match[3],
				DriverDigest:        match[4],
				SettlementNonce:     match[5],
				RequestSHA256:       match[6],
				Discarded:           match[7] == "1",
				Retained:            match[8] == "1",
			}
		}
		if !terminated {
			break
		}
		cursor = lineEnd + 1
	}
	if !seen {
		return nativePackageBrokerSettlementDiscardReceipt{}, errors.New(
			"driver helper emitted no settled-tombstone discard receipt",
		)
	}
	return receipt, nil
}

func (r nativePackageBrokerCommitResult) proofLine() string {
	status := "error"
	if r.success {
		status = "success"
	}
	changed := 0
	if r.changed {
		changed = 1
	}
	return fmt.Sprintf(
		"result=%s operation=native-package-broker-commit changed=%d rollback=%s exitCode=%d\n",
		status, changed, r.rollback, r.exitCode,
	)
}

func (r nativePackageBrokerCommitResult) journalProofLine() string {
	if len(r.journal.TransactionID) != 32 ||
		!nativePackageSHA256.MatchString(r.journal.OuterTransactionID) ||
		!nativePackageSHA256.MatchString(r.journal.CandidateSHA256) ||
		!nativePackageSHA256.MatchString(r.journal.Digest) ||
		r.journal.TransactionID != strings.ToLower(r.journal.TransactionID) ||
		r.journal.OuterTransactionID != strings.ToLower(r.journal.OuterTransactionID) ||
		r.journal.CandidateSHA256 != strings.ToLower(r.journal.CandidateSHA256) ||
		r.journal.Digest != strings.ToLower(r.journal.Digest) ||
		(r.journal.State != "nested-ready" && r.journal.State != "rollback-settled" &&
			r.journal.State != "manual") {
		return ""
	}
	if _, err := hex.DecodeString(r.journal.TransactionID); err != nil {
		return ""
	}
	return fmt.Sprintf(
		"journal-proof operation=native-package-broker-commit transactionId=%s outerTransactionId=%s candidateSha256=%s state=%s digest=%s\n",
		r.journal.TransactionID, r.journal.OuterTransactionID, r.journal.CandidateSHA256,
		r.journal.State, r.journal.Digest,
	)
}

type nativePackageBrokerCommitError struct {
	cause    error
	exitCode int
}

func (e *nativePackageBrokerCommitError) Error() string { return e.cause.Error() }
func (e *nativePackageBrokerCommitError) Unwrap() error { return e.cause }
func (e *nativePackageBrokerCommitError) ExitCode() int { return e.exitCode }

func nativePackageBrokerPreflightFailure(err error) (nativePackageBrokerCommitResult, error) {
	return nativePackageBrokerCommitResult{rollback: "not-needed", exitCode: 4}, err
}

func (c *NativePackageBrokerCommit) Run(logger *slog.Logger) error {
	var result nativePackageBrokerCommitResult
	var err error
	if !nativePackageSHA256.MatchString(strings.TrimSpace(c.ExpectedTokenSHA256)) ||
		!nativePackageSHA256.MatchString(strings.TrimSpace(c.ExpectedBrokerSHA256)) {
		result, err = nativePackageBrokerPreflightFailure(
			errors.New("native package token and broker SHA-256 values must contain exactly 64 hexadecimal characters"),
		)
	} else {
		result, err = commitNativePackageBroker(logger, strings.TrimSpace(c.TokenFile),
			strings.ToLower(strings.TrimSpace(c.ExpectedTokenSHA256)),
			strings.ToLower(strings.TrimSpace(c.ExpectedBrokerSHA256)), strings.TrimSpace(c.TargetUserSID),
			strings.TrimSpace(c.TransactionDeadlineUnixMS), c.RecoveryOnly)
	}
	fmt.Fprint(os.Stdout, result.proofLine()+result.journalProofLine())
	if err != nil {
		return &nativePackageBrokerCommitError{cause: err, exitCode: result.exitCode}
	}
	return nil
}

func (c *NativePackageInstall) Run(logger *slog.Logger) error {
	executable, err := currentExecutable()
	if err != nil {
		return err
	}
	if strings.Contains(executable, "go-build") {
		return errors.New("cannot provision the native package from 'go run'")
	}
	request := nativePackageRequest{
		brokerSource:           executable,
		packageDirectory:       strings.TrimSpace(c.PackageDirectory),
		submissionManifest:     strings.TrimSpace(c.SubmissionManifest),
		sourceRevision:         strings.ToLower(strings.TrimSpace(c.SourceRevision)),
		driverHelper:           strings.TrimSpace(c.DriverHelper),
		expectedBrokerSHA256:   strings.ToLower(strings.TrimSpace(c.ExpectedBrokerSHA256)),
		expectedHelperSHA256:   strings.ToLower(strings.TrimSpace(c.ExpectedHelperSHA256)),
		expectedManifestSHA256: strings.ToLower(strings.TrimSpace(c.ExpectedManifestSHA256)),
		expectedInfSHA256:      strings.ToLower(strings.TrimSpace(c.ExpectedInfSHA256)),
		expectedSysSHA256:      strings.ToLower(strings.TrimSpace(c.ExpectedSysSHA256)),
		expectedCatSHA256:      strings.ToLower(strings.TrimSpace(c.ExpectedCatSHA256)),
		targetUserSID:          strings.TrimSpace(c.TargetUserSID),
		driverValidationMode:   strings.ToLower(strings.TrimSpace(c.DriverValidationMode)),
	}
	if err := request.validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativePackageTransactionTimeout)
	defer cancel()
	return installNativePackage(ctx, logger, request)
}

type nativePackageRequest struct {
	brokerSource           string
	packageDirectory       string
	submissionManifest     string
	sourceRevision         string
	driverHelper           string
	expectedBrokerSHA256   string
	expectedHelperSHA256   string
	expectedManifestSHA256 string
	expectedInfSHA256      string
	expectedSysSHA256      string
	expectedCatSHA256      string
	targetUserSID          string
	driverValidationMode   string
}

func (r nativePackageRequest) validate() error {
	for name, value := range map[string]string{
		"broker source": r.brokerSource, "driver package": r.packageDirectory,
		"submission manifest": r.submissionManifest, "driver helper": r.driverHelper,
		"target user SID": r.targetUserSID,
	} {
		if value == "" {
			return fmt.Errorf("native package %s is empty", name)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("native package %s contains NUL", name)
		}
	}
	if !nativePackageHexRevision.MatchString(r.sourceRevision) {
		return errors.New("native package source revision must contain exactly 40 or 64 hexadecimal characters")
	}
	if r.driverValidationMode != "production" && r.driverValidationMode != "local-test" {
		return errors.New("native package driver validation mode must be production or local-test")
	}
	if !nativePackageSHA256.MatchString(r.expectedBrokerSHA256) ||
		!nativePackageSHA256.MatchString(r.expectedHelperSHA256) ||
		!nativePackageSHA256.MatchString(r.expectedManifestSHA256) ||
		!nativePackageSHA256.MatchString(r.expectedInfSHA256) ||
		!nativePackageSHA256.MatchString(r.expectedSysSHA256) ||
		!nativePackageSHA256.MatchString(r.expectedCatSHA256) {
		return errors.New("native package broker, helper, manifest, INF, SYS, and CAT SHA-256 values must contain exactly 64 hexadecimal characters")
	}
	for name, path := range map[string]string{
		"broker source": r.brokerSource, "driver package": r.packageDirectory,
		"submission manifest": r.submissionManifest, "driver helper": r.driverHelper,
	} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("native package %s must be an absolute path: %s", name, path)
		}
	}
	return nil
}

type nativePackageServiceDisposition uint8

const (
	nativePackageServiceAbsent nativePackageServiceDisposition = iota
	nativePackageServiceTrusted
	nativePackageServiceWeakExactOwned
)

type nativePackageServiceSnapshot struct {
	disposition nativePackageServiceDisposition
	wasRunning  bool
	opaque      any
}

type nativePackageTransaction interface {
	Preflight(context.Context) error
	InspectService(context.Context) (nativePackageServiceSnapshot, error)
	Prepare(context.Context, nativePackageServiceSnapshot) error
	InstallDriverAndBroker(context.Context) error
	VerifyAuthenticatedHealth(context.Context) error
	Commit(context.Context) error
	Rollback(context.Context) error
	Close() error
}

func runNativePackageTransaction(
	ctx context.Context,
	logger *slog.Logger,
	transaction nativePackageTransaction,
) (resultErr error) {
	if transaction == nil {
		return errors.New("native package transaction is nil")
	}
	defer func() {
		if closeErr := transaction.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close native package transaction: %w", closeErr))
		}
	}()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package transaction canceled before preflight: %w", err)
	}
	if err := transaction.Preflight(ctx); err != nil {
		return fmt.Errorf("native package preflight rejected before mutation: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package transaction canceled before service inspection: %w", err)
	}
	service, err := transaction.InspectService(ctx)
	if err != nil {
		return fmt.Errorf("inspect native broker service before mutation: %w", err)
	}
	prepared := false
	committed := false
	defer func() {
		if !prepared || committed {
			return
		}
		rollbackCtx, cancelRollback := context.WithTimeout(
			context.WithoutCancel(ctx), nativePackageRollbackTimeout,
		)
		defer cancelRollback()
		if rollbackErr := transaction.Rollback(rollbackCtx); rollbackErr != nil {
			resultErr = errors.Join(resultErr,
				fmt.Errorf("roll back native package transaction: %w", rollbackErr))
		}
	}()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package transaction canceled before preparation: %w", err)
	}
	// Preparation can fail after stopping a prior service or publishing one of
	// the staged paths. Arm rollback before entering the mutating method.
	prepared = true
	if err := transaction.Prepare(ctx, service); err != nil {
		return fmt.Errorf("prepare protected native package staging: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package transaction canceled before driver installation: %w", err)
	}
	if err := transaction.InstallDriverAndBroker(ctx); err != nil {
		return fmt.Errorf("install native driver and broker transaction: %w", err)
	}
	if err := transaction.VerifyAuthenticatedHealth(ctx); err != nil {
		return fmt.Errorf("verify native package authenticated health: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit native package transaction: %w", err)
	}
	committed = true
	logger.Info("VIIPER native UDE package transaction committed",
		"transport", "native-ude", "sourceRevision", "verified")
	return nil
}
