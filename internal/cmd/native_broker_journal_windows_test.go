//go:build windows

package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

var errNativeBrokerJournalCutpoint = errors.New("simulated process loss")

func newNativeBrokerJournalModel(t *testing.T) (*nativeBrokerJournal, *[][]byte) {
	t.Helper()
	snapshot := nativeBrokerJournalSnapshot{
		Schema: nativeBrokerJournalSchema, TransactionID: strings.Repeat("a", 32),
		OuterTransactionID:      strings.Repeat("b", 64),
		OuterTokenPath:          `C:\Program Files\VIIPER\.viiper.transaction.test.token`,
		TargetUserSID:           "S-1-5-21-1-2-3-1001",
		CandidatePath:           `C:\Program Files\VIIPER\viiper.exe`,
		CandidateSHA256:         strings.Repeat("c", 64),
		PriorCredentialArtifact: strings.Repeat("d", 64),
		PriorLegacyArtifact:     strings.Repeat("e", 64),
	}
	payload, err := nativeBrokerJournalCanonicalJSON(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var persisted [][]byte
	j := &nativeBrokerJournal{
		snapshot: snapshot, snapshotDigest: nativeBrokerJournalHash(payload),
		appendRecord: func(record []byte) error {
			persisted = append(persisted, append([]byte(nil), record...))
			return nil
		},
	}
	return j, &persisted
}

func TestNativeBrokerJournalCutpointsLeaveCanonicalPrefix(t *testing.T) {
	t.Parallel()
	phases := append([]nativeBrokerJournalPhase(nil),
		nativeBrokerForwardPhaseOrder[:len(nativeBrokerForwardPhaseOrder)-1]...,
	)
	phases = append(phases,
		nativeBrokerPhaseRollbackIntent,
		nativeBrokerPhaseRollbackService,
		nativeBrokerPhaseRollbackCredential,
		nativeBrokerPhaseRollbackImage,
		nativeBrokerPhaseRollbackLegacy,
		nativeBrokerPhaseRollbackSettled,
	)
	for cutIndex := range phases {
		cutIndex := cutIndex
		t.Run(string(phases[cutIndex]), func(t *testing.T) {
			t.Parallel()
			journal, persisted := newNativeBrokerJournalModel(t)
			journal.cutpoint = func(name string) error {
				if name == "after-record-"+string(phases[cutIndex]) {
					return errNativeBrokerJournalCutpoint
				}
				return nil
			}
			for index, phase := range phases {
				detail := ""
				if phase == nativeBrokerPhaseOuterSettlementPending ||
					phase == nativeBrokerPhaseOuterSettled {
					detail = strings.Repeat("f", 64)
				}
				err := journal.appendPhase(phase, detail)
				if index < cutIndex && err != nil {
					t.Fatalf("phase %s failed before cutpoint: %v", phase, err)
				}
				if index == cutIndex {
					if !errors.Is(err, errNativeBrokerJournalCutpoint) {
						t.Fatalf("phase %s error=%v", phase, err)
					}
					break
				}
			}
			if len(*persisted) != cutIndex+1 || len(journal.records) != cutIndex+1 {
				t.Fatalf("persisted=%d records=%d want=%d",
					len(*persisted), len(journal.records), cutIndex+1)
			}
			reloaded := &nativeBrokerJournal{
				snapshot: journal.snapshot, snapshotDigest: journal.snapshotDigest,
			}
			for _, line := range *persisted {
				var record nativeBrokerJournalRecord
				if err := decodeCanonicalNativeBrokerJSON(
					line, &record, nativeBrokerJournalMaximumLine,
				); err != nil {
					t.Fatalf("decode persisted prefix: %v", err)
				}
				if err := reloaded.validateLoadedRecord(record); err != nil {
					t.Fatalf("validate persisted prefix: %v", err)
				}
				reloaded.records = append(reloaded.records, record)
			}
			if reloaded.lastPhase() != phases[cutIndex] {
				t.Fatalf("last phase=%s want=%s", reloaded.lastPhase(), phases[cutIndex])
			}
		})
	}
}

func TestNativeBrokerJournalRejectsTamperAndIllegalDirection(t *testing.T) {
	t.Parallel()
	journal, persisted := newNativeBrokerJournalModel(t)
	if err := journal.appendPhase(nativeBrokerPhasePrepared, ""); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendPhase(nativeBrokerPhaseImageSwitchIntent, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendPhase(nativeBrokerPhasePrepared, ""); err == nil {
		t.Fatal("forward journal accepted a backward transition")
	}
	tampered := append([]byte(nil), (*persisted)[1]...)
	index := bytes.Index(tampered, []byte(strings.Repeat("f", 64)))
	if index < 0 {
		t.Fatal("detail digest not found in canonical record")
	}
	tampered[index] = '0'
	var record nativeBrokerJournalRecord
	if err := decodeCanonicalNativeBrokerJSON(tampered, &record, nativeBrokerJournalMaximumLine); err != nil {
		t.Fatalf("tamper should remain canonical JSON: %v", err)
	}
	reloaded := &nativeBrokerJournal{
		snapshot: journal.snapshot, snapshotDigest: journal.snapshotDigest,
		records: []nativeBrokerJournalRecord{journal.records[0]},
	}
	if err := reloaded.validateLoadedRecord(record); err == nil {
		t.Fatal("hash-chain validation accepted tampered detail digest")
	}
}

func TestNativeBrokerJournalPreparationCutsNeverExposeIncompleteActiveState(t *testing.T) {
	t.Parallel()
	cutpoints := []string{
		"prepare-directory-created",
		"prepare-credential-written",
		"prepare-legacy-written",
		"prepare-prior-image-written",
		"prepare-snapshot-written",
		"prepare-record-stream-created",
		"prepare-prepared-written",
		"prepare-active-published",
	}
	for cutIndex, cutpoint := range cutpoints {
		cutIndex, cutpoint := cutIndex, cutpoint
		t.Run(cutpoint, func(t *testing.T) {
			t.Parallel()
			preparing, active := false, false
			var completed []string
			step := func(name string) func() error {
				return func() error {
					if name == "directory-created" {
						preparing = true
					} else if !preparing || active {
						return errors.New("preparation escaped its unpublished directory")
					}
					completed = append(completed, name)
					return nil
				}
			}
			err := executeNativeBrokerJournalPreparation(nativeBrokerJournalPreparationOperations{
				createDirectory:    step("directory-created"),
				writeCredential:    step("credential-written"),
				writeLegacy:        step("legacy-written"),
				writePriorImage:    step("prior-image-written"),
				writeSnapshot:      step("snapshot-written"),
				createRecordStream: step("record-stream-created"),
				writePrepared:      step("prepared-written"),
				publishActive: func() error {
					if !preparing || len(completed) != len(cutpoints)-1 {
						return errors.New("active publication preceded complete preparation")
					}
					preparing, active = false, true
					completed = append(completed, "active-published")
					return nil
				},
				cutpoint: func(name string) error {
					if name == cutpoint {
						return errNativeBrokerJournalCutpoint
					}
					return nil
				},
			})
			if !errors.Is(err, errNativeBrokerJournalCutpoint) || len(completed) != cutIndex+1 {
				t.Fatalf("cut=%s completed=%v err=%v", cutpoint, completed, err)
			}
			if cutpoint == "prepare-active-published" {
				if !active || preparing {
					t.Fatal("post-publication cut lost authoritative active state")
				}
			} else if active || !preparing {
				t.Fatal("pre-publication cut exposed active state or lost disposable preparation")
			}
		})
	}
}

func TestNativeBrokerJournalAtomicRecordCutsKeepPublishedPrefix(t *testing.T) {
	t.Parallel()
	journal, persisted := newNativeBrokerJournalModel(t)
	if err := journal.appendPhase(nativeBrokerPhasePrepared, ""); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendPhase(nativeBrokerPhaseServiceStopIntent, ""); err != nil {
		t.Fatal(err)
	}
	current := append(append([]byte(nil), (*persisted)[0]...), '\n')
	next, err := buildNativeBrokerJournalRecordStream(current, (*persisted)[1])
	if err != nil {
		t.Fatal(err)
	}
	cutpoints := []string{
		nativeBrokerCutRecordPartialWrite,
		nativeBrokerCutRecordWriteDone,
		nativeBrokerCutRecordSyncDone,
		nativeBrokerCutRecordReadbackDone,
		nativeBrokerCutRecordBeforePublish,
	}
	for _, cutpoint := range cutpoints {
		cutpoint := cutpoint
		t.Run(cutpoint, func(t *testing.T) {
			t.Parallel()
			published := append([]byte(nil), current...)
			var staged []byte
			err := executeNativeBrokerJournalRecordPublication(
				(*persisted)[1],
				nativeBrokerJournalRecordPublicationOperations{
					loadCurrent: func() ([]byte, error) {
						return append([]byte(nil), published...), nil
					},
					discardStaging: func() error { staged = nil; return nil },
					stage: func(candidate []byte) error {
						staged = append([]byte(nil), candidate...)
						if cutpoint == nativeBrokerCutRecordPartialWrite {
							staged = staged[:len(staged)/2]
						}
						if cutpoint != nativeBrokerCutRecordBeforePublish {
							return errNativeBrokerJournalCutpoint
						}
						return nil
					},
					beforePublish: func() error {
						if cutpoint == nativeBrokerCutRecordBeforePublish {
							return errNativeBrokerJournalCutpoint
						}
						return nil
					},
					publish: func() error {
						published = append([]byte(nil), staged...)
						return nil
					},
				},
			)
			if !errors.Is(err, errNativeBrokerJournalCutpoint) || len(staged) == 0 {
				t.Fatalf("cutpoint %s was not observed before publication: err=%v", cutpoint, err)
			}
			if !bytes.Equal(published, current) {
				t.Fatalf("cutpoint %s changed the authoritative published prefix", cutpoint)
			}
		})
	}
	published := append([]byte(nil), current...)
	var staged []byte
	if err := executeNativeBrokerJournalRecordPublication(
		(*persisted)[1],
		nativeBrokerJournalRecordPublicationOperations{
			loadCurrent:    func() ([]byte, error) { return append([]byte(nil), published...), nil },
			discardStaging: func() error { staged = nil; return nil },
			stage:          func(candidate []byte) error { staged = append([]byte(nil), candidate...); return nil },
			beforePublish:  func() error { return nil },
			publish:        func() error { published = append([]byte(nil), staged...); return nil },
		},
	); err != nil || !bytes.Equal(published, next) {
		t.Fatalf("fully read-back record stream was not atomically published: err=%v", err)
	}
	if _, err := buildNativeBrokerJournalRecordStream(
		current[:len(current)-1], (*persisted)[1],
	); err == nil {
		t.Fatal("a torn published trailing record was accepted as an append base")
	}
}

func TestNativeBrokerSettlementRequestPublicationRecoversEveryStagingCut(t *testing.T) {
	t.Parallel()
	expected := []byte(`{"schema":1,"request":"exact"}`)
	cutpoints := []string{
		nativeBrokerCutSettlementPartialWrite,
		nativeBrokerCutSettlementWriteDone,
		nativeBrokerCutSettlementSyncDone,
		nativeBrokerCutSettlementReadbackDone,
		nativeBrokerCutSettlementBeforePublish,
	}
	for _, cutpoint := range cutpoints {
		cutpoint := cutpoint
		t.Run(cutpoint, func(t *testing.T) {
			t.Parallel()
			var published, staged []byte
			publishedExists, stagingExists := false, false
			operations := func(cut string) nativeBrokerSettlementPublicationOperations {
				return nativeBrokerSettlementPublicationOperations{
					loadPublished: func() ([]byte, bool, error) {
						return append([]byte(nil), published...), publishedExists, nil
					},
					loadStaging: func() ([]byte, bool, error) {
						return append([]byte(nil), staged...), stagingExists, nil
					},
					discardStaging: func() error {
						staged, stagingExists = nil, false
						return nil
					},
					publishStaging: func() error {
						published = append([]byte(nil), staged...)
						publishedExists, stagingExists = true, false
						return nil
					},
					writeNew: func() error {
						stagingExists = true
						staged = append([]byte(nil), expected...)
						if cut == nativeBrokerCutSettlementPartialWrite {
							staged = staged[:len(staged)/2]
						}
						if cut != "" {
							return errNativeBrokerJournalCutpoint
						}
						published = append([]byte(nil), staged...)
						publishedExists, stagingExists = true, false
						return nil
					},
					readback: func() ([]byte, error) {
						if !publishedExists {
							return nil, errors.New("no published request")
						}
						return append([]byte(nil), published...), nil
					},
				}
			}
			err := executeNativeBrokerSettlementPublication(expected, operations(cutpoint))
			if !errors.Is(err, errNativeBrokerJournalCutpoint) || publishedExists || !stagingExists {
				t.Fatalf("cut=%s published=%v staging=%v err=%v",
					cutpoint, publishedExists, stagingExists, err)
			}
			if err := executeNativeBrokerSettlementPublication(expected, operations("")); err != nil {
				t.Fatalf("resume after %s: %v", cutpoint, err)
			}
			if !publishedExists || stagingExists || !bytes.Equal(published, expected) {
				t.Fatalf("resume after %s did not publish the exact request", cutpoint)
			}
		})
	}
	tampered := []byte(`{"schema":1,"request":"different"}`)
	err := executeNativeBrokerSettlementPublication(expected,
		nativeBrokerSettlementPublicationOperations{
			loadPublished:  func() ([]byte, bool, error) { return tampered, true, nil },
			loadStaging:    func() ([]byte, bool, error) { return nil, false, nil },
			discardStaging: func() error { return nil },
			publishStaging: func() error { return nil },
			writeNew:       func() error { return nil },
			readback:       func() ([]byte, error) { return tampered, nil },
		})
	var manual *nativeBrokerJournalManualError
	if !errors.As(err, &manual) {
		t.Fatalf("published interior corruption was not latched unsafe: %v", err)
	}
}

func TestNativeBrokerTwoPhaseSettlementCutsAreReplayable(t *testing.T) {
	t.Parallel()
	cutpoints := []string{
		nativeBrokerCutAfterBindingOutput,
		nativeBrokerCutBeforePending,
		nativeBrokerCutAfterPending,
		nativeBrokerCutAfterRequest,
		nativeBrokerCutBeforeDriverAck,
		nativeBrokerCutAfterDriverAck,
		nativeBrokerCutBeforeBrokerFinal,
		nativeBrokerCutAfterBrokerFinal,
		nativeBrokerCutBeforeRetirement,
		nativeBrokerCutAfterRetirement,
		nativeBrokerCutAfterDiscard,
	}
	for _, cutpoint := range cutpoints {
		cutpoint := cutpoint
		t.Run(cutpoint, func(t *testing.T) {
			t.Parallel()
			driverPending := true
			brokerPending, requestPublished := false, false
			driverSettled, brokerSettled := false, false
			brokerRetired, discardAttempted := false, false
			operations := func(cut string) nativeBrokerOuterSettlementOperations {
				return nativeBrokerOuterSettlementOperations{
					recordPending: func() error {
						if !driverPending {
							return errors.New("broker pending preceded driver pending")
						}
						brokerPending = true
						return nil
					},
					publishRequest: func() error {
						if !brokerPending {
							return errors.New("request preceded broker pending")
						}
						requestPublished = true
						return nil
					},
					acknowledgeDriver: func() error {
						if !requestPublished {
							return errors.New("driver acknowledgement preceded request")
						}
						driverSettled = true
						return nil
					},
					recordBrokerSettled: func() error {
						if !driverSettled {
							return errors.New("broker final preceded driver final")
						}
						brokerSettled = true
						return nil
					},
					retireBrokerJournal: func() error {
						if !discardAttempted {
							return errors.New("broker retirement preceded authenticated driver discard")
						}
						brokerRetired = true
						return nil
					},
					discardInertState: func() error {
						if !brokerSettled {
							return errors.New("driver discard preceded the protected broker-final receipt")
						}
						discardAttempted = true
						return nil
					},
					cutpoint: func(name string) error {
						if name == cut {
							return errNativeBrokerJournalCutpoint
						}
						return nil
					},
				}
			}
			err := executeNativeBrokerOuterSettlement(operations(cutpoint))
			if !errors.Is(err, errNativeBrokerJournalCutpoint) {
				t.Fatalf("cutpoint %s was not reached: %v", cutpoint, err)
			}
			if driverSettled && !requestPublished || brokerSettled && !driverSettled ||
				discardAttempted && !brokerSettled || brokerRetired && !discardAttempted {
				t.Fatalf("cutpoint %s violated settlement ordering", cutpoint)
			}
			if !brokerRetired {
				if err := executeNativeBrokerOuterSettlement(operations("")); err != nil {
					t.Fatalf("idempotent replay after %s: %v", cutpoint, err)
				}
				if !driverSettled || !brokerSettled || !brokerRetired || !discardAttempted {
					t.Fatalf("replay after %s did not reach complete settlement", cutpoint)
				}
			}
		})
	}
	observed := false
	err := executeNativeBrokerOuterSettlement(nativeBrokerOuterSettlementOperations{
		recordPending: func() error { return nil }, publishRequest: func() error { return nil },
		acknowledgeDriver: func() error { return nil }, recordBrokerSettled: func() error { return nil },
		retireBrokerJournal: func() error { return nil },
		discardInertState:   func() error { return errors.New("inert cleanup retained") },
		observeDiscardError: func(error) { observed = true },
	})
	if err == nil || !observed {
		t.Fatalf("unverified driver discard did not retain broker evidence: observed=%v err=%v",
			observed, err)
	}
}

func TestNativeBrokerSettlementEnvelopeBindsBothJournalChains(t *testing.T) {
	t.Parallel()
	journal, _ := newNativeBrokerJournalModel(t)
	if err := journal.appendPhase(nativeBrokerPhasePrepared, ""); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendPhase(nativeBrokerPhaseNestedReady, ""); err != nil {
		t.Fatal(err)
	}
	proof := nativePackageInstallProof{
		success: true, changed: true, exitCode: 0, journalRecovery: "fresh",
		journal: journal.proof(), driverTransactionID: strings.Repeat("1", 64),
		driverPendingDigest: strings.Repeat("2", 64), settlementNonce: strings.Repeat("3", 64),
	}
	binding, bindingDigest, err := validateNativeBrokerOuterSettlementBinding(journal, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.appendPhase(nativeBrokerPhaseOuterSettlementPending, bindingDigest); err != nil {
		t.Fatal(err)
	}
	request := nativeBrokerOuterSettlementRequest{
		Schema: nativeBrokerJournalSchema, BindingSHA256: bindingDigest,
		BrokerPendingDigest: journal.proof().Digest, Binding: binding,
	}
	if err := validateNativeBrokerOuterSettlementRequest(journal, request); err != nil {
		t.Fatal(err)
	}
	contents, requestDigest, err := encodeNativeBrokerOuterSettlementEnvelope(request)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := decodeNativeBrokerOuterSettlementEnvelope(contents)
	if err != nil || envelope.Payload != request {
		t.Fatalf("canonical settlement envelope did not round trip: envelope=%+v err=%v", envelope, err)
	}
	prepared := nativeBrokerOuterSettlementPrepared{
		Request: request, RequestPath: `C:\ProgramData\VIIPER\BrokerTransactions\active-v1\outer-settlement.json`,
		RequestSHA256: requestDigest, contents: contents,
	}
	receipt := nativePackageBrokerSettlementReceipt{
		BrokerTransactionID: binding.BrokerTransactionID,
		BrokerPendingDigest: request.BrokerPendingDigest,
		DriverTransactionID: binding.DriverTransactionID,
		DriverPendingDigest: binding.DriverPendingDigest,
		SettlementNonce:     binding.SettlementNonce,
		RequestSHA256:       requestDigest, State: string(nativeBrokerPhaseOuterSettled),
		Digest: strings.Repeat("4", 64),
	}
	if err := validateNativeBrokerOuterSettlementReceipt(prepared, receipt); err != nil {
		t.Fatal(err)
	}
	receiptBytes, err := nativeBrokerJournalCanonicalJSON(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.appendPhase(
		nativeBrokerPhaseOuterSettled, nativeBrokerJournalHash(receiptBytes),
	); err != nil {
		t.Fatal(err)
	}
	if err := validateNativeBrokerOuterSettlementRequest(journal, request); err != nil {
		t.Fatalf("terminal broker journal lost its exact pending request: %v", err)
	}
	finalReceipt := nativeBrokerOuterSettlementFinal{
		Schema:              nativeBrokerJournalSchema,
		BrokerTransactionID: binding.BrokerTransactionID,
		BrokerPendingDigest: request.BrokerPendingDigest,
		BrokerSettledDigest: journal.proof().Digest,
		DriverTransactionID: binding.DriverTransactionID,
		DriverPendingDigest: binding.DriverPendingDigest,
		DriverSettledDigest: receipt.Digest,
		SettlementNonce:     binding.SettlementNonce,
		RequestSHA256:       requestDigest,
		State:               string(nativeBrokerPhaseOuterSettled),
	}
	if err := validateNativeBrokerOuterSettlementFinal(
		journal, prepared, receipt, finalReceipt,
	); err != nil {
		t.Fatal(err)
	}
	if replayedReceipt := nativeBrokerDriverReceiptFromFinal(finalReceipt); replayedReceipt != receipt {
		t.Fatalf("protected final receipt did not reconstruct the exact driver acknowledgement: got=%+v want=%+v",
			replayedReceipt, receipt)
	}
	finalContents, finalDigest, err := encodeNativeBrokerOuterSettlementFinalEnvelope(finalReceipt)
	if err != nil || !isCanonicalNativeBrokerJournalSHA256(finalDigest) {
		t.Fatalf("encode protected final receipt: digest=%q err=%v", finalDigest, err)
	}
	finalEnvelope, err := decodeNativeBrokerOuterSettlementFinalEnvelope(finalContents)
	if err != nil || finalEnvelope.Payload != finalReceipt {
		t.Fatalf("canonical final receipt did not round trip: envelope=%+v err=%v", finalEnvelope, err)
	}
	mixedFinal := finalReceipt
	mixedFinal.BrokerSettledDigest = strings.Repeat("5", 64)
	if err := validateNativeBrokerOuterSettlementFinal(
		journal, prepared, receipt, mixedFinal,
	); err == nil {
		t.Fatal("unrelated canonical broker-final digest authorized driver retirement")
	}
	mutations := []func(*nativePackageBrokerSettlementReceipt){
		func(value *nativePackageBrokerSettlementReceipt) { value.BrokerTransactionID = strings.Repeat("5", 32) },
		func(value *nativePackageBrokerSettlementReceipt) { value.BrokerPendingDigest = strings.Repeat("5", 64) },
		func(value *nativePackageBrokerSettlementReceipt) { value.DriverTransactionID = strings.Repeat("5", 64) },
		func(value *nativePackageBrokerSettlementReceipt) { value.DriverPendingDigest = strings.Repeat("5", 64) },
		func(value *nativePackageBrokerSettlementReceipt) { value.RequestSHA256 = strings.Repeat("5", 64) },
	}
	for index, mutate := range mutations {
		changed := receipt
		mutate(&changed)
		if err := validateNativeBrokerOuterSettlementReceipt(prepared, changed); err == nil {
			t.Fatalf("settlement receipt mix-up mutation %d was accepted", index)
		}
	}
	changedNonce := receipt
	changedNonce.SettlementNonce = strings.Repeat("5", 64)
	if err := validateNativeBrokerOuterSettlementReceipt(prepared, changedNonce); err == nil {
		t.Fatal("settlement nonce mix-up was accepted")
	}
	changedBinding := request
	changedBinding.Binding.DriverPendingDigest = strings.Repeat("5", 64)
	if err := validateNativeBrokerOuterSettlementRequest(journal, changedBinding); err == nil {
		t.Fatal("request accepted a changed driver chain with the same nonce")
	}
}

func TestNativeBrokerJournalRetirementCutsPreserveAdmissionAuthority(t *testing.T) {
	t.Parallel()
	cutpoints := []string{
		"retire-before-rename",
		"retire-after-rename",
		"retire-active-absence-proven",
		"retire-tombstone-proven",
	}
	for _, cutpoint := range cutpoints {
		cutpoint := cutpoint
		t.Run(cutpoint, func(t *testing.T) {
			t.Parallel()
			active, tombstone := true, false
			err := executeNativeBrokerJournalRetirement(nativeBrokerJournalRetirementOperations{
				rename: func() error {
					if !active || tombstone {
						return errors.New("invalid model rename")
					}
					active, tombstone = false, true
					return nil
				},
				proveActiveAbsent: func() error {
					if active {
						return errors.New("active still present")
					}
					return nil
				},
				proveTombstone: func() error {
					if !tombstone {
						return errors.New("tombstone absent")
					}
					return nil
				},
				discardTombstone: func() error {
					tombstone = false
					return nil
				},
				cutpoint: func(name string) error {
					if name == cutpoint {
						return errNativeBrokerJournalCutpoint
					}
					return nil
				},
			})
			if !errors.Is(err, errNativeBrokerJournalCutpoint) {
				t.Fatalf("cutpoint error=%v", err)
			}
			if cutpoint == "retire-before-rename" {
				if !active || tombstone {
					t.Fatal("pre-rename cut lost the still-authoritative terminal active journal")
				}
			} else if active || !tombstone {
				t.Fatal("post-rename cut republished active admission or lost its protected tombstone")
			}
		})
	}
	active, tombstone := true, false
	if err := executeNativeBrokerJournalRetirement(nativeBrokerJournalRetirementOperations{
		rename: func() error { active, tombstone = false, true; return nil },
		proveActiveAbsent: func() error {
			if active {
				return errors.New("active still present")
			}
			return nil
		},
		proveTombstone: func() error {
			if !tombstone {
				return errors.New("tombstone absent")
			}
			return nil
		},
		discardTombstone: func() error { return errors.New("simulated cleanup failure") },
	}); err != nil || active || !tombstone {
		t.Fatalf("non-authoritative tombstone cleanup blocked settlement: active=%v tombstone=%v err=%v",
			active, tombstone, err)
	}
}

func TestNativeBrokerJournalNestedReadyProofReplaysAfterParentCut(t *testing.T) {
	t.Parallel()
	journal, _ := newNativeBrokerJournalModel(t)
	if err := journal.appendPhase(nativeBrokerPhasePrepared, ""); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendPhase(nativeBrokerPhaseNestedReady, ""); err != nil {
		t.Fatal(err)
	}
	beforeParentRecord := journal.proof()
	if err := validateNativeBrokerNestedReplayBinding(
		journal, journal.snapshot.TargetUserSID, journal.snapshot.OuterTokenPath,
		journal.snapshot.OuterTransactionID,
		journal.snapshot.CandidateSHA256,
	); err != nil {
		t.Fatal(err)
	}
	afterReplay := journal.proof()
	if beforeParentRecord != afterReplay || afterReplay.State != string(nativeBrokerPhaseNestedReady) {
		t.Fatalf("replayed proof changed across parent cut: before=%+v after=%+v",
			beforeParentRecord, afterReplay)
	}
	if err := validateNativeBrokerNestedReplayBinding(
		journal, journal.snapshot.TargetUserSID, journal.snapshot.OuterTokenPath,
		strings.Repeat("0", 64),
		journal.snapshot.CandidateSHA256,
	); err == nil {
		t.Fatal("nested-ready replay accepted a different outer transaction identity")
	}
	if err := validateNativeBrokerNestedReplayBinding(
		journal, journal.snapshot.TargetUserSID,
		`C:\Program Files\VIIPER\.viiper.transaction.other.token`,
		journal.snapshot.OuterTransactionID, journal.snapshot.CandidateSHA256,
	); err == nil {
		t.Fatal("nested-ready replay accepted a different outer token path")
	}
}

func TestNativeBrokerJournalReplayedOuterBindingSelectsOldTransactionIdentity(t *testing.T) {
	t.Parallel()
	currentOuter := strings.Repeat("1", 64)
	currentCandidate := strings.Repeat("2", 64)
	oldOuter := strings.Repeat("3", 64)
	oldCandidate := strings.Repeat("4", 64)
	proof := nativePackageInstallProof{
		success: true, changed: true, exitCode: 0, journalRecovery: "replayed",
		journal: nativeBrokerJournalProof{
			TransactionID: strings.Repeat("5", 32), OuterTransactionID: oldOuter,
			CandidateSHA256: oldCandidate, State: string(nativeBrokerPhaseNestedReady),
			Digest: strings.Repeat("6", 64),
		},
	}
	outer, candidate := nativeBrokerJournalOuterSettlementIdentity(
		currentOuter, currentCandidate, proof,
	)
	if outer != oldOuter || candidate != oldCandidate {
		t.Fatalf("replayed identity=(%s,%s) want old=(%s,%s)",
			outer, candidate, oldOuter, oldCandidate)
	}
	proof.journalRecovery = ""
	outer, candidate = nativeBrokerJournalOuterSettlementIdentity(
		currentOuter, currentCandidate, proof,
	)
	if outer != currentOuter || candidate != currentCandidate {
		t.Fatal("unbound proof replaced the current outer transaction identity")
	}
}

func TestNativeBrokerJournalTransactionDirectoryNamesAreCanonical(t *testing.T) {
	t.Parallel()
	valid := strings.Repeat("a", 32)
	if !isNativeBrokerJournalInactiveDirectoryName(nativeBrokerJournalPreparingPrefix+valid,
		nativeBrokerJournalPreparingPrefix) ||
		!isNativeBrokerJournalInactiveDirectoryName(nativeBrokerJournalSettledPrefix+valid,
			nativeBrokerJournalSettledPrefix) {
		t.Fatal("canonical transaction directory name was rejected")
	}
	for _, invalid := range []string{
		strings.ToUpper(valid), valid[:31], valid + "0", strings.Repeat("z", 32),
	} {
		if isNativeBrokerJournalTransactionID(invalid) {
			t.Fatalf("noncanonical transaction directory identity was accepted: %q", invalid)
		}
	}
}

func TestNativeBrokerJournalSnapshotBindsExactOuterTokenPath(t *testing.T) {
	t.Parallel()
	journal, _ := newNativeBrokerJournalModel(t)
	if err := validateNativeBrokerJournalOuterTokenPath(journal.snapshot); err != nil {
		t.Fatalf("canonical snapshot was rejected: %v", err)
	}
	journal.snapshot.OuterTokenPath =
		`C:\Program Files\Other\.viiper.transaction.test.token`
	if err := validateNativeBrokerJournalOuterTokenPath(journal.snapshot); err == nil {
		t.Fatal("snapshot accepted an outer token outside the candidate image directory")
	}
}

func TestStandaloneNativeBrokerInstallIsFailClosedByDefault(t *testing.T) {
	t.Setenv("VIIPER_DEVELOPER_STANDALONE", "")
	err := requireDeveloperStandaloneNativeInstall()
	if err == nil || !strings.Contains(err.Error(), "developer-only") {
		t.Fatalf("default standalone native install did not fail before mutation: %v", err)
	}
}

func TestStandaloneNativeBrokerInstallRequiresExactDeveloperOptIn(t *testing.T) {
	for _, value := range []string{"true", "01", " 1", "1 "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("VIIPER_DEVELOPER_STANDALONE", value)
			if err := requireDeveloperStandaloneNativeInstall(); err == nil {
				t.Fatalf("noncanonical developer opt-in %q was accepted", value)
			}
		})
	}
	t.Setenv("VIIPER_DEVELOPER_STANDALONE", "1")
	if err := requireDeveloperStandaloneNativeInstall(); err != nil {
		t.Fatalf("exact developer opt-in was rejected: %v", err)
	}
}

func TestRecoveredPriorPackageRequiresExplicitRetry(t *testing.T) {
	t.Parallel()
	err := error(&nativePackageRecoveryRetryError{})
	if !strings.Contains(err.Error(), "retry") || !strings.Contains(err.Error(), "settled") {
		t.Fatalf("recovery retry error is not explicit: %v", err)
	}
}

func TestNativeBrokerJournalAbsenceRequiresSettledChildOutcome(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		proof nativePackageInstallProof
		want  bool
	}{
		{
			name: "successful healthy child no-op after driver mutation",
			proof: nativePackageInstallProof{
				success: true,
				changed: true,
			},
			want: true,
		},
		{
			name: "successful child advertises durable identity",
			proof: nativePackageInstallProof{
				success: true,
				journal: nativeBrokerJournalProof{
					TransactionID: strings.Repeat("a", 32),
				},
			},
			want: false,
		},
		{
			name: "failed changed child completed rollback",
			proof: nativePackageInstallProof{
				changed:  true,
				rollback: "succeeded",
			},
			want: true,
		},
		{
			name: "failed child performed no mutation",
			proof: nativePackageInstallProof{
				rollback: "not-needed",
			},
			want: true,
		},
		{
			name: "failed changed child has unsettled rollback",
			proof: nativePackageInstallProof{
				changed:  true,
				rollback: "failed",
			},
			want: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := nativeBrokerJournalAbsenceIsSettled(test.proof); got != test.want {
				t.Fatalf("nativeBrokerJournalAbsenceIsSettled()=%t want %t", got, test.want)
			}
		})
	}
}

func TestNativeBrokerJournalProofContainsNoProtectedPayload(t *testing.T) {
	t.Parallel()
	result := nativePackageBrokerCommitResult{
		success: true, changed: true, rollback: "not-needed", exitCode: 0,
		journal: nativeBrokerJournalProof{
			TransactionID: strings.Repeat("a", 32), OuterTransactionID: strings.Repeat("c", 64),
			CandidateSHA256: strings.Repeat("d", 64), State: string(nativeBrokerPhaseNestedReady),
			Digest: strings.Repeat("b", 64),
		},
	}
	line := result.journalProofLine()
	if !strings.Contains(line, "transactionId=") || !strings.Contains(line, "outerTransactionId=") ||
		!strings.Contains(line, "candidateSha256=") ||
		!strings.Contains(line, "digest=") ||
		strings.Contains(strings.ToLower(line), "password") || strings.Contains(line, "scheduledXml") {
		t.Fatalf("unsafe journal proof=%q", line)
	}
}

func TestNativePackageInstallProofRequiresCanonicalJournalBinding(t *testing.T) {
	t.Parallel()
	base := "result=success operation=install changed=1 rebootRequired=0 rollback=not-needed exitCode=0\n"
	proof, err := parseNativePackageInstallProof(base, 0)
	if err != nil {
		t.Fatal(err)
	}
	if proof.journal.TransactionID != "" {
		t.Fatal("install proof invented an absent journal binding")
	}
	binding := "journal-binding operation=install transactionId=" + strings.Repeat("a", 32) +
		" outerTransactionId=" + strings.Repeat("c", 64) +
		" candidateSha256=" + strings.Repeat("d", 64) +
		" state=nested-ready digest=" + strings.Repeat("b", 64) +
		" driverTransactionId=" + strings.Repeat("e", 64) +
		" driverDigest=" + strings.Repeat("f", 64) +
		" settlementNonce=" + strings.Repeat("0", 64) + " recovery=fresh\n"
	proof, err = parseNativePackageInstallProof(base+binding, 0)
	if err != nil {
		t.Fatal(err)
	}
	if proof.journal.TransactionID != strings.Repeat("a", 32) ||
		proof.journal.OuterTransactionID != strings.Repeat("c", 64) ||
		proof.journal.CandidateSHA256 != strings.Repeat("d", 64) ||
		proof.journal.State != "nested-ready" || proof.journal.Digest != strings.Repeat("b", 64) ||
		proof.driverTransactionID != strings.Repeat("e", 64) ||
		proof.driverPendingDigest != strings.Repeat("f", 64) ||
		proof.settlementNonce != strings.Repeat("0", 64) ||
		proof.journalRecovery != "fresh" {
		t.Fatalf("journal binding=%+v", proof.journal)
	}
	replayed := strings.Replace(binding, "recovery=fresh", "recovery=replayed", 1)
	proof, err = parseNativePackageInstallProof(base+replayed, 0)
	if err != nil || proof.journalRecovery != "replayed" {
		t.Fatalf("canonical replayed binding was rejected: proof=%+v err=%v", proof, err)
	}
	if _, err := parseNativePackageInstallProof(base+binding+binding, 0); err == nil {
		t.Fatal("duplicate journal binding was accepted")
	}
	for _, malformed := range []string{
		strings.TrimSuffix(binding, "\n"),
		binding + "journal-binding operation=install transactionId=not-canonical\n",
		strings.Replace(binding, "nested-ready", "Nested-Ready", 1),
		strings.Replace(binding, "recovery=fresh", "recovery=unknown", 1),
	} {
		if _, err := parseNativePackageInstallProof(base+malformed, 0); err == nil {
			t.Fatalf("noncanonical journal binding was accepted: %q", malformed)
		}
	}
	failure := "result=error operation=install changed=1 rebootRequired=0 rollback=succeeded exitCode=1\n"
	if _, err := parseNativePackageInstallProof(failure+binding, 1); err == nil {
		t.Fatal("failure outcome carried an unauthorized forward journal binding")
	}
}

func TestNativePackageBrokerSettlementDiscardReceiptIsCanonical(t *testing.T) {
	t.Parallel()
	line := "journal-discard operation=broker-settlement-discard" +
		" brokerTransactionId=" + strings.Repeat("a", 32) +
		" brokerDigest=" + strings.Repeat("b", 64) +
		" driverTransactionId=" + strings.Repeat("c", 64) +
		" driverDigest=" + strings.Repeat("d", 64) +
		" settlementNonce=" + strings.Repeat("e", 64) +
		" requestSha256=" + strings.Repeat("f", 64) +
		" discarded=1 retained=1\n"
	receipt, err := parseNativePackageBrokerSettlementDiscardReceipt(line, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Discarded || !receipt.Retained ||
		receipt.BrokerTransactionID != strings.Repeat("a", 32) ||
		receipt.BrokerDigest != strings.Repeat("b", 64) ||
		receipt.DriverTransactionID != strings.Repeat("c", 64) ||
		receipt.DriverDigest != strings.Repeat("d", 64) ||
		receipt.SettlementNonce != strings.Repeat("e", 64) ||
		receipt.RequestSHA256 != strings.Repeat("f", 64) {
		t.Fatalf("discard receipt=%+v", receipt)
	}
	for _, malformed := range []string{
		strings.Replace(line, " retained=1", "", 1),
		strings.Replace(line, "retained=1", "retained=true", 1),
		strings.TrimSuffix(line, "\n"),
		line + line,
	} {
		if _, err := parseNativePackageBrokerSettlementDiscardReceipt(malformed, 0); err == nil {
			t.Fatalf("noncanonical discard receipt was accepted: %q", malformed)
		}
	}
}
