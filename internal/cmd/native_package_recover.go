package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var nativePackageRecoverEmptyStatusPattern = regexp.MustCompile(
	`(?m)^devices=0 packages=0\r?$`,
)
var nativePackageRecoverStatusOutcomePattern = regexp.MustCompile(
	`(?m)^result=success operation=status changed=0 rebootRequired=0 rollback=not-needed exitCode=0\r?$`,
)

// NativePackageRecover is deliberately narrower than Uninstall. It verifies
// the packaged helper and invokes only the verify-only, recordless R4 failure
// operation. It never invokes generic recover/remove and cannot reconcile or
// mutate an independently installed successor journal or topology.
type NativePackageRecover struct {
	DriverHelper                        string `help:"Path to the packaged ViiperUdeCtl.exe." required:""`
	ExpectedHelperSHA256                string `help:"Installer-embedded SHA-256 of ViiperUdeCtl.exe." required:""`
	CertificatePath                     string `help:"Path to the exact failed-install certificate bytes." required:""`
	ExpectedCertificateSHA256           string `help:"SHA-256 of the exact failed-install certificate bytes." required:""`
	RecoveryAuthorization               string `help:"Path to the immutable failed-install recovery authorization receipt." required:""`
	ExpectedRecoveryAuthorizationSHA256 string `help:"SHA-256 of the immutable failed-install recovery authorization receipt." required:""`
	RecoveryRootAuthorizationSHA256     string `help:"Stable SHA-256 of the first failed-install recovery authorization in this retry chain." required:""`
	SourceRevision                      string `help:"Current source-bound VIIPER revision." required:""`
	RecoveryCapability                  string `help:"Protected parent-bound failed-install recovery capability." required:""`
	ExpectedRecoveryCapabilitySHA256    string `help:"SHA-256 of the protected failed-install recovery capability." required:""`
	CurrentPackageLockSHA256            string `help:"Current source-bound local-test package-lock SHA-256." required:""`
	CurrentBundleManifestSHA256         string `help:"Current source-bound validation bundle-manifest SHA-256." required:""`
	AllowPartialCertificateState        bool   `help:"Allow an exact zero/one per-store state only on a bound retry."`
}

type nativePackageRecoverRequest struct {
	driverHelper                        string
	expectedHelperSHA256                string
	certificatePath                     string
	expectedCertificateSHA256           string
	recoveryAuthorization               string
	expectedRecoveryAuthorizationSHA256 string
	recoveryRootAuthorizationSHA256     string
	sourceRevision                      string
	brokerSource                        string
	recoveryCapability                  string
	expectedRecoveryCapabilitySHA256    string
	currentPackageLockSHA256            string
	currentBundleManifestSHA256         string
	allowPartialCertificateState        bool
}

func (r nativePackageRecoverRequest) validate() error {
	if strings.TrimSpace(r.driverHelper) == "" {
		return errors.New("native package recovery driver helper is empty")
	}
	if strings.IndexByte(r.driverHelper, 0) >= 0 {
		return errors.New("native package recovery driver helper contains NUL")
	}
	if !filepath.IsAbs(r.driverHelper) {
		return fmt.Errorf("native package recovery driver helper must be an absolute path: %s", r.driverHelper)
	}
	if !strings.EqualFold(filepath.Base(r.driverHelper), "ViiperUdeCtl.exe") {
		return fmt.Errorf("native package recovery helper must be named ViiperUdeCtl.exe: %s", r.driverHelper)
	}
	if !nativePackageSHA256.MatchString(r.expectedHelperSHA256) {
		return errors.New("native package recovery helper SHA-256 must contain exactly 64 hexadecimal characters")
	}
	if strings.TrimSpace(r.certificatePath) == "" || strings.IndexByte(r.certificatePath, 0) >= 0 ||
		!filepath.IsAbs(r.certificatePath) || !strings.EqualFold(filepath.Base(r.certificatePath), "ViiperUdeTest.cer") {
		return errors.New("native package recovery certificate must be an absolute ViiperUdeTest.cer path")
	}
	if !nativePackageSHA256.MatchString(r.expectedCertificateSHA256) {
		return errors.New("native package recovery certificate SHA-256 must contain exactly 64 hexadecimal characters")
	}
	if strings.TrimSpace(r.recoveryAuthorization) == "" ||
		strings.IndexByte(r.recoveryAuthorization, 0) >= 0 ||
		!filepath.IsAbs(r.recoveryAuthorization) ||
		!strings.EqualFold(filepath.Base(r.recoveryAuthorization), "failed-install-recovery-progress.json") {
		return errors.New("native package recovery authorization must be an absolute failed-install-recovery-progress.json path")
	}
	if !nativePackageSHA256.MatchString(r.expectedRecoveryAuthorizationSHA256) {
		return errors.New("native package recovery authorization SHA-256 must contain exactly 64 hexadecimal characters")
	}
	if !nativePackageSHA256.MatchString(r.recoveryRootAuthorizationSHA256) {
		return errors.New("native package recovery root authorization SHA-256 must contain exactly 64 hexadecimal characters")
	}
	if !nativePackageHexRevision.MatchString(r.sourceRevision) {
		return errors.New("native package recovery source revision must contain exactly 40 or 64 hexadecimal characters")
	}
	if strings.TrimSpace(r.brokerSource) == "" || strings.IndexByte(r.brokerSource, 0) >= 0 ||
		!filepath.IsAbs(r.brokerSource) {
		return errors.New("native package recovery broker source must be an absolute path without NUL")
	}
	if strings.TrimSpace(r.recoveryCapability) == "" ||
		strings.IndexByte(r.recoveryCapability, 0) >= 0 ||
		!filepath.IsAbs(r.recoveryCapability) ||
		!strings.EqualFold(filepath.Base(r.recoveryCapability), nativePackageFailedInstallRecoveryCapabilityName) {
		return errors.New("native package recovery capability must be an absolute failed-install-recovery-capability.json path")
	}
	if !nativePackageSHA256.MatchString(r.expectedRecoveryCapabilitySHA256) ||
		!nativePackageSHA256.MatchString(r.currentPackageLockSHA256) ||
		!nativePackageSHA256.MatchString(r.currentBundleManifestSHA256) {
		return errors.New("native package recovery capability, package-lock, and bundle-manifest SHA-256 values must be exact")
	}
	return nil
}

const nativePackageFailedInstallRecoveryCapabilitySchema = "viiper.native.failed-install-recovery-capability/v1"
const nativePackageFailedInstallRecoveryCapabilityName = "failed-install-recovery-capability.json"

const (
	nativePackageR4RecoveryProgressSchema   = "viiper.windows11.failed-install-recovery-progress/v1"
	nativePackageR4EvidenceRoot             = `C:\Users\hbash\Documents\Codex\2026-08-15\the\outputs\VIIPER-Win11-9481f9d-272f6a0-r4`
	nativePackageR4InstallEvidenceDirectory = `C:\Users\hbash\Documents\Codex\2026-08-15\the\outputs\VIIPER-Win11-9481f9d-272f6a0-r4\steps\20260816T034608909Z-install-27fffa05b7e544feb3c5a415ebd1f6c4`
	nativePackageR4StatePath                = `C:\Users\hbash\Documents\Codex\2026-08-15\the\outputs\VIIPER-Win11-9481f9d-272f6a0-r4\state\validation-state.json`
	nativePackageR4StateSHA256              = "e13c686a0cddcf66620940005568b3a7a9a41abb277f61977dd88994863d8cda"
	nativePackageR4InstallCommandSHA256     = "c38579b1504c8851dd72317d49f4439d14b7878b4e19907ebe864c8ad986e3f7"
	nativePackageR4InstallResultSHA256      = "1095194f448455f746b5af92b89ae4f08f8f69a7ba9fac1d17a90d73e8a971b0"
	// The exact stdout digest binds changed=0, rebootRequired=0,
	// rollback=not-needed, exitCode=4, phase=install-journal-broker-image-hash,
	// win32Error=23, and the immutable-broker-digest failure message.
	nativePackageR4InstallStdoutSHA256      = "ca95fac3b8bd6fe7871a7f42400031f01ea946dc88786e9e9a746084144c205b"
	nativePackageR4InstallStderrSHA256      = "2610d56f76be3c1aea4f6b3dd4e4b38d134a1d311133ac46f389a28f8faeb520"
	nativePackageR4BundleManifestSHA256     = "765de4fe822004e97940fa66ba73602dafd68194d14fd64e20b388444cd4c247"
	nativePackageR4ViiperSourceRevision     = "9481f9dbfde64af99905fa325546e50b5ea03d6e"
	nativePackageR4DS4WindowsSourceRevision = "272f6a05f1476d5aa9c055a234e61c292d3c1556"
	nativePackageR4PackageLockSHA256        = "16e08c31bb1c240a3612a6c4ddc8219b040d0e2dec5773e39f363d045113ab8c"
	nativePackageR4CertificateSHA256        = "09ca0c2d4d3da29268eff59cf85b6c1347d4a28ddc098b8640381694ad74c517"
)

var nativePackageR4RecoveryTargetSIDPattern = regexp.MustCompile(
	`^S-1-5-21-(?:[0-9]+-){3}[0-9]+$`,
)

type nativePackageR4RecoveryPredecessor struct {
	PredecessorEvidenceRoot  string `json:"predecessorEvidenceRoot"`
	InstallEvidenceDirectory string `json:"installEvidenceDirectory"`
	StatePath                string `json:"statePath"`
	StateSHA256              string `json:"stateSha256"`
	CommandSHA256            string `json:"commandSha256"`
	ResultSHA256             string `json:"resultSha256"`
	StdoutSHA256             string `json:"stdoutSha256"`
	StderrSHA256             string `json:"stderrSha256"`
	BundleManifestSHA256     string `json:"bundleManifestSha256"`
	ViiperSourceRevision     string `json:"viiperSourceRevision"`
	DS4WindowsSourceRevision string `json:"ds4WindowsSourceRevision"`
	PackageLockSHA256        string `json:"packageLockSha256"`
}

type nativePackageR4RecoveryTrustBefore struct {
	Root             int `json:"Root"`
	TrustedPublisher int `json:"TrustedPublisher"`
}

type nativePackageR4RecoveryAuthorization struct {
	Schema                          string                             `json:"schema"`
	Status                          string                             `json:"status"`
	RetryPermitted                  bool                               `json:"retryPermitted"`
	FirstAuthorizedUTC              string                             `json:"firstAuthorizedUtc"`
	CurrentBundleManifestSHA256     string                             `json:"currentBundleManifestSha256"`
	CurrentViiperSourceRevision     string                             `json:"currentViiperSourceRevision"`
	CurrentPackageLockSHA256        string                             `json:"currentPackageLockSha256"`
	Predecessor                     nativePackageR4RecoveryPredecessor `json:"predecessor"`
	PredecessorCertificateSHA256    string                             `json:"predecessorCertificateSha256"`
	Machine                         string                             `json:"machine"`
	TargetUserSID                   string                             `json:"targetUserSid"`
	TrustBeforeNativeAttempt        nativePackageR4RecoveryTrustBefore `json:"trustBeforeNativeAttempt"`
	Resume                          bool                               `json:"resume"`
	UpdatedUTC                      string                             `json:"updatedUtc"`
	RecoveryRootAuthorizationSHA256 *string                            `json:"recoveryRootAuthorizationSha256,omitempty"`
}

func consumeNativePackageRecoveryJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("recovery authorization object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("recovery authorization contains duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeNativePackageRecoveryJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("recovery authorization object is not terminated")
		}
	case '[':
		for decoder.More() {
			if err := consumeNativePackageRecoveryJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("recovery authorization array is not terminated")
		}
	default:
		return errors.New("recovery authorization contains an unexpected JSON delimiter")
	}
	return nil
}

func rejectNativePackageRecoveryDuplicateJSONFields(contents []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := consumeNativePackageRecoveryJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("recovery authorization contains trailing JSON")
		}
		return fmt.Errorf("read recovery authorization terminator: %w", err)
	}
	return nil
}

func requireNativePackageRecoveryJSONKeys(
	object map[string]json.RawMessage,
	required []string,
	optional []string,
	label string,
) error {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, name := range required {
		allowed[name] = true
		if _, present := object[name]; !present {
			return fmt.Errorf("recovery authorization %s is missing field %q", label, name)
		}
	}
	for _, name := range optional {
		allowed[name] = true
	}
	for name := range object {
		if !allowed[name] {
			return fmt.Errorf("recovery authorization %s contains unknown field %q", label, name)
		}
	}
	return nil
}

func decodeNativePackageR4RecoveryAuthorization(
	contents []byte,
) (nativePackageR4RecoveryAuthorization, error) {
	value := nativePackageR4RecoveryAuthorization{}
	if err := rejectNativePackageRecoveryDuplicateJSONFields(contents); err != nil {
		return value, err
	}
	root := make(map[string]json.RawMessage)
	if err := json.Unmarshal(contents, &root); err != nil {
		return value, fmt.Errorf("parse recovery authorization object: %w", err)
	}
	if err := requireNativePackageRecoveryJSONKeys(root, []string{
		"schema", "status", "retryPermitted", "firstAuthorizedUtc",
		"currentBundleManifestSha256", "currentViiperSourceRevision",
		"currentPackageLockSha256", "predecessor",
		"predecessorCertificateSha256", "machine", "targetUserSid",
		"trustBeforeNativeAttempt", "resume", "updatedUtc",
	}, []string{"recoveryRootAuthorizationSha256"}, "root"); err != nil {
		return value, err
	}
	predecessor := make(map[string]json.RawMessage)
	if err := json.Unmarshal(root["predecessor"], &predecessor); err != nil {
		return value, fmt.Errorf("parse recovery authorization predecessor: %w", err)
	}
	if err := requireNativePackageRecoveryJSONKeys(predecessor, []string{
		"predecessorEvidenceRoot", "installEvidenceDirectory", "statePath",
		"stateSha256", "commandSha256", "resultSha256", "stdoutSha256",
		"stderrSha256", "bundleManifestSha256", "viiperSourceRevision",
		"ds4WindowsSourceRevision", "packageLockSha256",
	}, nil, "predecessor"); err != nil {
		return value, err
	}
	trust := make(map[string]json.RawMessage)
	if err := json.Unmarshal(root["trustBeforeNativeAttempt"], &trust); err != nil {
		return value, fmt.Errorf("parse recovery authorization trust admission: %w", err)
	}
	if err := requireNativePackageRecoveryJSONKeys(
		trust, []string{"Root", "TrustedPublisher"}, nil, "trust admission",
	); err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode exact recovery authorization: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, errors.New("recovery authorization contains trailing JSON")
		}
		return value, fmt.Errorf("read exact recovery authorization terminator: %w", err)
	}
	return value, nil
}

func validateNativePackageR4RecoveryAuthorization(
	contents []byte,
	request nativePackageRecoverRequest,
	currentHostname string,
) (nativePackageR4RecoveryAuthorization, error) {
	authorization, err := decodeNativePackageR4RecoveryAuthorization(contents)
	if err != nil {
		return authorization, err
	}
	predecessor := authorization.Predecessor
	if authorization.Schema != nativePackageR4RecoveryProgressSchema ||
		authorization.Status != "native-attempt" || !authorization.RetryPermitted ||
		authorization.CurrentBundleManifestSHA256 != request.currentBundleManifestSHA256 ||
		authorization.CurrentViiperSourceRevision != request.sourceRevision ||
		authorization.CurrentPackageLockSHA256 != request.currentPackageLockSHA256 ||
		authorization.PredecessorCertificateSHA256 != nativePackageR4CertificateSHA256 ||
		authorization.PredecessorCertificateSHA256 != request.expectedCertificateSHA256 ||
		predecessor.StateSHA256 != nativePackageR4StateSHA256 ||
		predecessor.CommandSHA256 != nativePackageR4InstallCommandSHA256 ||
		predecessor.ResultSHA256 != nativePackageR4InstallResultSHA256 ||
		predecessor.StdoutSHA256 != nativePackageR4InstallStdoutSHA256 ||
		predecessor.StderrSHA256 != nativePackageR4InstallStderrSHA256 ||
		predecessor.BundleManifestSHA256 != nativePackageR4BundleManifestSHA256 ||
		predecessor.ViiperSourceRevision != nativePackageR4ViiperSourceRevision ||
		predecessor.DS4WindowsSourceRevision != nativePackageR4DS4WindowsSourceRevision ||
		predecessor.PackageLockSHA256 != nativePackageR4PackageLockSHA256 ||
		!strings.EqualFold(predecessor.PredecessorEvidenceRoot, nativePackageR4EvidenceRoot) ||
		!strings.EqualFold(predecessor.InstallEvidenceDirectory, nativePackageR4InstallEvidenceDirectory) ||
		!strings.EqualFold(predecessor.StatePath, nativePackageR4StatePath) {
		return authorization, errors.New("recovery authorization does not bind the exact manifest-known R4 failed-install predecessor and failure proof")
	}
	if strings.TrimSpace(authorization.Machine) == "" ||
		!strings.EqualFold(authorization.Machine, currentHostname) ||
		!nativePackageR4RecoveryTargetSIDPattern.MatchString(authorization.TargetUserSID) {
		return authorization, errors.New("recovery authorization machine and target user identity are invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, authorization.FirstAuthorizedUTC); err != nil {
		return authorization, errors.New("recovery authorization first authorization time is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, authorization.UpdatedUTC); err != nil {
		return authorization, errors.New("recovery authorization update time is invalid")
	}
	if authorization.Resume != request.allowPartialCertificateState {
		return authorization, errors.New("recovery authorization resume state does not match the native request")
	}
	trust := authorization.TrustBeforeNativeAttempt
	if !authorization.Resume {
		if authorization.RecoveryRootAuthorizationSHA256 != nil ||
			trust.Root != 1 || trust.TrustedPublisher != 1 {
			return authorization, errors.New("initial recovery authorization has invalid root binding or trust admission")
		}
		return authorization, nil
	}
	if authorization.RecoveryRootAuthorizationSHA256 == nil ||
		*authorization.RecoveryRootAuthorizationSHA256 != request.recoveryRootAuthorizationSHA256 ||
		trust.Root < 0 || trust.Root > 1 || trust.TrustedPublisher < 0 || trust.TrustedPublisher > 1 {
		return authorization, errors.New("recovery retry authorization has invalid root binding or trust admission")
	}
	return authorization, nil
}

type nativePackageFailedInstallRecoveryCapability struct {
	Schema                          string `json:"schema"`
	Nonce                           string `json:"nonce"`
	ParentPID                       uint32 `json:"parentPid"`
	ParentCreationFileTime          uint64 `json:"parentCreationFileTime"`
	LeasePath                       string `json:"leasePath"`
	SourceRevision                  string `json:"sourceRevision"`
	HelperSHA256                    string `json:"helperSha256"`
	CertificateSHA256               string `json:"certificateSha256"`
	RecoveryAuthorizationSHA256     string `json:"recoveryAuthorizationSha256"`
	RecoveryRootAuthorizationSHA256 string `json:"recoveryRootAuthorizationSha256"`
	PackageLockSHA256               string `json:"packageLockSha256"`
	BundleManifestSHA256            string `json:"bundleManifestSha256"`
	AllowPartialCertificateState    bool   `json:"allowPartialCertificateState"`
}

type nativePackageRecoverProof struct {
	success        bool
	changed        bool
	rebootRequired bool
	rollback       string
	exitCode       int
}

func parseNativePackageRecoverProof(output string, processExitCode int) (nativePackageRecoverProof, error) {
	if len(output) == 0 || len(output) > nativePackageRemoveProofMaximumLineBytes {
		return nativePackageRecoverProof{}, errors.New("recordless recovery helper output is empty or exceeds its bound")
	}
	const canonical = "result=success operation=recover-failed-install-recordless changed=0 rebootRequired=0 rollback=not-needed exitCode=0"
	if processExitCode != 0 {
		return nativePackageRecoverProof{}, fmt.Errorf(
			"recordless recovery helper process exited %d", processExitCode,
		)
	}
	if output != canonical+"\n" && output != canonical+"\r\n" {
		return nativePackageRecoverProof{}, errors.New(
			"recordless recovery helper did not emit exactly one canonical terminated success line",
		)
	}
	return nativePackageRecoverProof{
		success: true, rollback: "not-needed", exitCode: 0,
	}, nil
}

func validateNativePackageRecoverEmptyStatus(output string, processExitCode int) error {
	if processExitCode != 0 {
		return fmt.Errorf("driver helper status process exited %d", processExitCode)
	}
	if len(output) > 2*nativePackageRemoveProofMaximumLineBytes {
		return errors.New("driver helper status evidence exceeded its bounded contract")
	}
	if len(nativePackageRecoverEmptyStatusPattern.FindAllStringIndex(output, -1)) != 1 ||
		len(nativePackageRecoverStatusOutcomePattern.FindAllStringIndex(output, -1)) != 1 {
		return errors.New("driver helper did not prove exact zero-device, zero-package recovery status")
	}
	resultLines := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "result=") {
			resultLines++
		}
	}
	if resultLines != 1 {
		return errors.New("driver helper status did not emit exactly one structured outcome")
	}
	return nil
}

type nativePackageRecoverExitError struct {
	cause    error
	exitCode int
}

func (e *nativePackageRecoverExitError) Error() string { return e.cause.Error() }
func (e *nativePackageRecoverExitError) Unwrap() error { return e.cause }
func (e *nativePackageRecoverExitError) ExitCode() int { return e.exitCode }

func (c *NativePackageRecover) Run(logger *slog.Logger) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate native package recovery broker: %w", err)
	}
	request := nativePackageRecoverRequest{
		driverHelper:                        strings.TrimSpace(c.DriverHelper),
		expectedHelperSHA256:                strings.ToLower(strings.TrimSpace(c.ExpectedHelperSHA256)),
		certificatePath:                     strings.TrimSpace(c.CertificatePath),
		expectedCertificateSHA256:           strings.ToLower(strings.TrimSpace(c.ExpectedCertificateSHA256)),
		recoveryAuthorization:               strings.TrimSpace(c.RecoveryAuthorization),
		expectedRecoveryAuthorizationSHA256: strings.ToLower(strings.TrimSpace(c.ExpectedRecoveryAuthorizationSHA256)),
		recoveryRootAuthorizationSHA256:     strings.ToLower(strings.TrimSpace(c.RecoveryRootAuthorizationSHA256)),
		sourceRevision:                      strings.ToLower(strings.TrimSpace(c.SourceRevision)),
		brokerSource:                        executable,
		recoveryCapability:                  strings.TrimSpace(c.RecoveryCapability),
		expectedRecoveryCapabilitySHA256:    strings.ToLower(strings.TrimSpace(c.ExpectedRecoveryCapabilitySHA256)),
		currentPackageLockSHA256:            strings.ToLower(strings.TrimSpace(c.CurrentPackageLockSHA256)),
		currentBundleManifestSHA256:         strings.ToLower(strings.TrimSpace(c.CurrentBundleManifestSHA256)),
		allowPartialCertificateState:        c.AllowPartialCertificateState,
	}
	if err := request.validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativePackageTransactionTimeout)
	defer cancel()
	return recoverNativePackage(ctx, logger, request)
}

func nativePackageRecoverDeadline(ctx context.Context) (time.Time, error) {
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) {
		return time.Time{}, context.DeadlineExceeded
	}
	return deadline, nil
}
