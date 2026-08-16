package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validNativePackageRecoverRequest() nativePackageRecoverRequest {
	return nativePackageRecoverRequest{
		driverHelper:                        `C:\bundle\ViiperUdeCtl.exe`,
		expectedHelperSHA256:                strings.Repeat("a", 64),
		certificatePath:                     `C:\bundle\ViiperUdeTest.cer`,
		expectedCertificateSHA256:           strings.Repeat("b", 64),
		recoveryAuthorization:               `C:\evidence\failed-install-recovery-progress.json`,
		expectedRecoveryAuthorizationSHA256: strings.Repeat("c", 64),
		recoveryRootAuthorizationSHA256:     strings.Repeat("d", 64),
		sourceRevision:                      strings.Repeat("e", 40),
		brokerSource:                        `C:\stage\viiper.exe`,
		recoveryCapability:                  `C:\stage\failed-install-recovery-capability.json`,
		expectedRecoveryCapabilitySHA256:    strings.Repeat("f", 64),
		currentPackageLockSHA256:            strings.Repeat("1", 64),
		currentBundleManifestSHA256:         strings.Repeat("2", 64),
	}
}

func TestNativePackageRecoverRequestValidation(t *testing.T) {
	t.Parallel()
	valid := validNativePackageRecoverRequest()
	if err := valid.validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*nativePackageRecoverRequest)
		want   string
	}{
		{"relative helper", func(r *nativePackageRecoverRequest) { r.driverHelper = "ViiperUdeCtl.exe" }, "absolute"},
		{"wrong helper", func(r *nativePackageRecoverRequest) { r.driverHelper = `C:\bundle\other.exe` }, "named"},
		{"bad helper hash", func(r *nativePackageRecoverRequest) { r.expectedHelperSHA256 = "abc" }, "64"},
		{"relative certificate", func(r *nativePackageRecoverRequest) { r.certificatePath = "ViiperUdeTest.cer" }, "absolute"},
		{"wrong certificate", func(r *nativePackageRecoverRequest) { r.certificatePath = `C:\bundle\other.cer` }, "ViiperUdeTest.cer"},
		{"bad certificate hash", func(r *nativePackageRecoverRequest) { r.expectedCertificateSHA256 = "abc" }, "64"},
		{"relative authorization", func(r *nativePackageRecoverRequest) {
			r.recoveryAuthorization = "failed-install-recovery-progress.json"
		}, "absolute"},
		{"wrong authorization", func(r *nativePackageRecoverRequest) { r.recoveryAuthorization = `C:\evidence\other.json` }, "failed-install-recovery-progress.json"},
		{"bad authorization hash", func(r *nativePackageRecoverRequest) { r.expectedRecoveryAuthorizationSHA256 = "abc" }, "64"},
		{"bad root authorization hash", func(r *nativePackageRecoverRequest) { r.recoveryRootAuthorizationSHA256 = "abc" }, "root authorization"},
		{"bad source revision", func(r *nativePackageRecoverRequest) { r.sourceRevision = "abc" }, "source revision"},
		{"relative broker", func(r *nativePackageRecoverRequest) { r.brokerSource = "viiper.exe" }, "broker source"},
		{"relative capability", func(r *nativePackageRecoverRequest) { r.recoveryCapability = "failed-install-recovery-capability.json" }, "absolute"},
		{"wrong capability name", func(r *nativePackageRecoverRequest) { r.recoveryCapability = `C:\stage\other.json` }, "failed-install-recovery-capability.json"},
		{"bad capability hash", func(r *nativePackageRecoverRequest) { r.expectedRecoveryCapabilitySHA256 = "abc" }, "capability"},
		{"bad package lock hash", func(r *nativePackageRecoverRequest) { r.currentPackageLockSHA256 = "abc" }, "package-lock"},
		{"bad bundle manifest hash", func(r *nativePackageRecoverRequest) { r.currentBundleManifestSHA256 = "abc" }, "bundle-manifest"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := valid
			test.mutate(&request)
			err := request.validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}
}

func validNativePackageR4RecoveryAuthorization(
	request nativePackageRecoverRequest,
	machine string,
) nativePackageR4RecoveryAuthorization {
	return nativePackageR4RecoveryAuthorization{
		Schema:                      nativePackageR4RecoveryProgressSchema,
		Status:                      "native-attempt",
		RetryPermitted:              true,
		FirstAuthorizedUTC:          time.Now().UTC().Format(time.RFC3339Nano),
		CurrentBundleManifestSHA256: request.currentBundleManifestSHA256,
		CurrentViiperSourceRevision: request.sourceRevision,
		CurrentPackageLockSHA256:    request.currentPackageLockSHA256,
		Predecessor: nativePackageR4RecoveryPredecessor{
			PredecessorEvidenceRoot:  nativePackageR4EvidenceRoot,
			InstallEvidenceDirectory: nativePackageR4InstallEvidenceDirectory,
			StatePath:                nativePackageR4StatePath,
			StateSHA256:              nativePackageR4StateSHA256,
			CommandSHA256:            nativePackageR4InstallCommandSHA256,
			ResultSHA256:             nativePackageR4InstallResultSHA256,
			StdoutSHA256:             nativePackageR4InstallStdoutSHA256,
			StderrSHA256:             nativePackageR4InstallStderrSHA256,
			BundleManifestSHA256:     nativePackageR4BundleManifestSHA256,
			ViiperSourceRevision:     nativePackageR4ViiperSourceRevision,
			DS4WindowsSourceRevision: nativePackageR4DS4WindowsSourceRevision,
			PackageLockSHA256:        nativePackageR4PackageLockSHA256,
		},
		PredecessorCertificateSHA256: nativePackageR4CertificateSHA256,
		Machine:                      machine,
		TargetUserSID:                "S-1-5-21-1-2-3-1001",
		TrustBeforeNativeAttempt: nativePackageR4RecoveryTrustBefore{
			Root: 1, TrustedPublisher: 1,
		},
		UpdatedUTC: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func marshalNativePackageRecoveryAuthorization(
	t *testing.T,
	value nativePackageR4RecoveryAuthorization,
) []byte {
	t.Helper()
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestNativePackageR4RecoveryAuthorizationIsExactAndSourceBound(t *testing.T) {
	t.Parallel()
	const machine = "VIIPER-R4-CONTRACT"
	request := validNativePackageRecoverRequest()
	request.expectedCertificateSHA256 = nativePackageR4CertificateSHA256
	valid := validNativePackageR4RecoveryAuthorization(request, machine)
	if _, err := validateNativePackageR4RecoveryAuthorization(
		marshalNativePackageRecoveryAuthorization(t, valid), request, machine,
	); err != nil {
		t.Fatalf("exact R4 recovery authorization rejected: %v", err)
	}
	resumeRequest := request
	resumeRequest.allowPartialCertificateState = true
	resume := valid
	resume.Resume = true
	resume.TrustBeforeNativeAttempt = nativePackageR4RecoveryTrustBefore{
		Root: 0, TrustedPublisher: 1,
	}
	resume.RecoveryRootAuthorizationSHA256 = &resumeRequest.recoveryRootAuthorizationSHA256
	if _, err := validateNativePackageR4RecoveryAuthorization(
		marshalNativePackageRecoveryAuthorization(t, resume), resumeRequest, machine,
	); err != nil {
		t.Fatalf("exact bound R4 recovery retry authorization rejected: %v", err)
	}

	tests := []struct {
		name     string
		contents func() []byte
		hostname string
		want     string
	}{
		{
			name: "fabricated predecessor",
			contents: func() []byte {
				value := valid
				value.Predecessor.StateSHA256 = strings.Repeat("f", 64)
				return marshalNativePackageRecoveryAuthorization(t, value)
			},
			hostname: machine,
			want:     "exact manifest-known R4",
		},
		{
			name: "missing field",
			contents: func() []byte {
				value := make(map[string]any)
				if err := json.Unmarshal(marshalNativePackageRecoveryAuthorization(t, valid), &value); err != nil {
					t.Fatal(err)
				}
				delete(value, "status")
				contents, _ := json.Marshal(value)
				return contents
			},
			hostname: machine,
			want:     "missing field",
		},
		{
			name: "unknown field",
			contents: func() []byte {
				value := make(map[string]any)
				if err := json.Unmarshal(marshalNativePackageRecoveryAuthorization(t, valid), &value); err != nil {
					t.Fatal(err)
				}
				value["unboundAuthority"] = strings.Repeat("9", 64)
				contents, _ := json.Marshal(value)
				return contents
			},
			hostname: machine,
			want:     "unknown field",
		},
		{
			name: "duplicate field",
			contents: func() []byte {
				contents := string(marshalNativePackageRecoveryAuthorization(t, valid))
				return []byte(strings.Replace(
					contents,
					`"status":"native-attempt"`,
					`"status":"native-attempt","status":"native-attempt"`,
					1,
				))
			},
			hostname: machine,
			want:     "duplicate JSON field",
		},
		{
			name:     "other machine",
			contents: func() []byte { return marshalNativePackageRecoveryAuthorization(t, valid) },
			hostname: "OTHER-MACHINE",
			want:     "machine and target user",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := validateNativePackageR4RecoveryAuthorization(
				test.contents(), request, test.hostname,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestNativePackageRecoverStructuredProof(t *testing.T) {
	t.Parallel()
	canonical := "result=success operation=recover-failed-install-recordless changed=0 rebootRequired=0 rollback=not-needed exitCode=0"
	tests := []struct {
		name        string
		output      string
		exit        int
		wantErr     bool
		errContains string
	}{
		{
			name:   "canonical LF success",
			output: canonical + "\n",
		},
		{
			name:   "canonical CRLF success",
			output: canonical + "\r\n",
		},
		{
			name:        "missing terminator",
			output:      canonical,
			wantErr:     true,
			errContains: "canonical terminated",
		},
		{
			name:        "generic recover forbidden",
			output:      "result=success operation=recover changed=0 rebootRequired=0 rollback=not-needed exitCode=0\n",
			wantErr:     true,
			errContains: "canonical terminated",
		},
		{
			name:        "changed recovery forbidden",
			output:      "result=success operation=recover-failed-install-recordless changed=1 rebootRequired=0 rollback=not-needed exitCode=0\n",
			wantErr:     true,
			errContains: "canonical terminated",
		},
		{
			name:        "journal binding forbidden",
			output:      canonical + "\njournal-binding operation=install transactionId=unsafe\n",
			wantErr:     true,
			errContains: "exactly one",
		},
		{
			name:        "recovery diagnostics forbidden",
			output:      canonical + ` recoveryRecord="C:\\ProgramData\\VIIPER\\UdeCx\\active-v2"` + "\n",
			wantErr:     true,
			errContains: "canonical terminated",
		},
		{
			name:        "extra blank line forbidden",
			output:      canonical + "\n\n",
			wantErr:     true,
			errContains: "exactly one",
		},
		{
			name:        "nonzero process exit forbidden",
			output:      canonical + "\n",
			exit:        4,
			wantErr:     true,
			errContains: "exited 4",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseNativePackageRecoverProof(test.output, test.exit)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, test.wantErr)
			}
			if test.errContains != "" && (err == nil || !strings.Contains(err.Error(), test.errContains)) {
				t.Fatalf("error=%v missing %q", err, test.errContains)
			}
		})
	}
}

func TestNativePackageRecoverStatusRequiresEmptyTopology(t *testing.T) {
	t.Parallel()
	valid := "devices=0 packages=0\nresult=success operation=status changed=0 rebootRequired=0 rollback=not-needed exitCode=0\n"
	if err := validateNativePackageRecoverEmptyStatus(valid, 0); err != nil {
		t.Fatalf("empty status rejected: %v", err)
	}
	for name, output := range map[string]string{
		"successor device":  "devices=1 packages=1\nresult=success operation=status changed=0 rebootRequired=0 rollback=not-needed exitCode=0\n",
		"successor package": "devices=0 packages=1\nresult=success operation=status changed=0 rebootRequired=0 rollback=not-needed exitCode=0\n",
		"duplicate outcome": valid + "result=success operation=status changed=0 rebootRequired=0 rollback=not-needed exitCode=0\n",
	} {
		output := output
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := validateNativePackageRecoverEmptyStatus(output, 0); err == nil {
				t.Fatal("unsafe successor status was admitted")
			}
		})
	}
}

func TestNativePackageRecoverExitErrorPreservesCode(t *testing.T) {
	t.Parallel()
	err := &nativePackageRecoverExitError{cause: errors.New("rejected"), exitCode: 4}
	var exitCoder interface{ ExitCode() int }
	if !errors.As(err, &exitCoder) || exitCoder.ExitCode() != 4 {
		t.Fatalf("exit error lost code: %v", err)
	}
}
