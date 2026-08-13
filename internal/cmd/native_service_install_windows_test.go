//go:build windows

package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/transport/udecx"
	"github.com/Alia5/VIIPER/viipertypes"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestNativeBrokerServiceConfigurationIsExplicitAndEscaped(t *testing.T) {
	executable := `C:\Program Files\VIIPER\viiper.exe`
	credential := `C:\ProgramData\VIIPER\viiper key.txt`
	config, arguments, err := nativeBrokerServiceConfiguration(executable, credential)
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := []string{
		"service", "--transport", "native-ude", "--key-file", credential,
		"--log.file", filepath.Join(filepath.Dir(credential), nativeBrokerLogName),
	}
	if !reflect.DeepEqual(arguments, wantArguments) {
		t.Fatalf("arguments=%q want=%q", arguments, wantArguments)
	}
	if config.StartType != mgr.StartAutomatic || config.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS {
		t.Fatalf("service config is not an automatic own-process service: %+v", config)
	}
	if config.ServiceStartName != nativeServiceAccount || config.DelayedAutoStart {
		t.Fatalf("service account/start mode=%q/%v", config.ServiceStartName, config.DelayedAutoStart)
	}
	decomposed, err := windows.DecomposeCommandLine(config.BinaryPathName)
	if err != nil {
		t.Fatal(err)
	}
	if want := append([]string{executable}, wantArguments...); !reflect.DeepEqual(decomposed, want) {
		t.Fatalf("binary command=%q want=%q", decomposed, want)
	}
}

func TestNativeServiceConfigVerificationDoesNotFoldArgumentCase(t *testing.T) {
	first := mgr.Config{BinaryPathName: `"C:\Program Files\VIIPER\viiper.exe" service --key-file C:\key`}
	second := first
	second.BinaryPathName = `"C:\Program Files\VIIPER\viiper.exe" service --KEY-FILE C:\key`
	if nativeServiceConfigsEqual(first, second) {
		t.Fatal("case-only service switch mismatch verified as equal")
	}
	second = first
	second.BinaryPathName += " "
	if nativeServiceConfigsEqual(first, second) {
		t.Fatal("trailing service-command whitespace mismatch verified as equal")
	}
}

func TestNativeServiceDependenciesBlockCanClearAndRoundTrip(t *testing.T) {
	empty, err := nativeServiceDependenciesBlock(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty, []uint16{0, 0}) {
		t.Fatalf("empty dependency block=%v", empty)
	}
	block, err := nativeServiceDependenciesBlock([]string{"Tcpip", "+NetworkProvider"})
	if err != nil {
		t.Fatal(err)
	}
	want := append([]uint16{}, windows.StringToUTF16("Tcpip")...)
	want = append(want, windows.StringToUTF16("+NetworkProvider")...)
	want = append(want, 0)
	if !reflect.DeepEqual(block, want) {
		t.Fatalf("dependency block=%v want=%v", block, want)
	}
	if _, err := nativeServiceDependenciesBlock([]string{""}); err == nil {
		t.Fatal("accepted empty dependency name")
	}
}

func TestNativeServiceExecutablePathRejectsPortableAndNonDedicatedLocations(t *testing.T) {
	programFiles := `C:\Program Files`
	for _, executable := range []string{
		`C:\Users\user\Downloads\viiper.exe`,
		`C:\Program Files\viiper.exe`,
		`C:\Program Files\DS4Windows\viiper.exe`,
		`C:\Program Files\Other\VIIPER\viiper.exe`,
		`C:\Program Files\DS4Windows\VIIPER\renamed.exe`,
	} {
		if _, err := nativeServiceExecutableParent(programFiles, executable); err == nil {
			t.Fatalf("accepted unsafe LocalSystem service executable %q", executable)
		}
	}
	parent, err := nativeServiceExecutableParent(
		programFiles,
		`C:\Program Files\DS4Windows\VIIPER\viiper.exe`,
	)
	if err != nil || parent != `C:\Program Files\DS4Windows\VIIPER` {
		t.Fatalf("parent=%q error=%v", parent, err)
	}
	if parent, err := nativeServiceExecutableParent(
		programFiles, `C:\Program Files\VIIPER\viiper.exe`,
	); err != nil || parent != `C:\Program Files\VIIPER` {
		t.Fatalf("direct parent=%q error=%v", parent, err)
	}
}

func TestRotatedNativeServiceKeyNeverReusesPreseededCredential(t *testing.T) {
	generated := []string{"", "attacker-known", "fresh-random"}
	index := 0
	key, err := rotatedNativeServiceKey([]byte(" attacker-known\r\n"), func() (string, error) {
		value := generated[index]
		index++
		return value, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if key != "fresh-random" || index != 3 {
		t.Fatalf("rotated key=%q attempts=%d", key, index)
	}
}

func TestNativeFileLinkCountRejectsHardLinks(t *testing.T) {
	if err := validateNativeFileLinkCount(1); err != nil {
		t.Fatal(err)
	}
	for _, count := range []uint32{0, 2, 17} {
		if err := validateNativeFileLinkCount(count); err == nil {
			t.Fatalf("accepted unsafe file link count %d", count)
		}
	}
}

func TestNativeTaskXMLUsesExplicitUTF8Base64Transport(t *testing.T) {
	xml := `<Task><Author>Zoë 日本語 🎮</Author></Task>`
	decoded, err := base64.StdEncoding.DecodeString(encodeNativeTaskXML(xml))
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != xml {
		t.Fatalf("decoded XML=%q want=%q", decoded, xml)
	}
}

func TestNativeTaskRestorePayloadPreservesOriginalAndDisabledXML(t *testing.T) {
	original := `<Task><Author>ZoÃ« æ—¥æœ¬èªž</Author><Enabled>true</Enabled></Task>`
	current := `<Task><Author>ZoÃ« æ—¥æœ¬èªž</Author><Enabled>false</Enabled></Task>`
	decoded, err := base64.StdEncoding.DecodeString(encodeNativeTaskRestorePayload(original, current))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct{ Original, Current string }
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Original != original || payload.Current != current {
		t.Fatalf("restore payload=%+v", payload)
	}
}

func TestValidateNativeTaskDisabledOnlyRequiresExactStructuralTransition(t *testing.T) {
	const original = `<Task xmlns="urn:task"><RegistrationInfo><Author>VIIPER</Author></RegistrationInfo><Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers><Principals><Principal id="Author"><UserId>S-1-5-21-1</UserId></Principal></Principals><Settings><Enabled>true</Enabled></Settings><Actions><Exec><Command>C:\VIIPER\viiper.exe</Command></Exec></Actions></Task>`
	disabled := strings.Replace(original, `<Settings><Enabled>true</Enabled></Settings>`, `<Settings><Enabled>false</Enabled></Settings>`, 1)
	if err := validateNativeTaskDisabledOnly(original, disabled); err != nil {
		t.Fatalf("exact enabled-to-disabled transition rejected: %v", err)
	}
	mutations := map[string]string{
		"action":    strings.Replace(disabled, `C:\VIIPER\viiper.exe`, `C:\Evil\viiper.exe`, 1),
		"principal": strings.Replace(disabled, `S-1-5-21-1`, `S-1-5-18`, 1),
		"trigger":   strings.Replace(disabled, `<LogonTrigger>`, `<BootTrigger>`, 1),
		"namespace": strings.Replace(disabled, `urn:task`, `urn:other`, 1),
		"missing":   strings.Replace(original, `<Settings><Enabled>true</Enabled></Settings>`, `<Settings />`, 1),
		"duplicate": strings.Replace(disabled, `<Settings><Enabled>false</Enabled></Settings>`, `<Settings><Enabled>false</Enabled><Enabled>false</Enabled></Settings>`, 1),
		"invalid":   strings.Replace(disabled, `<Settings><Enabled>false</Enabled></Settings>`, `<Settings><Enabled>maybe</Enabled></Settings>`, 1),
		"malformed": strings.TrimSuffix(disabled, `</Task>`),
		"unchanged": original,
	}
	for name, current := range mutations {
		t.Run(name, func(t *testing.T) {
			if err := validateNativeTaskDisabledOnly(original, current); err == nil {
				t.Fatal("accepted non-exact task transition")
			}
		})
	}
}

func TestNativeLegacyRegistrationSnapshotRejectsAbsentOwnersAndTypeChanges(t *testing.T) {
	snapshot := nativeRunRegistration{value: `"C:\VIIPER\viiper.exe"`, valueType: registry.EXPAND_SZ}
	if err := validateNativeRunRegistrationSnapshot(nil, nativeRunRegistration{}, false); err != nil {
		t.Fatal(err)
	}
	if err := validateNativeRunRegistrationSnapshot(nil, snapshot, true); err == nil {
		t.Fatal("accepted Run registration that appeared after an absent snapshot")
	}
	changedType := snapshot
	changedType.valueType = registry.SZ
	if err := validateNativeRunRegistrationSnapshot(&snapshot, changedType, true); err == nil {
		t.Fatal("accepted Run registration type change with identical data")
	}
	if err := validateNativeRunRegistrationSnapshot(&snapshot, nativeRunRegistration{}, false); err == nil {
		t.Fatal("accepted disappeared Run registration")
	}
	if err := validateNativeScheduledTaskSnapshot(nativeLegacyState{}, nativeLegacyCommand{}, "", true); err == nil {
		t.Fatal("accepted RunVIIPER task that appeared after an absent snapshot")
	}
	if _, _, err := currentNativeRunRegistration(nativeLegacyState{}); err == nil ||
		!strings.Contains(err.Error(), "hive is not retained") {
		t.Fatalf("unretained user hive did not fail closed: %v", err)
	}
}

func TestNativeScheduledTaskIdentityAndStateAreFailClosed(t *testing.T) {
	for _, name := range []string{"RunVIIPER", "runviiper", "RUNVIIPER"} {
		if !nativeScheduledTaskNameMatches(name) {
			t.Fatalf("case-equivalent Task Scheduler name %q was missed", name)
		}
	}
	if nativeScheduledTaskNameMatches("RunVIIPER-Evil") {
		t.Fatal("accepted a different Task Scheduler name")
	}
	if err := validateNativeScheduledTaskState(true, false); err == nil {
		t.Fatal("accepted active-but-disabled task snapshot")
	}
	for _, state := range [][2]bool{{false, false}, {false, true}, {true, true}} {
		if err := validateNativeScheduledTaskState(state[0], state[1]); err != nil {
			t.Fatalf("active=%v enabled=%v error=%v", state[0], state[1], err)
		}
	}
}

func TestNativeServiceExecutableCommandMustRemainRepresentable(t *testing.T) {
	got, err := nativeServiceExecutableFromCommandLine(`"C:\Program Files\VIIPER\viiper.exe" service --key-file C:\key`)
	if err != nil || got != `C:\Program Files\VIIPER\viiper.exe` {
		t.Fatalf("executable=%q error=%v", got, err)
	}
	for _, commandLine := range []string{"", "viiper.exe service", "\x00"} {
		if _, err := nativeServiceExecutableFromCommandLine(commandLine); err == nil {
			t.Fatalf("accepted unsafe service command line %q", commandLine)
		}
	}
}

func TestInstallingUserSelectionPrefersInteractiveOriginAndFailsClosedForSystem(t *testing.T) {
	selected, err := selectNativeInstallingUserSID(
		"S-1-5-21-1-2-3-500", false,
		"S-1-5-21-1-2-3-1001", nil,
	)
	if err != nil || selected != "S-1-5-21-1-2-3-1001" {
		t.Fatalf("over-the-shoulder selection=%q error=%v", selected, err)
	}
	if _, err := selectNativeInstallingUserSID(
		"", true, "", errors.New("no active console"),
	); err == nil || !strings.Contains(err.Error(), "--target-user-sid") {
		t.Fatalf("LocalSystem without origin did not fail closed: %v", err)
	}
}

func TestNativeInstallRejectsUntrustedExecutableBeforeAnyMutation(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	dependencies.lockExecutable = func(string) (func(), error) {
		return nil, errors.New("user-writable path")
	}
	credentialProvisioned := false
	dependencies.provisionCredential = func() (nativeCredential, error) {
		credentialProvisioned = true
		return nativeCredential{}, nil
	}
	err := installNativeBrokerTransaction(
		context.Background(), testLogger(), `C:\Users\user\viiper.exe`, dependencies,
	)
	if err == nil || !strings.Contains(err.Error(), "user-writable path") {
		t.Fatalf("error=%v", err)
	}
	if credentialProvisioned || len(events) != 0 {
		t.Fatalf("unsafe executable mutated state: credential=%v events=%v", credentialProvisioned, events)
	}
}

func TestNativeInstallLocksPriorServiceExecutableBeforeMutation(t *testing.T) {
	events := []string{}
	service := &fakeNativeService{
		config: mgr.Config{
			ServiceStartName: nativeServiceAccount,
			BinaryPathName:   `"C:\Untrusted\viiper.exe" service`,
		},
		status: svc.Status{State: svc.Stopped}, events: &events,
	}
	manager := newFakeNativeSCM(service, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	var currentValidatedPaths []string
	dependencies.lockExecutable = func(path string) (func(), error) {
		currentValidatedPaths = append(currentValidatedPaths, path)
		return func() {}, nil
	}
	var validatedPaths []string
	dependencies.lockPriorExecutable = func(path string) (func(), error) {
		validatedPaths = append(validatedPaths, path)
		if strings.EqualFold(path, `C:\Untrusted\viiper.exe`) {
			return nil, errors.New("prior service path is not protected")
		}
		return func() {}, nil
	}
	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err == nil || !strings.Contains(err.Error(), "prior service path is not protected") {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(events, []string{"service-open"}) {
		t.Fatalf("untrusted prior service path mutated transaction state: %v", events)
	}
	if !reflect.DeepEqual(currentValidatedPaths, []string{`C:\Program Files\VIIPER\viiper.exe`}) ||
		!reflect.DeepEqual(validatedPaths, []string{`C:\Untrusted\viiper.exe`}) {
		t.Fatalf("prior proof did not remain isolated: current=%v prior=%v", currentValidatedPaths, validatedPaths)
	}
}

func TestNativeServiceExecutableTrustPathIsReadOnlyByConstruction(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	sourcePath := filepath.Join(filepath.Dir(testFile), "native_service_install_windows.go")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "func lockNativeServiceExecutableReadOnly(")
	end := strings.Index(text, "func nativeServiceExecutableParent(")
	if start < 0 || end <= start {
		t.Fatal("cannot isolate native executable trust implementation")
	}
	span := text[start:end]
	for _, forbidden := range []string{"applyNativeACLToHandle", "SetSecurityInfo", "WRITE_DAC", "WRITE_OWNER"} {
		if strings.Contains(span, forbidden) {
			t.Fatalf("read-only executable trust path contains mutating primitive %q", forbidden)
		}
	}
}

func TestNativeInstallRejectsWeakPriorServiceSecurityBeforeMutation(t *testing.T) {
	events := []string{}
	const priorSecurity = "O:BAD:(A;;GA;;;SY)(A;;GA;;;BA)(A;;RPWP;;;BU)"
	service := &fakeNativeService{
		config: mgr.Config{
			ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, StartType: mgr.StartAutomatic,
			ServiceStartName: nativeServiceAccount,
			BinaryPathName:   `"C:\Program Files\VIIPER\viiper.exe" service`,
		},
		securityDescriptor: priorSecurity,
		status:             svc.Status{State: svc.Running},
		events:             &events,
	}
	manager := newFakeNativeSCM(service, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	var evidence nativeBrokerInstallEvidence
	err := installNativeBrokerTransactionWithEvidence(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies, &evidence)
	if err == nil || !strings.Contains(err.Error(), "untrusted service security descriptor") {
		t.Fatalf("error=%v", err)
	}
	if service.securityDescriptor != priorSecurity {
		t.Fatalf("weak prior service DACL was mutated: %q", service.securityDescriptor)
	}
	if service.status.State != svc.Running {
		t.Fatalf("weak prior service was stopped during rejected snapshot: %+v", service.status)
	}
	if !reflect.DeepEqual(events, []string{"service-open"}) {
		t.Fatalf("weak prior service caused mutation before rejection: %v", events)
	}
	if evidence.mutationStarted || evidence.rollbackSucceeded {
		t.Fatalf("preflight rejection reported mutation evidence: %+v", evidence)
	}
}

func TestNativeTransactionContextBoundsLegacyProviderCalls(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	dependencies.snapshotLegacy = func(ctx context.Context) (nativeLegacyState, error) {
		<-ctx.Done()
		return nativeLegacyState{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := installNativeBrokerTransaction(ctx, testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled legacy provider exceeded bound: %s", elapsed)
	}
}

func TestNativeInstallKeepsLegacyRegistrationUntilAuthenticatedReady(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	legacy := nativeLegacyState{
		runValue: nativeRunRegistrationPointer(`"C:\Legacy\viiper.exe" server --transport usbip`, registry.SZ),
		commands: []nativeLegacyCommand{{executable: `C:\Legacy\viiper.exe`}},
	}
	rolledBackCredential := false
	dependencies := fakeNativeInstallDependencies(manager, legacy, &events)
	dependencies.rollbackCredential = func(nativeCredential) error {
		rolledBackCredential = true
		return nil
	}
	dependencies.stopLegacy = func(_ context.Context, state *nativeLegacyState, _ *slog.Logger) error {
		events = append(events, "legacy-stop")
		state.commands[0].running = true
		return nil
	}
	dependencies.verifyBroker = func(_ context.Context, password string) error {
		events = append(events, "verify")
		if password != "credential" {
			t.Fatalf("password=%q", password)
		}
		if manager.service == nil || manager.service.status.State != svc.Running {
			t.Fatal("service was not running during authenticated verification")
		}
		return nil
	}
	dependencies.removeLegacy = func(_ context.Context, state nativeLegacyState) error {
		events = append(events, "legacy-remove")
		if state.runValue == nil {
			t.Fatal("legacy registration was not retained through verification")
		}
		return nil
	}

	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBackCredential {
		t.Fatal("committed credential was rolled back")
	}
	if !beforeEvent(events, "verify", "legacy-remove") {
		t.Fatalf("legacy registration was removed before authenticated verification: %v", events)
	}
	if manager.service == nil || manager.service.deleted {
		t.Fatal("native service was not retained")
	}
	if manager.service.config.StartType != mgr.StartAutomatic {
		t.Fatal("native service was not registered for automatic startup")
	}
	if !reflect.DeepEqual(manager.service.recoveryActions, nativeServiceRecoveryActions) ||
		manager.service.recoveryReset != nativeServiceRecoveryResetSecond ||
		!manager.service.recoverNonCrash {
		t.Fatalf("bounded recovery policy not applied: %+v", manager.service)
	}
}

func TestNativeInstallHoldsExecutableLockThroughAuthenticatedReady(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	released := false
	dependencies.lockExecutable = func(string) (func(), error) {
		events = append(events, "executable-lock")
		return func() {
			released = true
			events = append(events, "executable-release")
		}, nil
	}
	dependencies.verifyBroker = func(context.Context, string) error {
		if released {
			t.Fatal("service executable lock was released before authenticated readiness")
		}
		events = append(events, "verify")
		return nil
	}
	if err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies); err != nil {
		t.Fatal(err)
	}
	if !released || !beforeEvent(events, "verify", "executable-release") {
		t.Fatalf("executable handle lifetime was not transactional: %v", events)
	}
}

func TestNativeInstallReverifiesAfterLegacyRemovalAndRestoresOnFailure(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	legacy := nativeLegacyState{runValue: nativeRunRegistrationPointer(`"C:\Legacy\viiper.exe" server`, registry.SZ)}
	dependencies := fakeNativeInstallDependencies(manager, legacy, &events)
	verifications := 0
	dependencies.verifyBroker = func(context.Context, string) error {
		verifications++
		events = append(events, "verify")
		if verifications == 2 {
			return errors.New("legacy owner raced endpoint")
		}
		return nil
	}
	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err == nil || !strings.Contains(err.Error(), "legacy owner raced endpoint") {
		t.Fatalf("error=%v", err)
	}
	if verifications != 2 || !beforeEvent(events, "legacy-remove", "legacy-restore") {
		t.Fatalf("post-removal verification/rollback events=%v", events)
	}
	if manager.service == nil || !manager.service.deleted {
		t.Fatalf("failed migration retained replacement service: %+v", manager.service)
	}
}

func TestNativeInstallRejectsBrokerThatStopsAfterAuthenticatedPing(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	dependencies.verifyBroker = func(context.Context, string) error {
		events = append(events, "verify")
		manager.service.status.State = svc.Stopped
		manager.service.processID = 0
		return nil
	}
	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err == nil || !strings.Contains(err.Error(), "left Running state") {
		t.Fatalf("error=%v events=%v", err, events)
	}
	if manager.service == nil || !manager.service.deleted {
		t.Fatalf("stopped impersonable broker was committed: %+v", manager.service)
	}
}

func TestNativeInstallRestoresPriorServiceAndLegacyProcessOnPingFailure(t *testing.T) {
	events := []string{}
	priorConfig := mgr.Config{
		ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, StartType: mgr.StartManual,
		ErrorControl: mgr.ErrorIgnore, BinaryPathName: `"C:\Old\viiper.exe" service`,
		ServiceStartName: nativeServiceAccount, DisplayName: "Prior VIIPER",
	}
	priorRecovery := []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: time.Minute}, {Type: mgr.NoAction}}
	service := &fakeNativeService{
		config: priorConfig, status: svc.Status{State: svc.Running},
		recoveryActions: priorRecovery, recoveryReset: 321, recoverNonCrash: false,
		events: &events,
	}
	manager := newFakeNativeSCM(service, &events)
	legacy := nativeLegacyState{commands: []nativeLegacyCommand{{executable: `C:\Legacy\viiper.exe`}}}
	credentialRolledBack := false
	legacyRestarted := false
	dependencies := fakeNativeInstallDependencies(manager, legacy, &events)
	dependencies.rollbackCredential = func(nativeCredential) error {
		events = append(events, "credential-restore")
		credentialRolledBack = true
		return nil
	}
	dependencies.stopLegacy = func(_ context.Context, state *nativeLegacyState, _ *slog.Logger) error {
		events = append(events, "legacy-stop")
		state.commands[0].running = true
		return nil
	}
	dependencies.restartLegacy = func(_ context.Context, state nativeLegacyState) error {
		events = append(events, "legacy-restart")
		legacyRestarted = hasRunningLegacyCommand(state)
		return nil
	}
	dependencies.verifyBroker = func(context.Context, string) error {
		events = append(events, "verify-failed")
		return errors.New("wrong ABI")
	}

	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err == nil || !strings.Contains(err.Error(), "wrong ABI") {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(service.config, priorConfig) {
		t.Fatalf("prior config was not restored: %+v", service.config)
	}
	if service.status.State != svc.Running {
		t.Fatalf("prior running state was not restored: %v", service.status.State)
	}
	if !reflect.DeepEqual(service.recoveryActions, priorRecovery) || service.recoveryReset != 321 || service.recoverNonCrash {
		t.Fatalf("prior recovery policy was not restored: %+v", service)
	}
	if !legacyRestarted || !credentialRolledBack {
		t.Fatalf("rollback incomplete: legacy=%v credential=%v", legacyRestarted, credentialRolledBack)
	}
	if beforeEvent(events, "legacy-remove", "verify-failed") {
		t.Fatalf("legacy startup changed before a failed verification: %v", events)
	}
	credentialIndex := slices.Index(events, "credential-restore")
	priorStartIndex := lastIndex(events, "service-start")
	legacyRestartIndex := slices.Index(events, "legacy-restart")
	if credentialIndex < 0 || priorStartIndex <= credentialIndex || legacyRestartIndex <= priorStartIndex {
		t.Fatalf("rollback did not restore credential before prior owners: %v", events)
	}
}

func TestNativeInstallDeletesNewServiceWhenMigrationFails(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	dependencies.verifyBroker = func(context.Context, string) error { return errors.New("not ready") }

	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err == nil {
		t.Fatal("expected verification failure")
	}
	if manager.service == nil || !manager.service.deleted {
		t.Fatal("new service was not deleted during rollback")
	}
	if manager.service.status.State != svc.Stopped {
		t.Fatalf("new service was not stopped before deletion: %v", manager.service.status.State)
	}
}

func TestNativeInstallDeletesNewServiceAfterOptionalConfigFailure(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	manager.newServiceFailUpdate = errors.New("optional service config failed")
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err == nil || !strings.Contains(err.Error(), "optional service config failed") {
		t.Fatalf("error=%v", err)
	}
	if manager.service == nil || !manager.service.deleted {
		t.Fatalf("partially configured new service was orphaned: events=%v", events)
	}
}

func TestNativeInstallRestoresAfterPartialUpdateConfigFailure(t *testing.T) {
	events := []string{}
	prior := mgr.Config{
		ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, StartType: mgr.StartManual,
		ServiceStartName: nativeServiceAccount, BinaryPathName: `"C:\Prior\viiper.exe" service`,
	}
	service := &fakeNativeService{
		config: prior, status: svc.Status{State: svc.Stopped}, events: &events,
		failUpdate: errors.New("optional config failed after base config changed"),
	}
	manager := newFakeNativeSCM(service, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	updateCalls := 0
	service.updateHook = func() {
		updateCalls++
		if updateCalls == 2 {
			service.failUpdate = nil
		}
	}

	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err == nil {
		t.Fatal("expected partial UpdateConfig failure")
	}
	if !reflect.DeepEqual(service.config, prior) {
		t.Fatalf("partially changed service config was not restored: %+v", service.config)
	}
}

func TestNativeInstallRestoresRunningServiceAfterStopWaitFails(t *testing.T) {
	events := []string{}
	prior := mgr.Config{
		ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, StartType: mgr.StartAutomatic,
		ServiceStartName: nativeServiceAccount, BinaryPathName: `"C:\Program Files\VIIPER\viiper.exe" service`,
	}
	service := &fakeNativeService{
		config: prior, status: svc.Status{State: svc.Running}, events: &events,
		failControl: errors.New("status wait failed after stop was accepted"),
	}
	manager := newFakeNativeSCM(service, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	controlCalls := 0
	service.controlHook = func() {
		controlCalls++
		if controlCalls == 2 {
			service.failControl = nil
		}
	}

	var evidence nativeBrokerInstallEvidence
	err := installNativeBrokerTransactionWithEvidence(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies, &evidence)
	if err == nil {
		t.Fatal("expected the forward stop failure")
	}
	if service.status.State != svc.Running || service.startCalls != 1 {
		t.Fatalf("prior running state was not reconciled: status=%v starts=%d", service.status.State, service.startCalls)
	}
	if !evidence.mutationStarted || !evidence.rollbackSucceeded {
		t.Fatalf("settled rollback evidence=%+v", evidence)
	}
}

func TestNativeRollbackDoesNotStartServiceAfterConfigRestoreFailure(t *testing.T) {
	events := []string{}
	service := &fakeNativeService{
		config: mgr.Config{
			ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, StartType: mgr.StartAutomatic,
			ServiceStartName: nativeServiceAccount, BinaryPathName: `"C:\Old\viiper.exe" service`,
		},
		status: svc.Status{State: svc.Running}, events: &events,
		failUpdate: errors.New("configuration write failed"),
	}
	manager := newFakeNativeSCM(service, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	credentialRolledBack := false
	dependencies.rollbackCredential = func(nativeCredential) error {
		credentialRolledBack = true
		return nil
	}
	var evidence nativeBrokerInstallEvidence
	err := installNativeBrokerTransactionWithEvidence(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies, &evidence)
	if err == nil {
		t.Fatal("expected update and rollback failure")
	}
	if service.startCalls != 0 || service.status.State != svc.Stopped {
		t.Fatalf("service started with unverified config: starts=%d state=%v", service.startCalls, service.status.State)
	}
	if credentialRolledBack {
		t.Fatal("credential was invalidated while the replacement service configuration remained installed")
	}
	if !evidence.mutationStarted || evidence.rollbackSucceeded {
		t.Fatalf("indeterminate rollback evidence=%+v", evidence)
	}
}

func TestNativeInstallRejectsUnrepresentableRecoveryPolicyBeforeMutation(t *testing.T) {
	events := []string{}
	service := &fakeNativeService{
		config: mgr.Config{
			ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, StartType: mgr.StartManual,
			ServiceStartName: nativeServiceAccount, BinaryPathName: `"C:\Old\viiper.exe" service`,
		},
		status: svc.Status{State: svc.Stopped}, events: &events,
		recoveryActions: nil, recoveryReset: 777,
	}
	manager := newFakeNativeSCM(service, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err == nil || !strings.Contains(err.Error(), "unrepresentable recovery policy") {
		t.Fatalf("expected an unrepresentable-policy error, got %v", err)
	}
	if !reflect.DeepEqual(events, []string{"service-open"}) {
		t.Fatalf("unrepresentable policy mutated SCM/legacy state: %v", events)
	}
}

func TestNativeInstallRejectsUnrestorableLoadOrderStateBeforeMutation(t *testing.T) {
	for _, config := range []mgr.Config{
		{ServiceStartName: nativeServiceAccount, LoadOrderGroup: "legacy-group"},
		{ServiceStartName: nativeServiceAccount, TagId: 7},
	} {
		events := []string{}
		service := &fakeNativeService{
			config: config, status: svc.Status{State: svc.Stopped}, events: &events,
		}
		manager := newFakeNativeSCM(service, &events)
		dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
		err := installNativeBrokerTransaction(context.Background(), testLogger(),
			`C:\Program Files\VIIPER\viiper.exe`, dependencies)
		if err == nil || !strings.Contains(err.Error(), "load-order") {
			t.Fatalf("config=%+v error=%v", config, err)
		}
		if !reflect.DeepEqual(events, []string{"service-open"}) {
			t.Fatalf("unrestorable config mutated state: %v", events)
		}
	}
}

func TestNativeInstallWaitsForPriorServiceDeletionBeforeCreating(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	manager.openErrors = []error{
		windows.ERROR_SERVICE_MARKED_FOR_DELETE,
		windows.ERROR_SERVICE_MARKED_FOR_DELETE,
	}
	waits := 0
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	dependencies.wait = func(context.Context, time.Duration) error {
		waits++
		return nil
	}
	if err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies); err != nil {
		t.Fatal(err)
	}
	if waits != 2 || manager.service == nil {
		t.Fatalf("deletion retry waits=%d service=%v events=%v", waits, manager.service, events)
	}
}

func TestNativeInstallRejectsPausedPriorServiceBeforeMutation(t *testing.T) {
	events := []string{}
	service := &fakeNativeService{
		config: mgr.Config{ServiceStartName: nativeServiceAccount},
		status: svc.Status{State: svc.Paused}, events: &events,
	}
	manager := newFakeNativeSCM(service, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies)
	if err == nil || !strings.Contains(err.Error(), "unsupported state") {
		t.Fatalf("error=%v", err)
	}
	if len(events) != 1 || events[0] != "service-open" {
		t.Fatalf("paused service was mutated: %v", events)
	}
}

func TestNativeInstallRejectsEmptyCredentialBeforeServiceConfiguration(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	rolledBack := false
	dependencies.provisionCredential = func() (nativeCredential, error) {
		return nativeCredential{path: `C:\ProgramData\VIIPER\viiper.key.txt`, password: "   ", created: true}, nil
	}
	dependencies.rollbackCredential = func(nativeCredential) error {
		rolledBack = true
		return nil
	}
	if err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies); err == nil {
		t.Fatal("accepted empty credential")
	}
	for _, event := range events {
		if event == "service-create" || event == "service-update" || event == "service-start" {
			t.Fatalf("empty credential changed service configuration: events=%v", events)
		}
	}
	if !rolledBack {
		t.Fatalf("empty credential was not rolled back: events=%v", events)
	}
}

func TestRollbackUsesIndependentContextAfterForwardTimeout(t *testing.T) {
	events := []string{}
	service := &fakeNativeService{
		config: mgr.Config{ServiceStartName: nativeServiceAccount},
		status: svc.Status{State: svc.Running}, events: &events, delayStartAfter: 1,
	}
	manager := newFakeNativeSCM(service, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	ctx, cancel := context.WithCancel(context.Background())
	dependencies.verifyBroker = func(context.Context, string) error {
		cancel()
		return context.Canceled
	}
	rollbackObservedLiveContext := false
	dependencies.wait = func(waitCtx context.Context, _ time.Duration) error {
		if ctx.Err() != nil && waitCtx.Err() == nil {
			rollbackObservedLiveContext = true
		}
		service.status.State = svc.Running
		return nil
	}
	if err := installNativeBrokerTransaction(ctx, testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies); err == nil {
		t.Fatal("expected canceled verification")
	}
	if !rollbackObservedLiveContext {
		t.Fatal("rollback reused the canceled forward-operation context")
	}
}

func TestNativeInstallRollsBackPartialLegacyStop(t *testing.T) {
	events := []string{}
	manager := newFakeNativeSCM(nil, &events)
	legacy := nativeLegacyState{commands: []nativeLegacyCommand{
		{executable: `C:\One\viiper.exe`}, {executable: `C:\Two\viiper.exe`},
	}}
	restarted := false
	dependencies := fakeNativeInstallDependencies(manager, legacy, &events)
	dependencies.stopLegacy = func(_ context.Context, state *nativeLegacyState, _ *slog.Logger) error {
		state.commands[0].running = true
		return errors.New("second process query failed")
	}
	dependencies.restartLegacy = func(_ context.Context, state nativeLegacyState) error {
		restarted = state.commands[0].running
		return nil
	}

	if err := installNativeBrokerTransaction(context.Background(), testLogger(),
		`C:\Program Files\VIIPER\viiper.exe`, dependencies); err == nil {
		t.Fatal("expected stop failure")
	}
	if !restarted {
		t.Fatal("partially stopped legacy process was not restarted")
	}
}

func TestLegacyTaskAndRunOwnershipAreStoppedByTheirOwnMechanisms(t *testing.T) {
	events := []string{}
	command := nativeLegacyCommand{
		executable: `C:\Users\user\VIIPER\viiper.exe`, source: legacyCommandRun,
	}
	state := nativeLegacyState{
		userSID:             "S-1-5-21-1-2-3-1001",
		scheduledAction:     &nativeLegacyCommand{executable: command.executable},
		scheduledXML:        stringPointer("<Task />"),
		scheduledCurrentXML: stringPointer("<Task />"),
		scheduledActive:     true,
		commands:            []nativeLegacyCommand{command},
	}
	operations := nativeLegacyStopOperations{
		stopScheduled: func(_ context.Context, xml string, active bool) (nativeScheduledStopResult, error) {
			events = append(events, "task-stop")
			if xml != "<Task />" || !active {
				t.Fatalf("task snapshot xml=%q active=%v", xml, active)
			}
			return nativeScheduledStopResult{stopped: true, disabled: true, currentXML: "<Task Disabled='true' />"}, nil
		},
		openProcesses: func(executable, userSID string) ([]nativeLegacyProcess, error) {
			events = append(events, "run-process-query")
			if executable != command.executable || userSID != state.userSID {
				t.Fatalf("residual query executable=%q user=%q", executable, userSID)
			}
			return []nativeLegacyProcess{{handle: 123, pid: 456}}, nil
		},
		terminate: func(nativeLegacyProcess) error {
			events = append(events, "run-process-stop")
			return nil
		},
		closeHandle: func(windows.Handle) { events = append(events, "run-process-close") },
	}
	if err := stopNativeLegacyStartupWith(context.Background(), &state, testLogger(), operations); err != nil {
		t.Fatal(err)
	}
	if !state.scheduledStopped || !state.commands[0].running {
		t.Fatalf("source state not preserved: %+v", state)
	}
	want := []string{"task-stop", "run-process-query", "run-process-stop", "run-process-close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("source ordering=%v want=%v", events, want)
	}

	// A task-only registration must not enumerate or terminate an unrelated
	// manual process merely because it shares the scheduled action's path.
	state = nativeLegacyState{
		scheduledAction:     &nativeLegacyCommand{executable: command.executable},
		scheduledXML:        stringPointer("<Task />"),
		scheduledCurrentXML: stringPointer("<Task />"),
	}
	operations.stopScheduled = func(context.Context, string, bool) (nativeScheduledStopResult, error) {
		return nativeScheduledStopResult{currentXML: "<Task />"}, nil
	}
	operations.openProcesses = func(string, string) ([]nativeLegacyProcess, error) {
		t.Fatal("task-only migration enumerated residual same-path processes")
		return nil, nil
	}
	if err := stopNativeLegacyStartupWith(context.Background(), &state, testLogger(), operations); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyTaskStopMarksPossibleDisableBeforeSubprocessResult(t *testing.T) {
	original := "<Task Enabled='true' />"
	state := nativeLegacyState{
		scheduledAction:     &nativeLegacyCommand{executable: `C:\Legacy\viiper.exe`},
		scheduledXML:        &original,
		scheduledCurrentXML: &original,
		scheduledEnabled:    true,
	}
	operations := nativeLegacyStopOperations{
		stopScheduled: func(context.Context, string, bool) (nativeScheduledStopResult, error) {
			return nativeScheduledStopResult{}, context.DeadlineExceeded
		},
		openProcesses: func(string, string) ([]nativeLegacyProcess, error) { return nil, nil },
		terminate:     func(nativeLegacyProcess) error { return nil },
		closeHandle:   func(windows.Handle) {},
	}
	err := stopNativeLegacyStartupWith(context.Background(), &state, testLogger(), operations)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if !state.scheduledDisabled || state.scheduledCurrentXML == nil || *state.scheduledCurrentXML != original {
		t.Fatalf("partial disable was not admitted to rollback state: %+v", state)
	}
}

func TestKilledTaskStopRollbackRestoresOnlyExactDisabledTaskAndRunningState(t *testing.T) {
	const original = `<Task><Settings><Enabled>true</Enabled></Settings><Actions><Exec><Command>C:\Legacy\viiper.exe</Command></Exec></Actions></Task>`
	validDisabled := strings.Replace(original, `<Enabled>true</Enabled>`, `<Enabled>false</Enabled>`, 1)
	for _, test := range []struct {
		name          string
		current       string
		wantRestarted bool
	}{
		{name: "exact disabled task", current: validDisabled, wantRestarted: true},
		{name: "concurrent replacement", current: strings.Replace(validDisabled, `C:\Legacy`, `C:\Evil`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []string{}
			manager := newFakeNativeSCM(nil, &events)
			legacy := nativeLegacyState{
				scheduledAction:  &nativeLegacyCommand{executable: `C:\Legacy\viiper.exe`},
				scheduledXML:     stringPointer(original),
				scheduledActive:  true,
				scheduledEnabled: true,
			}
			dependencies := fakeNativeInstallDependencies(manager, legacy, &events)
			dependencies.stopLegacy = func(_ context.Context, state *nativeLegacyState, _ *slog.Logger) error {
				state.scheduledDisabled = true
				state.scheduledStopped = state.scheduledActive
				return context.DeadlineExceeded
			}
			dependencies.restoreLegacy = func(_ context.Context, state nativeLegacyState) error {
				events = append(events, "legacy-restore")
				return validateNativeTaskDisabledOnly(*state.scheduledXML, test.current)
			}
			restarted := false
			dependencies.restartLegacy = func(_ context.Context, state nativeLegacyState) error {
				events = append(events, "legacy-restart")
				restarted = state.scheduledStopped
				return nil
			}
			err := installNativeBrokerTransaction(context.Background(), testLogger(),
				`C:\Program Files\VIIPER\viiper.exe`, dependencies)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v", err)
			}
			if restarted != test.wantRestarted {
				t.Fatalf("restarted=%v want=%v events=%v error=%v", restarted, test.wantRestarted, events, err)
			}
		})
	}
}

func TestLegacyTaskAlreadyDisabledRemainsValidForNativeOwnership(t *testing.T) {
	original := "<Task Enabled='false' />"
	state := nativeLegacyState{
		scheduledAction:     &nativeLegacyCommand{executable: `C:\Missing\viiper.exe`},
		scheduledXML:        &original,
		scheduledCurrentXML: &original,
	}
	operations := nativeLegacyStopOperations{
		stopScheduled: func(context.Context, string, bool) (nativeScheduledStopResult, error) {
			return nativeScheduledStopResult{disabled: true, currentXML: original}, nil
		},
		openProcesses: func(string, string) ([]nativeLegacyProcess, error) {
			t.Fatal("disabled task action was treated as an active process owner")
			return nil, nil
		},
		terminate:   func(nativeLegacyProcess) error { return nil },
		closeHandle: func(windows.Handle) {},
	}
	if err := stopNativeLegacyStartupWith(context.Background(), &state, testLogger(), operations); err != nil {
		t.Fatal(err)
	}
	if !state.scheduledDisabled || state.scheduledStopped {
		t.Fatalf("pre-disabled task state=%+v", state)
	}
}

func TestNativeUninstallSnapshotsAndRemovesLegacyBeforeServiceDelete(t *testing.T) {
	events := []string{}
	service := &fakeNativeService{
		config: mgr.Config{ServiceStartName: nativeServiceAccount},
		status: svc.Status{State: svc.Running}, events: &events,
	}
	manager := newFakeNativeSCM(service, &events)
	legacy := nativeLegacyState{userSID: "S-1-5-21-1-2-3-1001"}
	dependencies := fakeNativeInstallDependencies(manager, legacy, &events)
	dependencies.snapshotLegacy = func(context.Context) (nativeLegacyState, error) {
		events = append(events, "legacy-snapshot")
		return legacy, nil
	}
	if err := uninstallNativeBrokerTransaction(
		context.Background(), testLogger(), manager, dependencies,
	); err != nil {
		t.Fatal(err)
	}
	if !service.deleted {
		t.Fatal("native service was not deleted")
	}
	if !beforeEvent(events, "legacy-snapshot", "service-stop") ||
		!beforeEvent(events, "legacy-remove", "service-delete") {
		t.Fatalf("uninstall transaction order=%v", events)
	}
}

func TestNativeUninstallRollsBackServiceRegistrationsAndProcessOnDeleteFailure(t *testing.T) {
	events := []string{}
	service := &fakeNativeService{
		config: mgr.Config{ServiceStartName: nativeServiceAccount},
		status: svc.Status{State: svc.Running}, events: &events,
		failDelete: errors.New("delete failed"),
	}
	manager := newFakeNativeSCM(service, &events)
	legacy := nativeLegacyState{
		userSID: "S-1-5-21-1-2-3-1001",
		commands: []nativeLegacyCommand{{
			executable: `C:\Legacy\viiper.exe`, source: legacyCommandRun,
		}},
	}
	dependencies := fakeNativeInstallDependencies(manager, legacy, &events)
	dependencies.stopLegacy = func(_ context.Context, state *nativeLegacyState, _ *slog.Logger) error {
		events = append(events, "legacy-stop")
		state.commands[0].running = true
		return nil
	}
	err := uninstallNativeBrokerTransaction(context.Background(), testLogger(), manager, dependencies)
	if err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("error=%v", err)
	}
	if service.deleted || service.status.State != svc.Running {
		t.Fatalf("service rollback state=%v deleted=%v", service.status.State, service.deleted)
	}
	if !beforeEvent(events, "service-start", "legacy-restore") ||
		!beforeEvent(events, "legacy-restore", "legacy-restart") {
		t.Fatalf("uninstall rollback order=%v", events)
	}
}

func TestNativeUninstallRejectsPausedServiceBeforeMutation(t *testing.T) {
	events := []string{}
	service := &fakeNativeService{
		config: mgr.Config{ServiceStartName: nativeServiceAccount},
		status: svc.Status{State: svc.Paused}, events: &events,
	}
	manager := newFakeNativeSCM(service, &events)
	dependencies := fakeNativeInstallDependencies(manager, nativeLegacyState{}, &events)
	err := uninstallNativeBrokerTransaction(context.Background(), testLogger(), manager, dependencies)
	if err == nil || !strings.Contains(err.Error(), "unsupported state") {
		t.Fatalf("error=%v", err)
	}
	if service.deleted || slices.Contains(events, "service-stop") {
		t.Fatalf("paused service was mutated: %v", events)
	}
}

func TestValidateNativeBrokerPingRequiresExactContract(t *testing.T) {
	expected, err := udecx.DeriveBuildIdentity(
		strings.Repeat("a", 40), udecx.DriverPackageVersion,
		udecx.ABIMajor, udecx.ABIMinor, udecx.AdvertisedCapabilities,
	)
	if err != nil {
		t.Fatal(err)
	}
	ready := true
	valid := &viipertypes.PingResponse{
		Server: "VIIPER", Transport: "native-ude", Ready: &ready,
		NativeUDE: &viipertypes.NativeUDEInfo{
			ABIMajor: udecx.ABIMajor, ABIMinor: udecx.ABIMinor,
			Capabilities:                 uint32(udecx.AdvertisedCapabilities),
			ExpectedDriverPackageVersion: udecx.DriverPackageVersion,
			LoadedDriverBuildIdentity:    udecx.BuildIdentityHex(expected),
		},
	}
	if err := validateNativeBrokerPingAgainstIdentity(valid, expected); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*viipertypes.PingResponse){
		"not ready":  func(p *viipertypes.PingResponse) { value := false; p.Ready = &value },
		"wrong ABI":  func(p *viipertypes.PingResponse) { p.NativeUDE.ABIMinor++ },
		"extra caps": func(p *viipertypes.PingResponse) { p.NativeUDE.Capabilities |= uint32(udecx.CapabilityStreams) },
		"wrong package version": func(p *viipertypes.PingResponse) {
			p.NativeUDE.ExpectedDriverPackageVersion = "0.1.0.3"
		},
		"missing loaded identity": func(p *viipertypes.PingResponse) {
			p.NativeUDE.LoadedDriverBuildIdentity = ""
		},
		"malformed loaded identity": func(p *viipertypes.PingResponse) {
			p.NativeUDE.LoadedDriverBuildIdentity = strings.Repeat("z", 64)
		},
		"stale loaded identity with matching ABI and caps": func(p *viipertypes.PingResponse) {
			p.NativeUDE.LoadedDriverBuildIdentity = strings.Repeat("0", 64)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			copyResponse := *valid
			copyNative := *valid.NativeUDE
			copyResponse.NativeUDE = &copyNative
			mutate(&copyResponse)
			if err := validateNativeBrokerPingAgainstIdentity(&copyResponse, expected); err == nil {
				t.Fatal("expected exact-contract rejection")
			}
		})
	}
}

func TestValidateNativeBrokerPingFailsClosedWithoutBuildInjection(t *testing.T) {
	if _, err := udecx.ExpectedBuildIdentity(); err == nil {
		t.Skip("test binary has an explicitly injected native source revision")
	}
	if err := validateNativeBrokerPing(nil); !errors.Is(err, udecx.ErrBuildIdentity) {
		t.Fatalf("error=%v want ErrBuildIdentity", err)
	}
}

func TestValidateNativeBrokerPingUsesInjectedBuildIdentity(t *testing.T) {
	expected, err := udecx.ExpectedBuildIdentity()
	if err != nil {
		t.Skip("test binary has no injected native source revision")
	}
	ready := true
	response := &viipertypes.PingResponse{
		Server: "VIIPER", Transport: "native-ude", Ready: &ready,
		NativeUDE: &viipertypes.NativeUDEInfo{
			ABIMajor: udecx.ABIMajor, ABIMinor: udecx.ABIMinor,
			Capabilities:                 uint32(udecx.AdvertisedCapabilities),
			ExpectedDriverPackageVersion: udecx.DriverPackageVersion,
			LoadedDriverBuildIdentity:    udecx.BuildIdentityHex(expected),
		},
	}
	if err := validateNativeBrokerPing(response); err != nil {
		t.Fatal(err)
	}
	response.NativeUDE.LoadedDriverBuildIdentity = strings.Repeat("0", 64)
	if err := validateNativeBrokerPing(response); err == nil {
		t.Fatal("authenticated readiness accepted a stale same-ABI/capability loaded kernel")
	}
}

func TestCredentialACLUsesSIDsRatherThanLocalizedAccountNames(t *testing.T) {
	const userSID = "S-1-5-21-1-2-3-1001"
	for _, sddl := range []string{nativeCredentialDirectorySDDL(userSID), nativeCredentialFileSDDL(userSID)} {
		if !strings.Contains(sddl, ";;;SY") || !strings.Contains(sddl, ";;;BA") || !strings.Contains(sddl, userSID) {
			t.Fatalf("ACL does not explicitly name SYSTEM, administrators, and installing user by SID: %s", sddl)
		}
		if _, err := windows.SecurityDescriptorFromString(sddl); err != nil {
			t.Fatalf("invalid SDDL %q: %v", sddl, err)
		}
	}
}

func TestCredentialDirectorySecurityRejectsPrecreatedOwnerOrDACL(t *testing.T) {
	const userSID = "S-1-5-21-1-2-3-1001"
	expected, err := windows.SecurityDescriptorFromString(nativeCredentialDirectorySDDL(userSID))
	if err != nil {
		t.Fatal(err)
	}
	identical, _ := windows.SecurityDescriptorFromString(nativeCredentialDirectorySDDL(userSID))
	if err := nativeSecurityDescriptorsEqual(identical, expected, nativeFileAccessMapping); err != nil {
		t.Fatalf("exact protected descriptor rejected: %v", err)
	}
	wrongOwner, _ := windows.SecurityDescriptorFromString(
		"O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;" + userSID + ")",
	)
	if err := nativeSecurityDescriptorsEqual(wrongOwner, expected, nativeFileAccessMapping); err == nil {
		t.Fatal("accepted user-precreated credential directory with wrong owner")
	}
	unprotected, _ := windows.SecurityDescriptorFromString(
		"O:BAD:(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;" + userSID + ")",
	)
	if err := nativeSecurityDescriptorsEqual(unprotected, expected, nativeFileAccessMapping); err == nil {
		t.Fatal("accepted credential directory without protected canonical DACL")
	}
}

func TestNativeFileSecurityComparisonAcceptsWindowsMaterializedGenericRights(t *testing.T) {
	expected, err := windows.SecurityDescriptorFromString(nativeBrokerDirectorySDDL)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := windows.SecurityDescriptorFromString(
		"O:BAG:S-1-5-21-1-2-3-1001D:P" +
			"(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := nativeSecurityDescriptorsEqual(actual, expected, nativeFileAccessMapping); err != nil {
		t.Fatalf("rejected exact Windows-materialized file DACL: %v", err)
	}
}

func TestNativeFileSecurityComparisonRoundTripsThroughObjectManager(t *testing.T) {
	path := filepath.Join(t.TempDir(), "protected")
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ownerSID := user.User.Sid.String()
	if ownerSID == "" {
		t.Fatal("current process token returned an empty owner SID")
	}
	roundTripSDDL := strings.Replace(nativeBrokerDirectorySDDL, "O:BA", "O:"+ownerSID, 1)
	security, err := nativeSecurityAttributes(roundTripSDDL)
	if err != nil {
		t.Fatal(err)
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CreateDirectory(pointer, security); err != nil {
		t.Fatal(err)
	}
	handle, err := openNativePathWithoutReparse(
		path, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	if err := validateNativeSecurityDescriptor(handle, roundTripSDDL); err != nil {
		actual, queryErr := windows.GetSecurityInfo(
			handle, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
		)
		if queryErr != nil {
			t.Fatalf("round-trip rejected (%v), then query failed: %v", err, queryErr)
		}
		t.Fatalf("round-trip rejected: %v (actual=%s)", err, actual.String())
	}
}

func TestNativeSecurityComparisonRejectsWidenedOrNonAllowDACLs(t *testing.T) {
	expected, err := windows.SecurityDescriptorFromString(nativeBrokerDirectorySDDL)
	if err != nil {
		t.Fatal(err)
	}
	for name, sddl := range map[string]string{
		"widened":  "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;BU)",
		"narrowed": "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GR;;;BU)",
		"wrong_sid": "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" +
			"(A;OICI;GRGX;;;WD)",
		"wrong_flags": "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" +
			"(A;OI;GRGX;;;BU)",
		"inherited": "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" +
			"(A;OICIID;GRGX;;;BU)",
		"reordered": "O:BAD:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)" +
			"(A;OICI;GRGX;;;BU)",
		"extra": "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" +
			"(A;OICI;GRGX;;;BU)(A;OICI;GR;;;WD)",
		"deny": "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(D;OICI;GW;;;BU)" +
			"(A;OICI;GRGX;;;BU)",
	} {
		t.Run(name, func(t *testing.T) {
			actual, parseErr := windows.SecurityDescriptorFromString(sddl)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if err := nativeSecurityDescriptorsEqual(
				actual, expected, nativeFileAccessMapping,
			); err == nil {
				t.Fatal("accepted a non-exact protected DACL")
			}
		})
	}
}

func TestNativeSecurityComparisonRejectsMissingNullOrDefaultedDACL(t *testing.T) {
	expected, err := windows.SecurityDescriptorFromString(nativeBrokerDirectorySDDL)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := expected.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := expected.DACL()
	if err != nil {
		t.Fatal(err)
	}
	for name, build := range map[string]func(*windows.SECURITY_DESCRIPTOR) error{
		"missing": func(descriptor *windows.SECURITY_DESCRIPTOR) error {
			return descriptor.SetDACL(nil, false, false)
		},
		"null": func(descriptor *windows.SECURITY_DESCRIPTOR) error {
			return descriptor.SetDACL(nil, true, false)
		},
		"defaulted": func(descriptor *windows.SECURITY_DESCRIPTOR) error {
			return descriptor.SetDACL(dacl, true, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			actual, newErr := windows.NewSecurityDescriptor()
			if newErr != nil {
				t.Fatal(newErr)
			}
			if err := actual.SetOwner(owner, false); err != nil {
				t.Fatal(err)
			}
			if err := build(actual); err != nil {
				t.Fatal(err)
			}
			if err := actual.SetControl(
				windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED,
			); err != nil {
				t.Fatal(err)
			}
			if err := nativeSecurityDescriptorsEqual(
				actual, expected, nativeFileAccessMapping,
			); err == nil {
				t.Fatal("accepted missing, NULL, or defaulted DACL")
			}
		})
	}
}

func TestNativeServiceSecurityComparisonMapsGenericAll(t *testing.T) {
	expected, err := windows.SecurityDescriptorFromString(nativeBrokerServiceSDDL)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := windows.SecurityDescriptorFromString(
		"O:BAG:S-1-5-21-1-2-3-1001D:P(A;;0xf01ff;;;SY)(A;;0xf01ff;;;BA)",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := nativeSecurityDescriptorsEqual(actual, expected, nativeServiceAccessMapping); err != nil {
		t.Fatalf("rejected exact Windows-materialized service DACL: %v", err)
	}
}

func TestParseWindowsCommandRejectsNonViiperAndPreservesArguments(t *testing.T) {
	identity := func(value string) (string, error) { return value, nil }
	command, err := parseWindowsCommand(
		`"C:\Program Files\VIIPER\viiper.exe" server --log.file "C:\logs\native log.txt"`,
		identity,
	)
	if err != nil {
		t.Fatal(err)
	}
	if command.executable != `C:\Program Files\VIIPER\viiper.exe` ||
		!reflect.DeepEqual(command.arguments, []string{"server", "--log.file", `C:\logs\native log.txt`}) {
		t.Fatalf("command=%+v", command)
	}
	if _, err := parseWindowsCommand(`"C:\Windows\System32\cmd.exe" /c calc`, identity); err == nil {
		t.Fatal("accepted a non-VIIPER startup command")
	}
	command, err = parseWindowsCommand(`"%LOCALAPPDATA%\VIIPER\viiper.exe" server`, func(value string) (string, error) {
		return strings.ReplaceAll(value, `%LOCALAPPDATA%`, `C:\Users\target\AppData\Local`), nil
	})
	if err != nil || command.executable != `C:\Users\target\AppData\Local\VIIPER\viiper.exe` {
		t.Fatalf("target-user expansion command=%+v error=%v", command, err)
	}
}

func TestNativeUserRunKeyPathUsesExplicitSIDHive(t *testing.T) {
	got, err := nativeUserRunKeyPath("S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	want := `S-1-5-21-1-2-3-1001\` + runKeyPath
	if got != want {
		t.Fatalf("run key=%q want=%q", got, want)
	}
	for _, invalid := range []string{"", `S-1-5-21\Software`, "not-a-sid"} {
		if _, err := nativeUserRunKeyPath(invalid); err == nil {
			t.Fatalf("accepted invalid user SID %q", invalid)
		}
	}
}

type fakeNativeSCM struct {
	service              *fakeNativeService
	events               *[]string
	newServiceFailUpdate error
	openErrors           []error
}

func newFakeNativeSCM(service *fakeNativeService, events *[]string) *fakeNativeSCM {
	if service != nil {
		if service.config.BinaryPathName == "" {
			service.config.BinaryPathName = `"C:\Program Files\VIIPER\viiper.exe" service`
		}
		service.events = events
	}
	return &fakeNativeSCM{service: service, events: events}
}

func (m *fakeNativeSCM) OpenService(string) (nativeManagedService, error) {
	*m.events = append(*m.events, "service-open")
	if len(m.openErrors) != 0 {
		err := m.openErrors[0]
		m.openErrors = m.openErrors[1:]
		return nil, err
	}
	if m.service == nil || m.service.deleted {
		return nil, windows.ERROR_SERVICE_DOES_NOT_EXIST
	}
	return m.service, nil
}

func (m *fakeNativeSCM) CreateService(_ string, executable string, config mgr.Config, args ...string) (nativeManagedService, error) {
	*m.events = append(*m.events, "service-create")
	if m.service != nil && !m.service.deleted {
		return nil, windows.ERROR_SERVICE_EXISTS
	}
	commandLine, err := windowsCommandLine(executable, args...)
	if err != nil {
		return nil, err
	}
	config.BinaryPathName = commandLine
	m.service = &fakeNativeService{
		config: config, status: svc.Status{State: svc.Stopped}, events: m.events,
		failUpdate: m.newServiceFailUpdate,
	}
	return m.service, nil
}

func (m *fakeNativeSCM) Close() error { return nil }

type fakeNativeService struct {
	config             mgr.Config
	securityDescriptor string
	status             svc.Status
	recoveryActions    []mgr.RecoveryAction
	recoveryReset      uint32
	recoverNonCrash    bool
	deleted            bool
	events             *[]string
	failUpdate         error
	failSecurity       error
	failRecovery       error
	failRecoveryFlag   error
	failControl        error
	failDelete         error
	updateHook         func()
	controlHook        func()
	processID          uint32
	startCalls         int
	delayStartAfter    int
}

func (s *fakeNativeService) Config() (mgr.Config, error) { return s.config, nil }
func (s *fakeNativeService) UpdateConfig(config mgr.Config) error {
	*s.events = append(*s.events, "service-update")
	// Model x/sys' multi-call behavior: the base service configuration may be
	// committed before an optional service setting reports failure.
	s.config = config
	if s.updateHook != nil {
		s.updateHook()
	}
	return s.failUpdate
}
func (s *fakeNativeService) SecurityDescriptor() (string, error) {
	if s.securityDescriptor == "" {
		return nativeBrokerServiceSDDL, nil
	}
	return s.securityDescriptor, nil
}
func (s *fakeNativeService) SetSecurityDescriptor(sddl string) error {
	*s.events = append(*s.events, "service-security")
	if s.failSecurity != nil {
		return s.failSecurity
	}
	s.securityDescriptor = sddl
	return nil
}
func (s *fakeNativeService) Query() (svc.Status, error) { return s.status, nil }
func (s *fakeNativeService) ProcessID() (uint32, error) {
	if s.processID != 0 {
		return s.processID, nil
	}
	if s.status.State == svc.Running {
		return 4242, nil
	}
	return 0, nil
}
func (s *fakeNativeService) Start(...string) error {
	*s.events = append(*s.events, "service-start")
	s.startCalls++
	if s.delayStartAfter != 0 && s.startCalls > s.delayStartAfter {
		s.status.State = svc.StartPending
	} else {
		s.status.State = svc.Running
	}
	return nil
}
func (s *fakeNativeService) Control(command svc.Cmd) (svc.Status, error) {
	if command != svc.Stop {
		return s.status, errors.New("unsupported fake control")
	}
	*s.events = append(*s.events, "service-stop")
	s.status.State = svc.Stopped
	if s.controlHook != nil {
		s.controlHook()
	}
	return s.status, s.failControl
}
func (s *fakeNativeService) Delete() error {
	*s.events = append(*s.events, "service-delete")
	if s.failDelete != nil {
		return s.failDelete
	}
	s.deleted = true
	return nil
}
func (s *fakeNativeService) SetRecoveryActions(actions []mgr.RecoveryAction, reset uint32) error {
	*s.events = append(*s.events, "service-recovery")
	s.recoveryActions = append([]mgr.RecoveryAction(nil), actions...)
	s.recoveryReset = reset
	return s.failRecovery
}
func (s *fakeNativeService) SetRecoveryActionsExact(actions []mgr.RecoveryAction, reset uint32) error {
	return s.SetRecoveryActions(actions, reset)
}
func (s *fakeNativeService) RecoveryActions() ([]mgr.RecoveryAction, error) {
	return append([]mgr.RecoveryAction(nil), s.recoveryActions...), nil
}
func (s *fakeNativeService) ResetRecoveryActions() error {
	s.recoveryActions = nil
	s.recoveryReset = 0
	return nil
}
func (s *fakeNativeService) ResetPeriod() (uint32, error) { return s.recoveryReset, nil }
func (s *fakeNativeService) SetRecoveryActionsOnNonCrashFailures(value bool) error {
	*s.events = append(*s.events, "service-recovery-flag")
	s.recoverNonCrash = value
	return s.failRecoveryFlag
}
func (s *fakeNativeService) RecoveryActionsOnNonCrashFailures() (bool, error) {
	return s.recoverNonCrash, nil
}
func (s *fakeNativeService) Close() error { return nil }

func fakeNativeInstallDependencies(
	manager *fakeNativeSCM,
	legacy nativeLegacyState,
	events *[]string,
) nativeInstallDependencies {
	return nativeInstallDependencies{
		connectSCM:          func() (nativeSCM, error) { return manager, nil },
		lockExecutable:      func(string) (func(), error) { return func() {}, nil },
		lockPriorExecutable: func(string) (func(), error) { return func() {}, nil },
		provisionCredential: func() (nativeCredential, error) {
			return nativeCredential{path: `C:\ProgramData\VIIPER\viiper.key.txt`, password: "credential", created: true}, nil
		},
		rollbackCredential: func(nativeCredential) error { return nil },
		preflightDriver: func() error {
			*events = append(*events, "driver-preflight")
			return nil
		},
		snapshotLegacy: func(context.Context) (nativeLegacyState, error) { return legacy, nil },
		stopLegacy: func(context.Context, *nativeLegacyState, *slog.Logger) error {
			*events = append(*events, "legacy-stop")
			return nil
		},
		removeLegacy: func(context.Context, nativeLegacyState) error {
			*events = append(*events, "legacy-remove")
			return nil
		},
		restoreLegacy: func(context.Context, nativeLegacyState) error {
			*events = append(*events, "legacy-restore")
			return nil
		},
		restartLegacy: func(context.Context, nativeLegacyState) error {
			*events = append(*events, "legacy-restart")
			return nil
		},
		verifyBroker: func(context.Context, string) error {
			*events = append(*events, "verify")
			return nil
		},
		wait: immediateWait,
	}
}

func immediateWait(context.Context, time.Duration) error { return nil }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func stringPointer(value string) *string { return &value }

func nativeRunRegistrationPointer(value string, valueType uint32) *nativeRunRegistration {
	return &nativeRunRegistration{value: value, valueType: valueType}
}

func beforeEvent(events []string, first, second string) bool {
	firstIndex, secondIndex := -1, -1
	for index, event := range events {
		if event == first && firstIndex < 0 {
			firstIndex = index
		}
		if event == second && secondIndex < 0 {
			secondIndex = index
		}
	}
	return firstIndex >= 0 && secondIndex >= 0 && firstIndex < secondIndex
}

func lastIndex(events []string, value string) int {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index] == value {
			return index
		}
	}
	return -1
}
