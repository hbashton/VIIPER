//go:build windows

package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestNativePackageLocalTestTrustCapabilityBindsParentAndPackage(t *testing.T) {
	t.Parallel()
	request := nativePackageRequest{
		sourceRevision:                     strings.Repeat("a", 40),
		localTestCertificatePath:           `C:\package\ViiperUdeTest.cer`,
		expectedLocalTestCertificateSHA256: strings.Repeat("b", 64),
		expectedLocalTestPackageLockSHA256: strings.Repeat("c", 64),
	}
	journalDirectory := `C:\ProgramData\VIIPER-TrustManager`
	capability := nativePackageLocalTestTrustCapability{
		Schema:                 nativePackageLocalTestTrustCapabilitySchema,
		Nonce:                  strings.Repeat("01", 16),
		ParentPID:              1234,
		ParentCreationFileTime: 134000000000000000,
		SourceRevision:         request.sourceRevision,
		CertificatePath:        request.localTestCertificatePath,
		CertificateSHA256:      request.expectedLocalTestCertificateSHA256,
		PackageLockSHA256:      request.expectedLocalTestPackageLockSHA256,
		TrustJournalSchema:     nativePackageLocalTestTrustOwnershipSchema,
		TrustJournalDirectory:  journalDirectory,
	}
	if err := validateNativePackageLocalTestTrustCapability(
		capability, request, journalDirectory,
		capability.ParentPID, capability.ParentCreationFileTime,
	); err != nil {
		t.Fatalf("valid capability: %v", err)
	}
	mutations := map[string]func(*nativePackageLocalTestTrustCapability){
		"schema":           func(value *nativePackageLocalTestTrustCapability) { value.Schema = "v2" },
		"nonce":            func(value *nativePackageLocalTestTrustCapability) { value.Nonce = strings.Repeat("A", 32) },
		"parent PID":       func(value *nativePackageLocalTestTrustCapability) { value.ParentPID++ },
		"parent creation":  func(value *nativePackageLocalTestTrustCapability) { value.ParentCreationFileTime++ },
		"source":           func(value *nativePackageLocalTestTrustCapability) { value.SourceRevision = strings.Repeat("d", 40) },
		"certificate path": func(value *nativePackageLocalTestTrustCapability) { value.CertificatePath += ".other" },
		"certificate":      func(value *nativePackageLocalTestTrustCapability) { value.CertificateSHA256 = strings.Repeat("e", 64) },
		"package lock":     func(value *nativePackageLocalTestTrustCapability) { value.PackageLockSHA256 = strings.Repeat("f", 64) },
		"journal schema":   func(value *nativePackageLocalTestTrustCapability) { value.TrustJournalSchema = "v2" },
		"journal directory": func(value *nativePackageLocalTestTrustCapability) {
			value.TrustJournalDirectory += ".other"
		},
	}
	for name, mutate := range mutations {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			changed := capability
			mutate(&changed)
			if err := validateNativePackageLocalTestTrustCapability(
				changed, request, journalDirectory,
				capability.ParentPID, capability.ParentCreationFileTime,
			); err == nil {
				t.Fatal("mismatched capability accepted")
			}
		})
	}
}

func TestNativePackageLocalTestTrustOwnershipRequiresCanonicalBytes(t *testing.T) {
	t.Parallel()
	contents := []byte(`{"schema":"viiper.native.local-test-trust-ownership/v1","sourceRevision":"` +
		strings.Repeat("a", 40) + `","certificateSha256":"` + strings.Repeat("b", 64) +
		`","packageLockSha256":"` + strings.Repeat("c", 64) +
		`","baselineRoot":0,"baselineTrustedPublisher":1}` + "\n")
	value, err := decodeCanonicalNativePackageLocalTestTrustOwnership(contents)
	if err != nil {
		t.Fatalf("canonical ownership: %v", err)
	}
	if value.BaselineRoot != 0 || value.BaselineTrustedPublisher != 1 {
		t.Fatalf("ownership baselines=%d/%d", value.BaselineRoot, value.BaselineTrustedPublisher)
	}
	for name, changed := range map[string][]byte{
		"missing LF": contents[:len(contents)-1],
		"unknown field": []byte(strings.Replace(string(contents),
			`"baselineRoot":0`, `"unknown":0,"baselineRoot":0`, 1)),
		"duplicate field": []byte(strings.Replace(string(contents),
			`"baselineRoot":0`, `"baselineRoot":0,"baselineRoot":0`, 1)),
		"noncanonical whitespace": []byte(strings.Replace(string(contents), `,"sourceRevision"`, `, "sourceRevision"`, 1)),
	} {
		name, changed := name, changed
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeCanonicalNativePackageLocalTestTrustOwnership(changed); err == nil {
				t.Fatal("noncanonical ownership bytes accepted")
			}
		})
	}
}

func TestNativePackageLocalTestTrustDurableCutMatrix(t *testing.T) {
	t.Parallel()
	cutError := errors.New("simulated process cut")
	type model struct {
		journal   string
		root      int
		publisher int
		topology  bool
	}
	t.Run("protected publication", func(t *testing.T) {
		t.Parallel()
		for _, artifact := range []string{"lease", "preparing"} {
			artifact := artifact
			t.Run(artifact, func(t *testing.T) {
				t.Parallel()
				for _, cutName := range []string{
					"scratch-created", "scratch-written", "scratch-flushed",
					"scratch-verified", "before-publish", "after-publish",
				} {
					cutName := cutName
					t.Run(cutName, func(t *testing.T) {
						t.Parallel()
						type publication struct {
							scratch   string
							flushed   bool
							verified  bool
							canonical string
						}
						state := publication{}
						cut := func(name string) error {
							if name == cutName {
								return cutError
							}
							return nil
						}
						steps := []struct {
							name      string
							cutBefore bool
							op        func()
						}{
							{name: "scratch-created", op: func() { state.scratch = ".random.scratch" }},
							{name: "scratch-written", op: func() { state.scratch = "complete" }},
							{name: "scratch-flushed", op: func() { state.flushed = true }},
							{name: "scratch-verified", op: func() { state.verified = true }},
							{name: "before-publish", cutBefore: true, op: func() {
								state.canonical = state.scratch
								state.scratch = ""
							}},
							{name: "after-publish", op: func() {}},
						}
						var observed error
						for _, step := range steps {
							observed = executeNativePackageLocalTestTrustStep(
								step.name, step.cutBefore,
								func() error { step.op(); return nil }, cut,
							)
							if observed != nil {
								break
							}
						}
						if !errors.Is(observed, cutError) {
							t.Fatalf("publication cut %s was not reached: %v", cutName, observed)
						}
						if cutName == "after-publish" {
							if state.canonical != "complete" || state.scratch != "" ||
								!state.flushed || !state.verified {
								t.Fatalf("post-publication cut exposed incomplete authority: %+v", state)
							}
							return
						}
						if state.canonical != "" {
							t.Fatalf("pre-publication cut exposed canonical authority: %+v", state)
						}
						// A process cut may retain only a protected, random scratch name.
						// It is never interpreted as authority and a successor can retire it.
						state.scratch = ""
						if state.canonical != "" || state.scratch != "" {
							t.Fatalf("successor could not retire inert scratch: %+v", state)
						}
					})
				}
			})
		}
	})
	t.Run("install", func(t *testing.T) {
		t.Parallel()
		for _, cutName := range []string{
			"preparing-published",
			"pending-published",
			"root-added",
			"trusted-publisher-added",
			"topology-success-before-owned",
		} {
			cutName := cutName
			t.Run(cutName, func(t *testing.T) {
				t.Parallel()
				state := model{}
				cut := func(name string) error {
					if name == cutName {
						return cutError
					}
					return nil
				}
				steps := []struct {
					name      string
					cutBefore bool
					op        func()
				}{
					{name: "preparing-published", op: func() { state.journal = "preparing" }},
					{name: "pending-published", op: func() { state.journal = "pending" }},
					{name: "root-added", op: func() { state.root = 1 }},
					{name: "trusted-publisher-added", op: func() { state.publisher = 1 }},
					{name: "topology-success-before-owned", cutBefore: true, op: func() { state.journal = "owned" }},
				}
				var observed error
				for _, step := range steps {
					if step.name == "topology-success-before-owned" {
						state.topology = true
					}
					observed = executeNativePackageLocalTestTrustStep(
						step.name, step.cutBefore,
						func() error { step.op(); return nil }, cut,
					)
					if observed != nil {
						break
					}
				}
				if !errors.Is(observed, cutError) {
					t.Fatalf("cut %s was not reached: %v", cutName, observed)
				}
				expected := map[string]model{
					"preparing-published":           {journal: "preparing"},
					"pending-published":             {journal: "pending"},
					"root-added":                    {journal: "pending", root: 1},
					"trusted-publisher-added":       {journal: "pending", root: 1, publisher: 1},
					"topology-success-before-owned": {journal: "pending", root: 1, publisher: 1, topology: true},
				}[cutName]
				if state != expected {
					t.Fatalf("cut %s durable model=%+v want=%+v", cutName, state, expected)
				}
				if cutName == "topology-success-before-owned" {
					if !state.topology || state.journal != "pending" || state.root != 1 || state.publisher != 1 {
						t.Fatalf("success-before-owned cut lost pending authority: %+v", state)
					}
					// A successor repairs/revalidates the exact installed topology and
					// only then performs the same pre-cut Owned publication step.
					if err := executeNativePackageLocalTestTrustStep(
						"topology-success-before-owned", true,
						func() error { state.journal = "owned"; return nil }, nil,
					); err != nil {
						t.Fatal(err)
					}
					if state.journal != "owned" {
						t.Fatalf("successor did not settle owned: %+v", state)
					}
				}
			})
		}
	})

	t.Run("baseline restore", func(t *testing.T) {
		t.Parallel()
		for _, cutName := range []string{
			"root-restored", "trusted-publisher-restored", "cleared-published",
		} {
			cutName := cutName
			t.Run(cutName, func(t *testing.T) {
				t.Parallel()
				state := model{journal: "uninstalling", root: 1, publisher: 1}
				cut := func(name string) error {
					if name == cutName {
						return cutError
					}
					return nil
				}
				steps := []struct {
					name string
					op   func()
				}{
					{name: "root-restored", op: func() { state.root = 0 }},
					{name: "trusted-publisher-restored", op: func() { state.publisher = 0 }},
					{name: "cleared-published", op: func() { state.journal = "cleared" }},
				}
				var observed error
				for _, step := range steps {
					observed = executeNativePackageLocalTestTrustStep(
						step.name, false,
						func() error { step.op(); return nil }, cut,
					)
					if observed != nil {
						break
					}
				}
				if !errors.Is(observed, cutError) || state.journal != "uninstalling" && cutName != "cleared-published" {
					t.Fatalf("restore cut %s left unsafe model %+v error=%v", cutName, state, observed)
				}
				// All store operations are idempotent and an uninstalling retry
				// re-proves topology absence before completing the same sequence.
				state.root, state.publisher, state.journal = 0, 0, "cleared"
				if state.root != 0 || state.publisher != 0 || state.journal != "cleared" {
					t.Fatalf("restore retry did not settle: %+v", state)
				}
			})
		}
	})
}

func TestNativePackageLocalTestTrustFailureCleanupRejectsAmbiguousOrResumedAuthority(t *testing.T) {
	t.Parallel()
	baseState := nativePackageLocalTestTrustState{state: "pending", createdCurrent: true}
	baseProof := nativePackageInstallProof{
		success: false, changed: false, rebootRequired: false,
		rollback: "not-needed", exitCode: 4,
	}
	if !nativePackageLocalTestTrustMayRestoreAfterFailure(
		&baseState, baseProof, true, true, false, false,
	) {
		t.Fatal("exact fresh settled no-change failure was not eligible for topology-gated restore")
	}
	validExitOne := baseProof
	validExitOne.exitCode = 1
	if !nativePackageLocalTestTrustMayRestoreAfterFailure(
		&baseState, validExitOne, true, true, false, false,
	) {
		t.Fatal("exact fresh exit-1 no-change failure was not eligible for topology-gated restore")
	}
	cases := map[string]func(*nativePackageLocalTestTrustState, *nativePackageInstallProof) (bool, bool, bool, bool){
		"resumed pending": func(state *nativePackageLocalTestTrustState, _ *nativePackageInstallProof) (bool, bool, bool, bool) {
			state.resumed = true
			return true, true, false, false
		},
		"prior owned": func(state *nativePackageLocalTestTrustState, _ *nativePackageInstallProof) (bool, bool, bool, bool) {
			state.alreadyOwned = true
			return true, true, false, false
		},
		"not current": func(state *nativePackageLocalTestTrustState, _ *nativePackageInstallProof) (bool, bool, bool, bool) {
			state.createdCurrent = false
			return true, true, false, false
		},
		"wrong state": func(state *nativePackageLocalTestTrustState, _ *nativePackageInstallProof) (bool, bool, bool, bool) {
			state.state = "uninstalling"
			return true, true, false, false
		},
		"proof missing": func(_ *nativePackageLocalTestTrustState, _ *nativePackageInstallProof) (bool, bool, bool, bool) {
			return false, true, false, false
		},
		"helper unsettled": func(_ *nativePackageLocalTestTrustState, _ *nativePackageInstallProof) (bool, bool, bool, bool) {
			return true, false, false, false
		},
		"success": func(_ *nativePackageLocalTestTrustState, proof *nativePackageInstallProof) (bool, bool, bool, bool) {
			proof.success = true
			return true, true, false, false
		},
		"changed": func(_ *nativePackageLocalTestTrustState, proof *nativePackageInstallProof) (bool, bool, bool, bool) {
			proof.changed = true
			return true, true, false, false
		},
		"reboot": func(_ *nativePackageLocalTestTrustState, proof *nativePackageInstallProof) (bool, bool, bool, bool) {
			proof.rebootRequired = true
			return true, true, false, false
		},
		"rollback": func(_ *nativePackageLocalTestTrustState, proof *nativePackageInstallProof) (bool, bool, bool, bool) {
			proof.rollback = "succeeded"
			return true, true, false, false
		},
		"exit 3": func(_ *nativePackageLocalTestTrustState, proof *nativePackageInstallProof) (bool, bool, bool, bool) {
			proof.exitCode = 3
			return true, true, false, false
		},
		"broker settlement": func(_ *nativePackageLocalTestTrustState, _ *nativePackageInstallProof) (bool, bool, bool, bool) {
			return true, true, true, false
		},
		"broker handoff": func(_ *nativePackageLocalTestTrustState, _ *nativePackageInstallProof) (bool, bool, bool, bool) {
			return true, true, false, true
		},
	}
	for name, mutate := range cases {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := baseState
			proof := baseProof
			present, settled, brokerSettlement, handoff := mutate(&state, &proof)
			if nativePackageLocalTestTrustMayRestoreAfterFailure(
				&state, proof, present, settled, brokerSettlement, handoff,
			) {
				t.Fatal("ambiguous or successor-owned failure authorized trust restore")
			}
		})
	}
}

func TestNativePackageDriverCoordinationUsesDistinctInheritedEvents(t *testing.T) {
	t.Parallel()
	coordination, err := newNativePackageDriverCoordination()
	if err != nil {
		t.Fatalf("create driver coordination: %v", err)
	}
	defer coordination.close()
	seen := map[windows.Handle]bool{}
	for _, handle := range []windows.Handle{
		coordination.quiesceRequest, coordination.quiesceReady,
		coordination.quiesceAbort, coordination.brokerHandoff,
	} {
		if handle == 0 || seen[handle] {
			t.Fatalf("coordination event handle=%d is null or duplicated", handle)
		}
		seen[handle] = true
		status, err := windows.WaitForSingleObject(handle, 0)
		if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
			t.Fatalf("coordination event %d initial wait=(0x%x, %v), want timeout",
				handle, status, err)
		}
	}
	if len(coordination.inheritedHandles()) != 4 || len(coordination.arguments()) != 8 {
		t.Fatal("driver coordination did not publish exactly four inherited handle arguments")
	}
	if unsafe.Sizeof(windows.Handle(0)) != unsafe.Sizeof(uintptr(0)) {
		t.Fatal("Windows handle width no longer matches the decimal handoff contract")
	}
}

func TestNativePackageDriverCoordinationHoldsServiceMutexUntilBrokerHandoff(t *testing.T) {
	t.Parallel()
	coordination, err := newNativePackageDriverCoordination()
	if err != nil {
		t.Fatal(err)
	}
	defer coordination.close()
	process, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process) //nolint:errcheck

	released := make(chan struct{})
	transaction := &windowsNativePackageTransaction{
		serviceSnapshot: nativePackageServiceSnapshot{disposition: nativePackageServiceAbsent},
		releaseServiceMutex: func() {
			close(released)
		},
	}
	childDone := make(chan error, 1)
	go func() {
		if err := windows.SetEvent(coordination.quiesceRequest); err != nil {
			childDone <- err
			return
		}
		if status, err := windows.WaitForSingleObject(coordination.quiesceReady, 1000); err != nil || status != windows.WAIT_OBJECT_0 {
			childDone <- errors.Join(err, errors.New("quiescence readiness was not signaled"))
			return
		}
		select {
		case <-released:
			childDone <- errors.New("service mutex released before broker handoff")
			return
		default:
		}
		if err := windows.SetEvent(coordination.brokerHandoff); err != nil {
			childDone <- err
			return
		}
		select {
		case <-released:
		case <-time.After(time.Second):
			childDone <- errors.New("service mutex remained held after broker handoff")
			return
		}
		childDone <- windows.SetEvent(process)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transaction.coordinateDriverHelper(ctx, process, coordination); err != nil {
		t.Fatalf("coordinate driver helper: %v", err)
	}
	if err := <-childDone; err != nil {
		t.Fatal(err)
	}
	if !transaction.driverQuiesceRequested || !transaction.driverBrokerHandoff ||
		transaction.releaseServiceMutex != nil {
		t.Fatalf("coordination state request=%v handoff=%v release=%v",
			transaction.driverQuiesceRequested, transaction.driverBrokerHandoff,
			transaction.releaseServiceMutex != nil)
	}
}

func TestNativePackageDriverQuiescenceStopsTrustedRunningService(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{events: &events, status: svc.Status{State: svc.Running}}
	transaction := &windowsNativePackageTransaction{
		serviceSnapshot: nativePackageServiceSnapshot{
			disposition: nativePackageServiceTrusted,
			wasRunning:  true,
		},
		service:             service,
		releaseServiceMutex: func() {},
	}
	if err := transaction.quiescePriorServiceForDriver(context.Background()); err != nil {
		t.Fatalf("quiesce trusted service: %v", err)
	}
	if !transaction.stoppedTrustedService || service.status.State != svc.Stopped ||
		!slices.Equal(events, []string{"service-stop"}) {
		t.Fatalf("trusted service state stopped=%v status=%d events=%v",
			transaction.stoppedTrustedService, service.status.State, events)
	}

	if err := transaction.quiescePriorServiceForDriver(context.Background()); err != nil {
		t.Fatalf("repeat trusted quiescence: %v", err)
	}
}

func TestNativePackageDriverQuiescenceRemovesWeakExactOwnedService(t *testing.T) {
	t.Parallel()
	for _, running := range []bool{false, true} {
		running := running
		t.Run(map[bool]string{false: "stopped", true: "running"}[running], func(t *testing.T) {
			t.Parallel()
			events := []string{}
			state := svc.Stopped
			if running {
				state = svc.Running
			}
			service := &fakeNativeService{events: &events, status: svc.Status{State: state}}
			manager := newFakeNativeSCM(service, &events)
			transaction := &windowsNativePackageTransaction{
				serviceSnapshot: nativePackageServiceSnapshot{
					disposition: nativePackageServiceWeakExactOwned,
					wasRunning:  running,
				},
				service: service, manager: manager,
				releaseServiceMutex: func() {},
			}
			if err := transaction.quiescePriorServiceForDriver(context.Background()); err != nil {
				t.Fatalf("quiesce weak service: %v", err)
			}
			want := []string{"service-delete", "service-open"}
			if running {
				want = append([]string{"service-stop"}, want...)
			}
			if !transaction.weakServiceMutation || !transaction.weakServiceRemoved ||
				transaction.service != nil || !service.deleted || !slices.Equal(events, want) {
				t.Fatalf("weak service state mutation=%v removed=%v live=%v deleted=%v events=%v want=%v",
					transaction.weakServiceMutation, transaction.weakServiceRemoved,
					transaction.service != nil, service.deleted, events, want)
			}
			if err := transaction.quiescePriorServiceForDriver(context.Background()); err != nil {
				t.Fatalf("repeat weak quiescence: %v", err)
			}
			if !slices.Equal(events, want) {
				t.Fatalf("repeat weak quiescence mutated service again: events=%v want=%v", events, want)
			}
		})
	}
}

func TestNativePackageDriverQuiescenceDoesNotRestoreWeakServiceOnDeleteFailure(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{
		events: &events, status: svc.Status{State: svc.Running}, failDelete: errors.New("delete failed"),
	}
	weak := &windowsNativePackageTransaction{
		serviceSnapshot: nativePackageServiceSnapshot{
			disposition: nativePackageServiceWeakExactOwned,
			wasRunning:  true,
		},
		service: service, manager: newFakeNativeSCM(service, &events),
		releaseServiceMutex: func() {},
	}
	if err := weak.quiescePriorServiceForDriver(context.Background()); err == nil {
		t.Fatal("weak service delete failure was accepted")
	}
	if !weak.weakServiceMutation || weak.weakServiceRemoved || service.status.State != svc.Stopped ||
		service.deleted || !slices.Equal(events, []string{"service-stop", "service-delete"}) {
		t.Fatalf("weak failure state mutation=%v removed=%v status=%d deleted=%v events=%v",
			weak.weakServiceMutation, weak.weakServiceRemoved, service.status.State,
			service.deleted, events)
	}
	if err := weak.Rollback(context.Background()); err != nil {
		t.Fatalf("fail-closed weak rollback: %v", err)
	}
	if service.startCalls != 0 || service.status.State != svc.Stopped {
		t.Fatalf("untrusted weak service was restarted: starts=%d status=%d",
			service.startCalls, service.status.State)
	}
}

func TestNativePackageOuterRollbackLeavesServiceStoppedOnUnsettledDriverProof(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{events: &events, status: svc.Status{State: svc.Stopped}}
	transaction := &windowsNativePackageTransaction{
		serviceSnapshot: nativePackageServiceSnapshot{
			disposition: nativePackageServiceTrusted,
			wasRunning:  true,
		},
		service:               service,
		stoppedTrustedService: true,
		driverHelperSettled:   false,
	}
	err := transaction.Rollback(context.Background())
	if err == nil || service.startCalls != 0 || len(events) != 0 {
		t.Fatalf("unsettled outer rollback error=%v startCalls=%d events=%v",
			err, service.startCalls, events)
	}
}

func TestNativePackageCoordinationTokenAllowsNestedImmutableRead(t *testing.T) {
	requireNativeMutexAdministrator(t)
	transaction := &windowsNativePackageTransaction{parent: t.TempDir()}
	if err := transaction.stageCoordinationToken(); err != nil {
		t.Fatalf("stage coordination token: %v", err)
	}
	t.Cleanup(func() {
		if err := transaction.releaseCoordinationToken(); err != nil {
			t.Errorf("release coordination token: %v", err)
		}
	})

	// This is the exact access/share combination used by the nested broker.
	// It failed live while the outer transaction retained a write-capable
	// handle, even though both opens requested FILE_SHARE_READ.
	nested, err := lockNativePackageInput(transaction.tokenPath)
	if err != nil {
		t.Fatalf("nested immutable token open: %v", err)
	}
	defer windows.CloseHandle(nested) //nolint:errcheck
	hash, err := hashNativePackageHandle(nested)
	if err != nil {
		t.Fatalf("hash nested token handle: %v", err)
	}
	if hash != transaction.tokenSHA256 {
		t.Fatalf("nested token hash = %s, want %s", hash, transaction.tokenSHA256)
	}
}

func TestNativePackageRuntimePayloadExcludesCertificationPDB(t *testing.T) {
	want := []string{"ViiperUde.inf", "ViiperUde.sys", "ViiperUde.cat"}
	if !slices.Equal(nativePackageDriverFiles, want) {
		t.Fatalf("runtime driver payload = %v, want %v", nativePackageDriverFiles, want)
	}
	if slices.Contains(nativePackageDriverFiles, "ViiperUde.pdb") {
		t.Fatal("certification PDB became a runtime installation dependency")
	}
}

func TestNativePackageServiceTrustRequiresExactOwnedState(t *testing.T) {
	t.Parallel()
	expected := mgr.Config{
		ServiceType: 0x10, StartType: mgr.StartAutomatic, ErrorControl: mgr.ErrorNormal,
		BinaryPathName:   `"C:\Program Files\VIIPER\viiper.exe" service --transport native-ude`,
		ServiceStartName: nativeServiceAccount, DisplayName: nativeBrokerDisplayName,
		Description: nativeBrokerDescription, SidType: 1,
	}
	actions := append([]mgr.RecoveryAction(nil), nativeServiceRecoveryActions...)
	canonical := func(actual mgr.Config, dacl string, recovery []mgr.RecoveryAction,
		reset uint32, nonCrash bool) bool {
		return isCanonicalNativePackageService(
			actual, expected, dacl, recovery, reset, nonCrash,
		)
	}
	if !canonical(expected, nativeBrokerServiceSDDL, actions,
		nativeServiceRecoveryResetSecond, true) {
		t.Fatal("exact protected service was not trusted")
	}

	staleConfig := expected
	staleConfig.StartType = mgr.StartManual
	staleRecovery := append([]mgr.RecoveryAction(nil), actions...)
	staleRecovery[0].Delay = time.Millisecond
	cases := map[string]bool{
		"stale config":   canonical(staleConfig, nativeBrokerServiceSDDL, actions, nativeServiceRecoveryResetSecond, true),
		"weak DACL":      canonical(expected, "D:(A;;GA;;;WD)", actions, nativeServiceRecoveryResetSecond, true),
		"stale recovery": canonical(expected, nativeBrokerServiceSDDL, staleRecovery, nativeServiceRecoveryResetSecond, true),
		"stale reset":    canonical(expected, nativeBrokerServiceSDDL, actions, 1, true),
		"stale mode":     canonical(expected, nativeBrokerServiceSDDL, actions, nativeServiceRecoveryResetSecond, false),
	}
	for name, trusted := range cases {
		if trusted {
			t.Errorf("%s was trusted instead of delete/recreate", name)
		}
	}
}

func TestNativePackageRollbackReconcilesStoppedPriorService(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{events: &events, status: svc.Status{State: svc.Stopped}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reconcileNativePackageServiceRunning(ctx, service); err != nil {
		t.Fatalf("reconcile prior service: %v", err)
	}
	if service.startCalls != 1 || service.status.State != svc.Running {
		t.Fatalf("startCalls=%d state=%d events=%v", service.startCalls, service.status.State, events)
	}
}

func TestNativePackageRollbackPreservesImagesAndStoppedServiceWhenSCMRollbackIsUnsettled(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{events: &events, status: svc.Status{State: svc.Stopped}}
	transaction := &windowsNativePackageTransaction{
		nestedBrokerCommit:           true,
		nestedMutationStarted:        true,
		nestedServiceRollbackSettled: false,
		destinationPublished:         true,
		backupPath:                   `C:\Program Files\VIIPER\.prior.rollback.exe`,
		stoppedTrustedService:        true,
		serviceSnapshot:              nativePackageServiceSnapshot{wasRunning: true},
		service:                      service,
	}

	err := transaction.Rollback(context.Background())
	if err == nil {
		t.Fatal("unsettled nested SCM rollback was reported as restored")
	}
	if service.startCalls != 0 || len(events) != 0 {
		t.Fatalf("indeterminate service was restarted: startCalls=%d events=%v",
			service.startCalls, events)
	}
	if !transaction.destinationPublished {
		t.Fatal("staged broker image was removed after unsettled SCM rollback")
	}
	if transaction.backupPath == "" {
		t.Fatal("prior broker backup was consumed after unsettled SCM rollback")
	}
	if transaction.nestedRollbackSucceeded {
		t.Fatal("unsettled SCM rollback was exposed as a safe nested rollback")
	}
}
