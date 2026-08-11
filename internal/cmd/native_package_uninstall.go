package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const nativePackageUninstallCleanupTimeout = 2 * time.Minute

var nativePackageRemoveProofPattern = regexp.MustCompile(
	`(?m)^result=(success|error) operation=remove changed=([01]) rebootRequired=([01]) rollback=(not-needed|succeeded|failed) exitCode=([0-9]+)(?: .*)?\r?$`,
)

type nativePackageUninstallRequest struct {
	driverHelper         string
	expectedHelperSHA256 string
	targetUserSID        string
}

func (r nativePackageUninstallRequest) validate() error {
	if strings.TrimSpace(r.driverHelper) == "" {
		return errors.New("native package uninstall driver helper is empty")
	}
	if strings.TrimSpace(r.targetUserSID) == "" {
		return errors.New("native package uninstall target user SID is empty")
	}
	if strings.IndexByte(r.driverHelper, 0) >= 0 || strings.IndexByte(r.targetUserSID, 0) >= 0 {
		return errors.New("native package uninstall input contains NUL")
	}
	if !filepath.IsAbs(r.driverHelper) {
		return fmt.Errorf("native package uninstall driver helper must be an absolute path: %s", r.driverHelper)
	}
	if !strings.EqualFold(filepath.Base(r.driverHelper), "ViiperUdeCtl.exe") {
		return fmt.Errorf("native package uninstall helper must be named ViiperUdeCtl.exe: %s", r.driverHelper)
	}
	if !nativePackageSHA256.MatchString(r.expectedHelperSHA256) {
		return errors.New("native package uninstall helper SHA-256 must contain exactly 64 hexadecimal characters")
	}
	return nil
}

type nativePackageRemoveResult struct {
	rebootRequired         bool
	serviceRestoreVerified bool
}

type nativePackageRemoveProof struct {
	success        bool
	changed        bool
	rebootRequired bool
	rollback       string
	exitCode       int
}

func parseNativePackageRemoveProof(output string, processExitCode int) (nativePackageRemoveResult, error) {
	matches := nativePackageRemoveProofPattern.FindAllStringSubmatch(output, -1)
	if len(matches) != 1 {
		return nativePackageRemoveResult{}, errors.New("driver helper did not emit exactly one structured remove outcome")
	}
	proofExitCode, err := strconv.Atoi(matches[0][5])
	if err != nil {
		return nativePackageRemoveResult{}, fmt.Errorf("parse driver helper remove exit code: %w", err)
	}
	proof := nativePackageRemoveProof{
		success:        matches[0][1] == "success",
		changed:        matches[0][2] == "1",
		rebootRequired: matches[0][3] == "1",
		rollback:       matches[0][4],
		exitCode:       proofExitCode,
	}
	if proof.exitCode != processExitCode {
		return nativePackageRemoveResult{}, fmt.Errorf(
			"driver helper remove process exit %d disagreed with structured exit %d",
			processExitCode, proof.exitCode,
		)
	}
	switch proof.exitCode {
	case 0:
		if !proof.success || proof.rebootRequired || proof.rollback != "not-needed" {
			return nativePackageRemoveResult{}, errors.New("driver helper emitted an invalid success remove outcome")
		}
		return nativePackageRemoveResult{}, nil
	case nativePackageRebootRequiredCode:
		if !proof.success || !proof.changed || !proof.rebootRequired || proof.rollback != "not-needed" {
			return nativePackageRemoveResult{}, errors.New("driver helper emitted an invalid reboot-success remove outcome")
		}
		return nativePackageRemoveResult{rebootRequired: true}, nil
	case 4:
		if proof.success || proof.changed || proof.rebootRequired || proof.rollback != "not-needed" {
			return nativePackageRemoveResult{}, errors.New("driver helper emitted an invalid preflight-rejection outcome")
		}
		return nativePackageRemoveResult{serviceRestoreVerified: true}, fmt.Errorf("driver helper rejected package removal before mutation: %s", strings.TrimSpace(output))
	case 1:
		if proof.success || !proof.changed || proof.rollback != "succeeded" {
			return nativePackageRemoveResult{}, errors.New("driver helper emitted an invalid rolled-back failure outcome")
		}
		return nativePackageRemoveResult{serviceRestoreVerified: !proof.rebootRequired}, fmt.Errorf("driver helper package removal failed and rolled back: %s", strings.TrimSpace(output))
	case 3:
		if proof.success || !proof.changed || proof.rollback != "failed" {
			return nativePackageRemoveResult{}, errors.New("driver helper emitted an invalid rollback-failure outcome")
		}
		return nativePackageRemoveResult{}, fmt.Errorf("driver helper package removal and rollback failed: %s", strings.TrimSpace(output))
	default:
		return nativePackageRemoveResult{}, fmt.Errorf(
			"driver helper returned unsupported structured remove exit %d: %s",
			proof.exitCode, strings.TrimSpace(output),
		)
	}
}

type nativePackageUninstallServiceSnapshot struct {
	exists     bool
	wasRunning bool
	opaque     any
}

type nativePackageUninstallUnsafeRestoreError struct {
	cause error
}

func (e *nativePackageUninstallUnsafeRestoreError) Error() string {
	return e.cause.Error()
}

func (e *nativePackageUninstallUnsafeRestoreError) Unwrap() error {
	return e.cause
}

type nativePackageUninstallTransaction interface {
	LockPackage(context.Context) error
	LockService(context.Context) error
	Preflight(context.Context) error
	InspectService(context.Context) (nativePackageUninstallServiceSnapshot, error)
	StopService(context.Context, nativePackageUninstallServiceSnapshot) error
	RemoveDriver(context.Context) (nativePackageRemoveResult, error)
	Cleanup(context.Context, nativePackageUninstallServiceSnapshot) (bool, error)
	RestoreService(context.Context, nativePackageUninstallServiceSnapshot) error
	Close() error
}

func runNativePackageUninstallTransaction(
	ctx context.Context,
	logger *slog.Logger,
	transaction nativePackageUninstallTransaction,
) (resultErr error) {
	if transaction == nil {
		return errors.New("native package uninstall transaction is nil")
	}
	defer func() {
		if closeErr := transaction.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr,
				fmt.Errorf("close native package uninstall transaction: %w", closeErr))
		}
	}()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package uninstall canceled before package lock: %w", err)
	}
	if err := transaction.LockPackage(ctx); err != nil {
		return fmt.Errorf("acquire native package uninstall mutex: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package uninstall canceled before service lock: %w", err)
	}
	if err := transaction.LockService(ctx); err != nil {
		return fmt.Errorf("acquire native broker service mutex after package mutex: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package uninstall canceled before preflight: %w", err)
	}
	if err := transaction.Preflight(ctx); err != nil {
		return fmt.Errorf("native package uninstall preflight rejected before mutation: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package uninstall canceled before service inspection: %w", err)
	}
	snapshot, err := transaction.InspectService(ctx)
	if err != nil {
		return fmt.Errorf("inspect exact native broker service before package removal: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package uninstall canceled before service stop: %w", err)
	}

	restoreArmed := false
	serviceRestoreVerified := true
	driverRemovalSucceeded := false
	defer func() {
		if !restoreArmed || driverRemovalSucceeded {
			return
		}
		if !serviceRestoreVerified {
			resultErr = errors.Join(resultErr, errors.New(
				"native driver or managed-file restoration safety is unverified; exact broker was deliberately left stopped for external reconciliation",
			))
			return
		}
		rollbackCtx, cancelRollback := context.WithTimeout(
			context.WithoutCancel(ctx), nativePackageUninstallCleanupTimeout,
		)
		defer cancelRollback()
		if rollbackErr := transaction.RestoreService(rollbackCtx, snapshot); rollbackErr != nil {
			resultErr = errors.Join(resultErr,
				fmt.Errorf("restore exact native broker after package removal failure: %w", rollbackErr))
		}
	}()

	// Sending STOP is the first mutation. Arm exact run-state restoration before
	// entering the method because it can fail while the service is StopPending.
	restoreArmed = snapshot.exists
	if err := transaction.StopService(ctx, snapshot); err != nil {
		var unsafeRestore *nativePackageUninstallUnsafeRestoreError
		if errors.As(err, &unsafeRestore) {
			serviceRestoreVerified = false
		}
		return fmt.Errorf("stop exact native broker before package removal: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package uninstall canceled before driver removal: %w", err)
	}
	removeResult, err := transaction.RemoveDriver(ctx)
	if err != nil {
		serviceRestoreVerified = removeResult.serviceRestoreVerified
		return fmt.Errorf("remove exact native driver package: %w", err)
	}
	// The helper owns the authoritative Driver Store snapshot and reports success
	// only after either final verification or a Windows reboot-success boundary.
	// Never restart a now-driverless broker after this point, even if cleanup fails.
	driverRemovalSucceeded = true
	if err := ctx.Err(); err != nil {
		logger.Warn("Native driver removal completed at the transaction deadline; reconciling exact owned cleanup",
			"deadline", err)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(
		context.WithoutCancel(ctx), nativePackageUninstallCleanupTimeout,
	)
	defer cancelCleanup()
	cleanupRebootRequired, err := transaction.Cleanup(cleanupCtx, snapshot)
	if err != nil {
		if removeResult.rebootRequired {
			return fmt.Errorf("clean up exact native broker ownership after reboot-successful driver removal (restart still required): %w", err)
		}
		return fmt.Errorf("clean up exact native broker ownership after driver removal: %w", err)
	}
	if removeResult.rebootRequired || cleanupRebootRequired {
		return &nativePackageUninstallRebootRequiredError{}
	}
	return nil
}

type nativePackageUninstallRebootRequiredError struct{}

func (*nativePackageUninstallRebootRequiredError) Error() string {
	return "native package removal succeeded; restart Windows to complete exact driver removal"
}

func (*nativePackageUninstallRebootRequiredError) ExitCode() int {
	return nativePackageRebootRequiredCode
}
