package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const nativePackageUninstallCleanupTimeout = 2 * time.Minute

const (
	nativePackageRemoveProofMaximumLineBytes         = 64 * 1024
	nativePackageRemoveRetainedTombstoneWarning      = "remove-settled-cleanup-retained"
	nativePackageRemoveRetainedTombstoneMaximumRunes = 259
	nativePackageRemoveRecoveryDirectory             = "VIIPER-UdeCx-RemoveTransactions"
	nativePackageRemoveSettledPrefix                 = "settled-v2-"
)

type nativePackageUninstallRequest struct {
	driverHelper                       string
	expectedHelperSHA256               string
	targetUserSID                      string
	sourceRevision                     string
	localTestCertificatePath           string
	expectedLocalTestCertificateSHA256 string
	expectedLocalTestPackageLockSHA256 string
}

func nativePackageLocalTestUninstallMayMutateTopology(state string) bool {
	switch state {
	case "pending", "owned", "uninstalling":
		return true
	default:
		return false
	}
}

// nativePackageProductionUninstallLocalTrustAdmission classifies local-test
// ownership while the production uninstall holds Trust -> Package -> Service.
// A production uninstall has no source-bound local certificate authority, so
// only no journal or one validated terminal settlement may proceed.
func nativePackageProductionUninstallLocalTrustAdmission(states []string) error {
	if len(states) == 0 {
		return nil
	}
	if len(states) != 1 {
		return errors.New("multiple local-test trust ownership states block production uninstall")
	}
	switch states[0] {
	case "cleared":
		return nil
	case "preparing", "pending", "owned", "uninstalling":
		return fmt.Errorf(
			"active local-test trust %s state requires exact source-bound uninstall identity",
			states[0],
		)
	default:
		return fmt.Errorf(
			"unknown local-test trust %q state blocks production uninstall", states[0],
		)
	}
}

func (r nativePackageUninstallRequest) localTestTrustRequested() bool {
	return r.sourceRevision != "" || r.localTestCertificatePath != "" ||
		r.expectedLocalTestCertificateSHA256 != "" ||
		r.expectedLocalTestPackageLockSHA256 != ""
}

func (r nativePackageUninstallRequest) validate() error {
	if strings.TrimSpace(r.driverHelper) == "" {
		return errors.New("native package uninstall driver helper is empty")
	}
	if strings.TrimSpace(r.targetUserSID) == "" {
		return errors.New("native package uninstall target user SID is empty")
	}
	for _, value := range []string{
		r.driverHelper, r.targetUserSID, r.sourceRevision,
		r.localTestCertificatePath, r.expectedLocalTestCertificateSHA256,
		r.expectedLocalTestPackageLockSHA256,
	} {
		if strings.IndexByte(value, 0) >= 0 {
			return errors.New("native package uninstall input contains NUL")
		}
	}
	if r.localTestTrustRequested() {
		if !nativePackageHexRevision.MatchString(r.sourceRevision) {
			return errors.New("native package local-test uninstall source revision must contain exactly 40 or 64 hexadecimal characters")
		}
		if !filepath.IsAbs(r.localTestCertificatePath) ||
			!strings.EqualFold(filepath.Base(r.localTestCertificatePath), "ViiperUdeTest.cer") {
			return errors.New("native package local-test uninstall certificate must be an absolute ViiperUdeTest.cer path")
		}
		if !nativePackageSHA256.MatchString(r.expectedLocalTestCertificateSHA256) {
			return errors.New("native package local-test uninstall certificate SHA-256 must contain exactly 64 hexadecimal characters")
		}
		if !nativePackageSHA256.MatchString(r.expectedLocalTestPackageLockSHA256) {
			return errors.New("native package local-test uninstall package-lock SHA-256 must contain exactly 64 hexadecimal characters")
		}
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
	rebootRequired              bool
	serviceRestoreVerified      bool
	retainedTombstone           string
	retainedTombstoneWin32Error uint32
}

type nativePackageRemoveProof struct {
	success                     bool
	changed                     bool
	rebootRequired              bool
	rollback                    string
	exitCode                    int
	retainedTombstone           string
	retainedTombstoneWin32Error uint32
}

type nativePackageRemoveProofField struct {
	name   string
	value  string
	quoted bool
}

func parseNativePackageRemoveProofFields(line string) ([]nativePackageRemoveProofField, error) {
	if line == "" || len(line) > nativePackageRemoveProofMaximumLineBytes || !utf8.ValidString(line) ||
		strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("driver helper emitted a malformed structured remove outcome line")
	}
	fields := make([]nativePackageRemoveProofField, 0, 16)
	seen := make(map[string]struct{}, 16)
	for position := 0; position < len(line); {
		if position != 0 {
			if line[position] != ' ' {
				return nil, errors.New("driver helper structured remove fields are not space-delimited")
			}
			position++
			if position == len(line) || line[position] == ' ' {
				return nil, errors.New("driver helper structured remove outcome has empty or trailing fields")
			}
		}
		nameStart := position
		for position < len(line) && line[position] != '=' {
			character := line[position]
			if !((character >= 'a' && character <= 'z') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9')) {
				return nil, errors.New("driver helper structured remove outcome has an invalid field name")
			}
			position++
		}
		if position == nameStart || position == len(line) {
			return nil, errors.New("driver helper structured remove outcome has a field without a value")
		}
		name := line[nameStart:position]
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("driver helper structured remove outcome duplicated field %q", name)
		}
		seen[name] = struct{}{}
		position++
		quoted := position < len(line) && line[position] == '"'
		var value strings.Builder
		if quoted {
			position++
			closed := false
			for position < len(line) {
				character := line[position]
				position++
				if character == '"' {
					closed = true
					break
				}
				if character == '\\' {
					if position == len(line) || (line[position] != '\\' && line[position] != '"') {
						return nil, errors.New("driver helper structured remove outcome has an invalid quoted escape")
					}
					character = line[position]
					position++
				}
				if character < 0x20 || character == 0x7f {
					return nil, errors.New("driver helper structured remove outcome has a control character")
				}
				value.WriteByte(character)
			}
			if !closed {
				return nil, errors.New("driver helper structured remove outcome has an unterminated quoted value")
			}
			if position < len(line) && line[position] != ' ' {
				return nil, errors.New("driver helper structured remove outcome has trailing quoted data")
			}
		} else {
			valueStart := position
			for position < len(line) && line[position] != ' ' {
				character := line[position]
				if !((character >= 'a' && character <= 'z') ||
					(character >= 'A' && character <= 'Z') ||
					(character >= '0' && character <= '9') || character == '-') {
					return nil, errors.New("driver helper structured remove outcome has an invalid unquoted value")
				}
				position++
			}
			if position == valueStart {
				return nil, errors.New("driver helper structured remove outcome has an empty unquoted value")
			}
			value.WriteString(line[valueStart:position])
		}
		fields = append(fields, nativePackageRemoveProofField{
			name: name, value: value.String(), quoted: quoted,
		})
	}
	return fields, nil
}

func requireNativePackageRemoveProofField(
	fields []nativePackageRemoveProofField,
	position *int,
	name string,
	quoted bool,
) (string, error) {
	if *position >= len(fields) || fields[*position].name != name || fields[*position].quoted != quoted {
		return "", fmt.Errorf("driver helper structured remove outcome requires ordered field %q", name)
	}
	value := fields[*position].value
	(*position)++
	return value, nil
}

func parseNativePackageRemoveUint32(fieldName, value string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse driver helper remove %s: %w", fieldName, err)
	}
	return uint32(parsed), nil
}

func validateNativePackageRemoveErrorEvidence(
	fields []nativePackageRemoveProofField,
	position *int,
) error {
	phase, err := requireNativePackageRemoveProofField(fields, position, "phase", true)
	if err != nil {
		return err
	}
	if phase == "" || utf8.RuneCountInString(phase) > 256 {
		return errors.New("driver helper structured remove outcome has an invalid error phase")
	}
	win32Error, err := requireNativePackageRemoveProofField(fields, position, "win32Error", false)
	if err != nil {
		return err
	}
	if _, err := parseNativePackageRemoveUint32("Win32 error", win32Error); err != nil {
		return err
	}
	if *position < len(fields) && fields[*position].name == "nestedExitCode" {
		nestedExitCode, err := requireNativePackageRemoveProofField(fields, position, "nestedExitCode", false)
		if err != nil {
			return err
		}
		if _, err := strconv.ParseInt(nestedExitCode, 10, 32); err != nil {
			return fmt.Errorf("parse driver helper remove nested exit code: %w", err)
		}
	}
	message, err := requireNativePackageRemoveProofField(fields, position, "message", true)
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(message) > 4096 {
		return errors.New("driver helper structured remove outcome error message is unbounded")
	}
	if *position < len(fields) && fields[*position].name == "recoveryRecord" {
		recoveryRecord, err := requireNativePackageRemoveProofField(fields, position, "recoveryRecord", true)
		if err != nil {
			return err
		}
		if recoveryRecord == "" || utf8.RuneCountInString(recoveryRecord) > 32767 {
			return errors.New("driver helper structured remove outcome has an invalid recovery record path")
		}
		recordWritten, err := requireNativePackageRemoveProofField(fields, position, "recoveryRecordWritten", false)
		if err != nil {
			return err
		}
		if recordWritten != "0" && recordWritten != "1" {
			return errors.New("driver helper structured remove outcome has an invalid recovery record state")
		}
		if *position < len(fields) && fields[*position].name == "recoveryRecordPhase" {
			if recordWritten != "0" {
				return errors.New("driver helper structured remove outcome attached a write failure to a published recovery record")
			}
			if _, err := requireNativePackageRemoveProofField(fields, position, "recoveryRecordPhase", true); err != nil {
				return err
			}
			recordError, err := requireNativePackageRemoveProofField(fields, position, "recoveryRecordWin32Error", false)
			if err != nil {
				return err
			}
			if _, err := parseNativePackageRemoveUint32("recovery record Win32 error", recordError); err != nil {
				return err
			}
			if _, err := requireNativePackageRemoveProofField(fields, position, "recoveryRecordMessage", true); err != nil {
				return err
			}
		}
	}
	if *position < len(fields) && fields[*position].name == "recoveryBackup" {
		recoveryBackup, err := requireNativePackageRemoveProofField(fields, position, "recoveryBackup", true)
		if err != nil {
			return err
		}
		if recoveryBackup == "" || utf8.RuneCountInString(recoveryBackup) > 32767 {
			return errors.New("driver helper structured remove outcome has an invalid recovery backup path")
		}
		backupRetained, err := requireNativePackageRemoveProofField(fields, position, "recoveryBackupRetained", false)
		if err != nil {
			return err
		}
		if backupRetained != "0" && backupRetained != "1" {
			return errors.New("driver helper structured remove outcome has an invalid recovery backup state")
		}
	}
	return nil
}

func validateNativePackageRemoveRetainedTombstone(path string) error {
	if path == "" || utf8.RuneCountInString(path) > nativePackageRemoveRetainedTombstoneMaximumRunes ||
		len(path) < 4 || !((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) ||
		path[1] != ':' || path[2] != '\\' || strings.Contains(path, "/") {
		return errors.New("driver helper retained tombstone is not a bounded absolute Windows path")
	}
	components := strings.Split(path[3:], `\`)
	if len(components) < 3 ||
		!strings.EqualFold(components[len(components)-2], nativePackageRemoveRecoveryDirectory) {
		return errors.New("driver helper retained tombstone is outside the remove recovery directory")
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." ||
			strings.ContainsAny(component, `:*?"<>|`) || strings.HasSuffix(component, " ") ||
			strings.HasSuffix(component, ".") {
			return errors.New("driver helper retained tombstone has an invalid path component")
		}
		for _, character := range component {
			if character < 0x20 || character == 0x7f {
				return errors.New("driver helper retained tombstone has a control character")
			}
		}
	}
	settledName := components[len(components)-1]
	if !strings.HasPrefix(settledName, nativePackageRemoveSettledPrefix) {
		return errors.New("driver helper retained tombstone has an invalid settled identity")
	}
	transactionID := strings.TrimPrefix(settledName, nativePackageRemoveSettledPrefix)
	if len(transactionID) != 64 {
		return errors.New("driver helper retained tombstone has an invalid transaction identity length")
	}
	for _, character := range transactionID {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return errors.New("driver helper retained tombstone has a non-canonical transaction identity")
		}
	}
	return nil
}

func parseOptionalNativePackageRemoveWarning(
	fields []nativePackageRemoveProofField,
	position *int,
	proof *nativePackageRemoveProof,
) error {
	if *position == len(fields) {
		return nil
	}
	warning, err := requireNativePackageRemoveProofField(fields, position, "warning", true)
	if err != nil {
		return err
	}
	if warning != nativePackageRemoveRetainedTombstoneWarning {
		return fmt.Errorf("driver helper emitted unsupported remove warning %q", warning)
	}
	warningError, err := requireNativePackageRemoveProofField(fields, position, "warningWin32Error", false)
	if err != nil {
		return err
	}
	parsedWarningError, err := parseNativePackageRemoveUint32("warning Win32 error", warningError)
	if err != nil {
		return err
	}
	if parsedWarningError == 0 {
		return errors.New("driver helper retained tombstone warning has no cleanup error")
	}
	retainedTombstone, err := requireNativePackageRemoveProofField(fields, position, "retainedTombstone", true)
	if err != nil {
		return err
	}
	if err := validateNativePackageRemoveRetainedTombstone(retainedTombstone); err != nil {
		return err
	}
	if *position != len(fields) {
		return errors.New("driver helper structured remove outcome has trailing fields after its warning evidence")
	}
	proof.retainedTombstone = retainedTombstone
	proof.retainedTombstoneWin32Error = parsedWarningError
	return nil
}

func parseNativePackageRemoveProof(output string, processExitCode int) (nativePackageRemoveResult, error) {
	var proofLines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "result=") {
			proofLines = append(proofLines, line)
		}
	}
	if len(proofLines) != 1 {
		return nativePackageRemoveResult{}, errors.New("driver helper did not emit exactly one structured remove outcome")
	}
	fields, err := parseNativePackageRemoveProofFields(proofLines[0])
	if err != nil {
		return nativePackageRemoveResult{}, err
	}
	position := 0
	resultValue, err := requireNativePackageRemoveProofField(fields, &position, "result", false)
	if err != nil {
		return nativePackageRemoveResult{}, err
	}
	operation, err := requireNativePackageRemoveProofField(fields, &position, "operation", false)
	if err != nil || operation != "remove" {
		return nativePackageRemoveResult{}, errors.New("driver helper structured outcome is not an exact remove operation")
	}
	changed, err := requireNativePackageRemoveProofField(fields, &position, "changed", false)
	if err != nil || (changed != "0" && changed != "1") {
		return nativePackageRemoveResult{}, errors.New("driver helper structured remove outcome has an invalid changed state")
	}
	rebootRequired, err := requireNativePackageRemoveProofField(fields, &position, "rebootRequired", false)
	if err != nil || (rebootRequired != "0" && rebootRequired != "1") {
		return nativePackageRemoveResult{}, errors.New("driver helper structured remove outcome has an invalid reboot state")
	}
	rollback, err := requireNativePackageRemoveProofField(fields, &position, "rollback", false)
	if err != nil || (rollback != "not-needed" && rollback != "succeeded" && rollback != "failed") {
		return nativePackageRemoveResult{}, errors.New("driver helper structured remove outcome has an invalid rollback state")
	}
	exitCodeValue, err := requireNativePackageRemoveProofField(fields, &position, "exitCode", false)
	if err != nil {
		return nativePackageRemoveResult{}, err
	}
	proofExitCode, err := strconv.ParseUint(exitCodeValue, 10, 31)
	if err != nil {
		return nativePackageRemoveResult{}, fmt.Errorf("parse driver helper remove exit code: %w", err)
	}
	proof := nativePackageRemoveProof{
		success:        resultValue == "success",
		changed:        changed == "1",
		rebootRequired: rebootRequired == "1",
		rollback:       rollback,
		exitCode:       int(proofExitCode),
	}
	if resultValue != "success" && resultValue != "error" {
		return nativePackageRemoveResult{}, errors.New("driver helper structured remove outcome has an invalid result state")
	}
	if !proof.success {
		if err := validateNativePackageRemoveErrorEvidence(fields, &position); err != nil {
			return nativePackageRemoveResult{}, err
		}
	}
	if err := parseOptionalNativePackageRemoveWarning(fields, &position, &proof); err != nil {
		return nativePackageRemoveResult{}, err
	}
	if position != len(fields) {
		return nativePackageRemoveResult{}, errors.New("driver helper structured remove outcome has unknown or trailing fields")
	}
	if proof.exitCode != processExitCode {
		return nativePackageRemoveResult{}, fmt.Errorf(
			"driver helper remove process exit %d disagreed with structured exit %d",
			processExitCode, proof.exitCode,
		)
	}
	result := nativePackageRemoveResult{
		retainedTombstone:           proof.retainedTombstone,
		retainedTombstoneWin32Error: proof.retainedTombstoneWin32Error,
	}
	switch proof.exitCode {
	case 0:
		if !proof.success || proof.rebootRequired || proof.rollback != "not-needed" {
			return nativePackageRemoveResult{}, errors.New("driver helper emitted an invalid success remove outcome")
		}
		return result, nil
	case nativePackageRebootRequiredCode:
		if !proof.success || !proof.changed || !proof.rebootRequired || proof.rollback != "not-needed" {
			return nativePackageRemoveResult{}, errors.New("driver helper emitted an invalid reboot-success remove outcome")
		}
		result.rebootRequired = true
		return result, nil
	case 4:
		if proof.success || proof.changed || proof.rebootRequired || proof.rollback != "not-needed" {
			return nativePackageRemoveResult{}, errors.New("driver helper emitted an invalid preflight-rejection outcome")
		}
		result.serviceRestoreVerified = true
		return result, fmt.Errorf("driver helper rejected package removal before mutation: %s", strings.TrimSpace(output))
	case 1:
		if proof.success || !proof.changed || proof.rollback != "succeeded" {
			return nativePackageRemoveResult{}, errors.New("driver helper emitted an invalid rolled-back failure outcome")
		}
		result.serviceRestoreVerified = !proof.rebootRequired
		return result, fmt.Errorf("driver helper package removal failed and rolled back: %s", strings.TrimSpace(output))
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

// nativePackageUninstallAlreadySettledError is returned only after a
// source-bound local-test request proves, under Trust -> Package -> Service,
// that no driver package/device, service, or broker journal remains. The runner
// treats it as terminal idempotent success before entering any generic broker
// file inspection or topology mutation.
type nativePackageUninstallAlreadySettledError struct {
	state string
}

func (e *nativePackageUninstallAlreadySettledError) Error() string {
	return fmt.Sprintf("local-test uninstall is already settled in %s state", e.state)
}

type nativePackageUninstallTransaction interface {
	LockTrust(context.Context) error
	LockPackage(context.Context) error
	LockService(context.Context) error
	Preflight(context.Context) error
	InspectService(context.Context) (nativePackageUninstallServiceSnapshot, error)
	StopService(context.Context, nativePackageUninstallServiceSnapshot) error
	RemoveDriver(context.Context) (nativePackageRemoveResult, error)
	Cleanup(context.Context, nativePackageUninstallServiceSnapshot) (bool, error)
	FinalizeTrust(context.Context) error
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
		return fmt.Errorf("native package uninstall canceled before trust lock: %w", err)
	}
	if err := transaction.LockTrust(ctx); err != nil {
		return fmt.Errorf("acquire native package uninstall trust transaction: %w", err)
	}
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
		var alreadySettled *nativePackageUninstallAlreadySettledError
		if errors.As(err, &alreadySettled) {
			return nil
		}
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
	if removeResult.retainedTombstone != "" && logger != nil {
		logger.Warn("Native remove journal retired with a retained settled tombstone",
			"warning", nativePackageRemoveRetainedTombstoneWarning,
			"win32Error", removeResult.retainedTombstoneWin32Error,
			"retainedTombstone", removeResult.retainedTombstone)
	}
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
	if err := transaction.FinalizeTrust(cleanupCtx); err != nil {
		return fmt.Errorf("finalize exact local-test trust after topology removal: %w", err)
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
