package cmd

import (
	"context"
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

const (
	nativePackageTransactionTimeout = 4 * time.Minute
	nativePackageRollbackTimeout    = 2 * time.Minute
	nativePackageRebootRequiredCode = 3010
)

type nativePackageRebootRequiredError struct {
	cause error
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

// NativePackageInstall is the narrow bootstrapper boundary for the production
// native UDE package. It is hidden because normal users enter through the
// signed DS4Windows installer, which embeds the reviewed hashes passed here.
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
}

// NativePackageBrokerCommit is invoked only by ViiperUdeCtl while the signed
// outer package transaction holds its machine mutex and protected token.
type NativePackageBrokerCommit struct {
	TokenFile                 string `help:"Protected package-transaction token path." required:""`
	ExpectedTokenSHA256       string `help:"SHA-256 of the protected transaction token." required:""`
	ExpectedBrokerSHA256      string `help:"Installer-bound SHA-256 of the broker being committed." required:""`
	TargetUserSID             string `help:"Interactive Windows user SID that owns legacy startup state." required:""`
	TransactionDeadlineUnixMS string `help:"Outer package transaction deadline as Unix milliseconds." required:""`
}

type nativePackageBrokerCommitResult struct {
	success  bool
	changed  bool
	rollback string
	exitCode int
}

type nativePackageInstallProof struct {
	success        bool
	changed        bool
	rebootRequired bool
	rollback       string
	exitCode       int
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
		if proof.success || !proof.changed || !proof.rebootRequired || proof.rollback != "succeeded" {
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
	return proof, nil
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
			strings.TrimSpace(c.TransactionDeadlineUnixMS))
	}
	fmt.Fprint(os.Stdout, result.proofLine())
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
