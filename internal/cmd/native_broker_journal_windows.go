//go:build windows

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	nativeBrokerJournalSchema             = 1
	nativeBrokerJournalMaximumRecords     = 96
	nativeBrokerJournalMaximumLine        = 16 * 1024
	nativeBrokerJournalMaximumSnapshot    = 128 * 1024
	nativeBrokerJournalMaximumSecret      = 512 * 1024
	nativeBrokerJournalMaximumImage       = 128 * 1024 * 1024
	nativeBrokerJournalMaximumSettlement  = 16 * 1024
	nativeBrokerJournalRootName           = "BrokerTransactions"
	nativeBrokerJournalActiveName         = "active-v1"
	nativeBrokerJournalPreparingPrefix    = "preparing-"
	nativeBrokerJournalSettledPrefix      = "settled-"
	nativeBrokerJournalSnapshotName       = "snapshot.json"
	nativeBrokerJournalRecordsName        = "journal.jsonl"
	nativeBrokerJournalCredentialName     = "prior-key.dpapi"
	nativeBrokerJournalLegacyName         = "prior-legacy.dpapi"
	nativeBrokerJournalPriorImageName     = "prior-image.exe"
	nativeBrokerJournalSettlementName     = "outer-settlement.json"
	nativeBrokerJournalSettledReceiptName = "outer-settled.json"
	nativeBrokerJournalSDDL               = "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	nativeBrokerJournalFileSDDL           = "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"
)

const (
	nativeBrokerCutRecordPartialWrite      = "record-stage-partial-write"
	nativeBrokerCutRecordWriteDone         = "record-stage-write-complete"
	nativeBrokerCutRecordSyncDone          = "record-stage-sync-complete"
	nativeBrokerCutRecordReadbackDone      = "record-stage-readback-complete"
	nativeBrokerCutRecordBeforePublish     = "record-stage-before-publish"
	nativeBrokerCutAfterBindingOutput      = "settlement-after-binding-output"
	nativeBrokerCutBeforePending           = "settlement-before-broker-pending"
	nativeBrokerCutAfterPending            = "settlement-after-broker-pending"
	nativeBrokerCutAfterRequest            = "settlement-after-request-published"
	nativeBrokerCutBeforeDriverAck         = "settlement-before-driver-ack"
	nativeBrokerCutAfterDriverAck          = "settlement-after-driver-ack"
	nativeBrokerCutBeforeBrokerFinal       = "settlement-before-broker-final"
	nativeBrokerCutAfterBrokerFinal        = "settlement-after-broker-final"
	nativeBrokerCutBeforeRetirement        = "settlement-before-broker-retirement"
	nativeBrokerCutAfterRetirement         = "settlement-after-broker-retirement"
	nativeBrokerCutAfterDiscard            = "settlement-after-best-effort-discard"
	nativeBrokerCutSettlementPartialWrite  = "settlement-request-partial-write"
	nativeBrokerCutSettlementWriteDone     = "settlement-request-write-complete"
	nativeBrokerCutSettlementSyncDone      = "settlement-request-sync-complete"
	nativeBrokerCutSettlementReadbackDone  = "settlement-request-readback-complete"
	nativeBrokerCutSettlementBeforePublish = "settlement-request-before-publish"
)

type nativeBrokerJournalPhase string

const (
	nativeBrokerPhasePrepared               nativeBrokerJournalPhase = "prepared"
	nativeBrokerPhaseServiceStopIntent      nativeBrokerJournalPhase = "service-stop-intent"
	nativeBrokerPhaseServiceStopped         nativeBrokerJournalPhase = "service-stopped"
	nativeBrokerPhaseImageSwitchIntent      nativeBrokerJournalPhase = "image-switch-intent"
	nativeBrokerPhaseImageSwitched          nativeBrokerJournalPhase = "image-switched"
	nativeBrokerPhaseLegacyStopIntent       nativeBrokerJournalPhase = "legacy-stop-intent"
	nativeBrokerPhaseLegacyStopped          nativeBrokerJournalPhase = "legacy-stopped"
	nativeBrokerPhaseCredentialWriteIntent  nativeBrokerJournalPhase = "credential-write-intent"
	nativeBrokerPhaseCredentialWritten      nativeBrokerJournalPhase = "credential-written"
	nativeBrokerPhaseServiceConfigIntent    nativeBrokerJournalPhase = "service-config-intent"
	nativeBrokerPhaseServiceConfigured      nativeBrokerJournalPhase = "service-configured"
	nativeBrokerPhaseServiceStartIntent     nativeBrokerJournalPhase = "service-start-intent"
	nativeBrokerPhaseServiceStarted         nativeBrokerJournalPhase = "service-started"
	nativeBrokerPhaseAuthenticated          nativeBrokerJournalPhase = "authenticated"
	nativeBrokerPhaseLegacyRemoveIntent     nativeBrokerJournalPhase = "legacy-remove-intent"
	nativeBrokerPhaseLegacyRemoved          nativeBrokerJournalPhase = "legacy-removed"
	nativeBrokerPhaseReauthenticated        nativeBrokerJournalPhase = "reauthenticated"
	nativeBrokerPhaseNestedReady            nativeBrokerJournalPhase = "nested-ready"
	nativeBrokerPhaseOuterSettlementPending nativeBrokerJournalPhase = "outer-settlement-pending"
	nativeBrokerPhaseOuterSettled           nativeBrokerJournalPhase = "outer-settled"
	nativeBrokerPhaseRollbackIntent         nativeBrokerJournalPhase = "rollback-intent"
	nativeBrokerPhaseRollbackService        nativeBrokerJournalPhase = "rollback-service"
	nativeBrokerPhaseRollbackImage          nativeBrokerJournalPhase = "rollback-image"
	nativeBrokerPhaseRollbackCredential     nativeBrokerJournalPhase = "rollback-credential"
	nativeBrokerPhaseRollbackLegacy         nativeBrokerJournalPhase = "rollback-legacy"
	nativeBrokerPhaseRollbackSettled        nativeBrokerJournalPhase = "rollback-settled"
	nativeBrokerPhaseManual                 nativeBrokerJournalPhase = "manual"
)

var nativeBrokerForwardPhaseOrder = []nativeBrokerJournalPhase{
	nativeBrokerPhasePrepared,
	nativeBrokerPhaseServiceStopIntent,
	nativeBrokerPhaseServiceStopped,
	nativeBrokerPhaseImageSwitchIntent,
	nativeBrokerPhaseImageSwitched,
	nativeBrokerPhaseLegacyStopIntent,
	nativeBrokerPhaseLegacyStopped,
	nativeBrokerPhaseCredentialWriteIntent,
	nativeBrokerPhaseCredentialWritten,
	nativeBrokerPhaseServiceConfigIntent,
	nativeBrokerPhaseServiceConfigured,
	nativeBrokerPhaseServiceStartIntent,
	nativeBrokerPhaseServiceStarted,
	nativeBrokerPhaseAuthenticated,
	nativeBrokerPhaseLegacyRemoveIntent,
	nativeBrokerPhaseLegacyRemoved,
	nativeBrokerPhaseReauthenticated,
	nativeBrokerPhaseNestedReady,
	nativeBrokerPhaseOuterSettlementPending,
	nativeBrokerPhaseOuterSettled,
}

var nativeBrokerRollbackPhaseOrder = []nativeBrokerJournalPhase{
	nativeBrokerPhaseRollbackIntent,
	nativeBrokerPhaseRollbackService,
	nativeBrokerPhaseRollbackCredential,
	nativeBrokerPhaseRollbackImage,
	nativeBrokerPhaseRollbackLegacy,
	nativeBrokerPhaseRollbackSettled,
}

type nativeBrokerJournalService struct {
	Exists               bool                 `json:"exists"`
	WasRunning           bool                 `json:"wasRunning"`
	Config               mgr.Config           `json:"config"`
	SecurityDescriptor   string               `json:"securityDescriptor"`
	RecoveryActions      []mgr.RecoveryAction `json:"recoveryActions"`
	RecoveryResetSeconds uint32               `json:"recoveryResetSeconds"`
	RecoverNonCrash      bool                 `json:"recoverNonCrash"`
}

type nativeBrokerJournalSnapshot struct {
	Schema                  int                        `json:"schema"`
	TransactionID           string                     `json:"transactionId"`
	OuterTransactionID      string                     `json:"outerTransactionId"`
	OuterTokenPath          string                     `json:"outerTokenPath"`
	TargetUserSID           string                     `json:"targetUserSid"`
	CandidatePath           string                     `json:"candidatePath"`
	CandidateSHA256         string                     `json:"candidateSha256"`
	PriorImageExists        bool                       `json:"priorImageExists"`
	PriorImagePath          string                     `json:"priorImagePath"`
	PriorImageSHA256        string                     `json:"priorImageSha256"`
	PriorCredentialSHA256   string                     `json:"priorCredentialSha256"`
	PriorCredentialExists   bool                       `json:"priorCredentialExists"`
	PriorCredentialArtifact string                     `json:"priorCredentialArtifactSha256"`
	PriorLegacyArtifact     string                     `json:"priorLegacyArtifactSha256"`
	Service                 nativeBrokerJournalService `json:"service"`
}

type nativeBrokerJournalSnapshotEnvelope struct {
	Schema        int                         `json:"schema"`
	PayloadSHA256 string                      `json:"payloadSha256"`
	Payload       nativeBrokerJournalSnapshot `json:"payload"`
}

type nativeBrokerOuterSettlementBinding struct {
	Schema                   int    `json:"schema"`
	BrokerTransactionID      string `json:"brokerTransactionId"`
	BrokerOuterTransactionID string `json:"brokerOuterTransactionId"`
	BrokerCandidateSHA256    string `json:"brokerCandidateSha256"`
	BrokerNestedDigest       string `json:"brokerNestedDigest"`
	DriverTransactionID      string `json:"driverTransactionId"`
	DriverPendingDigest      string `json:"driverPendingDigest"`
	SettlementNonce          string `json:"settlementNonce"`
}

type nativeBrokerOuterSettlementRequest struct {
	Schema              int                                `json:"schema"`
	BindingSHA256       string                             `json:"bindingSha256"`
	BrokerPendingDigest string                             `json:"brokerPendingDigest"`
	Binding             nativeBrokerOuterSettlementBinding `json:"binding"`
}

type nativeBrokerOuterSettlementEnvelope struct {
	Schema        int                                `json:"schema"`
	PayloadSHA256 string                             `json:"payloadSha256"`
	Payload       nativeBrokerOuterSettlementRequest `json:"payload"`
}

type nativeBrokerOuterSettlementPrepared struct {
	Request       nativeBrokerOuterSettlementRequest
	RequestPath   string
	RequestSHA256 string
	contents      []byte
}

type nativeBrokerOuterSettlementFinal struct {
	Schema              int    `json:"schema"`
	BrokerTransactionID string `json:"brokerTransactionId"`
	BrokerPendingDigest string `json:"brokerPendingDigest"`
	BrokerSettledDigest string `json:"brokerSettledDigest"`
	DriverTransactionID string `json:"driverTransactionId"`
	DriverPendingDigest string `json:"driverPendingDigest"`
	DriverSettledDigest string `json:"driverSettledDigest"`
	SettlementNonce     string `json:"settlementNonce"`
	RequestSHA256       string `json:"requestSha256"`
	State               string `json:"state"`
}

type nativeBrokerOuterSettlementFinalEnvelope struct {
	Schema        int                              `json:"schema"`
	PayloadSHA256 string                           `json:"payloadSha256"`
	Payload       nativeBrokerOuterSettlementFinal `json:"payload"`
}

type nativeBrokerOuterSettlementFinalPrepared struct {
	Receipt       nativeBrokerOuterSettlementFinal
	ReceiptPath   string
	ReceiptSHA256 string
	contents      []byte
}

func nativeBrokerDriverReceiptFromFinal(
	receipt nativeBrokerOuterSettlementFinal,
) nativePackageBrokerSettlementReceipt {
	return nativePackageBrokerSettlementReceipt{
		BrokerTransactionID: receipt.BrokerTransactionID,
		BrokerPendingDigest: receipt.BrokerPendingDigest,
		DriverTransactionID: receipt.DriverTransactionID,
		DriverPendingDigest: receipt.DriverPendingDigest,
		SettlementNonce:     receipt.SettlementNonce,
		RequestSHA256:       receipt.RequestSHA256,
		State:               receipt.State,
		Digest:              receipt.DriverSettledDigest,
	}
}

type nativeBrokerJournalRecordUnsigned struct {
	Schema         int                      `json:"schema"`
	Sequence       uint32                   `json:"sequence"`
	TransactionID  string                   `json:"transactionId"`
	Phase          nativeBrokerJournalPhase `json:"phase"`
	PreviousSHA256 string                   `json:"previousSha256"`
	SnapshotSHA256 string                   `json:"snapshotSha256"`
	DetailSHA256   string                   `json:"detailSha256"`
}

type nativeBrokerJournalRecord struct {
	Schema         int                      `json:"schema"`
	Sequence       uint32                   `json:"sequence"`
	TransactionID  string                   `json:"transactionId"`
	Phase          nativeBrokerJournalPhase `json:"phase"`
	PreviousSHA256 string                   `json:"previousSha256"`
	SnapshotSHA256 string                   `json:"snapshotSha256"`
	DetailSHA256   string                   `json:"detailSha256"`
	RecordSHA256   string                   `json:"recordSha256"`
}

type nativeBrokerJournalCredentialSnapshot struct {
	Schema int    `json:"schema"`
	Exists bool   `json:"exists"`
	Bytes  []byte `json:"bytes"`
}

type nativeBrokerJournalLegacySnapshot struct {
	Schema           int                    `json:"schema"`
	UserSID          string                 `json:"userSid"`
	RunKeyExisted    bool                   `json:"runKeyExisted"`
	RunValue         *nativeRunRegistration `json:"-"`
	RunValueText     *string                `json:"runValue,omitempty"`
	RunValueType     uint32                 `json:"runValueType"`
	ScheduledXML     *string                `json:"scheduledXml,omitempty"`
	ScheduledActive  bool                   `json:"scheduledActive"`
	ScheduledEnabled bool                   `json:"scheduledEnabled"`
	Commands         []nativeLegacyCommand  `json:"-"`
	SerializableCmds []nativeBrokerCommand  `json:"commands"`
}

type nativeBrokerCommand struct {
	Executable       string   `json:"executable"`
	Arguments        []string `json:"arguments"`
	WorkingDirectory string   `json:"workingDirectory"`
	Source           uint8    `json:"source"`
	WasRunning       bool     `json:"wasRunning"`
}

type nativeBrokerJournal struct {
	directory      string
	snapshot       nativeBrokerJournalSnapshot
	snapshotDigest string
	records        []nativeBrokerJournalRecord
	priorLegacy    *nativeBrokerJournalLegacySnapshot
	cutpoint       func(string) error
	appendRecord   func([]byte) error
}

type nativeBrokerJournalManualError struct {
	cause error
}

type nativeBrokerJournalRetirementOperations struct {
	rename            func() error
	proveActiveAbsent func() error
	proveTombstone    func() error
	discardTombstone  func() error
	cutpoint          func(string) error
}

type nativeBrokerJournalRecordPublicationOperations struct {
	loadCurrent    func() ([]byte, error)
	discardStaging func() error
	stage          func([]byte) error
	beforePublish  func() error
	publish        func() error
}

type nativeBrokerJournalPreparationOperations struct {
	createDirectory    func() error
	writeCredential    func() error
	writeLegacy        func() error
	writePriorImage    func() error
	writeSnapshot      func() error
	createRecordStream func() error
	writePrepared      func() error
	publishActive      func() error
	cutpoint           func(string) error
}

type nativeBrokerOuterSettlementOperations struct {
	recordPending       func() error
	publishRequest      func() error
	acknowledgeDriver   func() error
	recordBrokerSettled func() error
	retireBrokerJournal func() error
	discardInertState   func() error
	observeDiscardError func(error)
	cutpoint            func(string) error
}

type nativeBrokerSettlementPublicationOperations struct {
	loadPublished  func() ([]byte, bool, error)
	loadStaging    func() ([]byte, bool, error)
	discardStaging func() error
	publishStaging func() error
	writeNew       func() error
	readback       func() ([]byte, error)
}

func executeNativeBrokerJournalPreparation(
	operations nativeBrokerJournalPreparationOperations,
) error {
	steps := []struct {
		name string
		run  func() error
	}{
		{"directory-created", operations.createDirectory},
		{"credential-written", operations.writeCredential},
		{"legacy-written", operations.writeLegacy},
		{"prior-image-written", operations.writePriorImage},
		{"snapshot-written", operations.writeSnapshot},
		{"record-stream-created", operations.createRecordStream},
		{"prepared-written", operations.writePrepared},
		{"active-published", operations.publishActive},
	}
	for _, step := range steps {
		if step.run == nil {
			return fmt.Errorf("native broker journal preparation operation %s is missing", step.name)
		}
		if err := step.run(); err != nil {
			return err
		}
		if operations.cutpoint != nil {
			if err := operations.cutpoint("prepare-" + step.name); err != nil {
				return err
			}
		}
	}
	return nil
}

func executeNativeBrokerOuterSettlement(
	operations nativeBrokerOuterSettlementOperations,
) error {
	if operations.recordPending == nil || operations.publishRequest == nil ||
		operations.acknowledgeDriver == nil || operations.recordBrokerSettled == nil ||
		operations.retireBrokerJournal == nil || operations.discardInertState == nil {
		return errors.New("native broker outer settlement operations are incomplete")
	}
	cut := func(name string) error {
		if operations.cutpoint == nil {
			return nil
		}
		return operations.cutpoint(name)
	}
	if err := cut(nativeBrokerCutAfterBindingOutput); err != nil {
		return err
	}
	if err := cut(nativeBrokerCutBeforePending); err != nil {
		return err
	}
	if err := operations.recordPending(); err != nil {
		return err
	}
	if err := cut(nativeBrokerCutAfterPending); err != nil {
		return err
	}
	if err := operations.publishRequest(); err != nil {
		return err
	}
	if err := cut(nativeBrokerCutAfterRequest); err != nil {
		return err
	}
	if err := cut(nativeBrokerCutBeforeDriverAck); err != nil {
		return err
	}
	if err := operations.acknowledgeDriver(); err != nil {
		return err
	}
	if err := cut(nativeBrokerCutAfterDriverAck); err != nil {
		return err
	}
	if err := cut(nativeBrokerCutBeforeBrokerFinal); err != nil {
		return err
	}
	if err := operations.recordBrokerSettled(); err != nil {
		return err
	}
	if err := cut(nativeBrokerCutAfterBrokerFinal); err != nil {
		return err
	}
	// The protected broker-final receipt is now authoritative. The driver
	// tombstone may only leave exact settled discovery after validating that
	// receipt; any recursive cleanup after its atomic rename is inert.
	if err := operations.discardInertState(); err != nil {
		if operations.observeDiscardError != nil {
			operations.observeDiscardError(err)
		}
		return err
	}
	if err := cut(nativeBrokerCutAfterDiscard); err != nil {
		return err
	}
	if err := cut(nativeBrokerCutBeforeRetirement); err != nil {
		return err
	}
	if err := operations.retireBrokerJournal(); err != nil {
		return err
	}
	if err := cut(nativeBrokerCutAfterRetirement); err != nil {
		return err
	}
	return nil
}

func executeNativeBrokerSettlementPublication(
	expected []byte,
	operations nativeBrokerSettlementPublicationOperations,
) error {
	if len(expected) == 0 || operations.loadPublished == nil ||
		operations.loadStaging == nil || operations.discardStaging == nil ||
		operations.publishStaging == nil || operations.writeNew == nil ||
		operations.readback == nil {
		return errors.New("native broker settlement publication operations are incomplete")
	}
	published, publishedExists, err := operations.loadPublished()
	if err != nil {
		if publishedExists {
			return &nativeBrokerJournalManualError{cause: fmt.Errorf(
				"read published broker settlement request: %w", err,
			)}
		}
		return err
	}
	staged, stagingExists, stagingErr := operations.loadStaging()
	if stagingExists {
		if !publishedExists && stagingErr == nil && bytes.Equal(staged, expected) {
			if err := operations.publishStaging(); err != nil {
				return err
			}
			published, publishedExists = expected, true
		} else if err := operations.discardStaging(); err != nil {
			return err
		}
	} else if stagingErr != nil {
		return stagingErr
	}
	if publishedExists {
		if !bytes.Equal(published, expected) {
			return &nativeBrokerJournalManualError{cause: errors.New(
				"published broker settlement request differs from the authoritative binding",
			)}
		}
	} else if err := operations.writeNew(); err != nil {
		return err
	}
	readback, err := operations.readback()
	if err != nil {
		return err
	}
	if !bytes.Equal(readback, expected) {
		return errors.New("broker settlement request failed write-through readback")
	}
	return nil
}

func (e *nativeBrokerJournalManualError) Error() string {
	return "native broker recovery requires manual reconciliation: " + e.cause.Error()
}

func (e *nativeBrokerJournalManualError) Unwrap() error { return e.cause }

func nativeBrokerJournalHash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func isCanonicalNativeBrokerJournalSHA256(value string) bool {
	return value == strings.ToLower(value) && nativePackageSHA256.MatchString(value)
}

func nativeBrokerJournalCanonicalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, '\n') >= 0 || len(data) > nativeBrokerJournalMaximumSnapshot {
		return nil, errors.New("native broker journal canonical payload exceeds its bound")
	}
	return data, nil
}

func decodeCanonicalNativeBrokerJSON(data []byte, value any, maximum int) error {
	if len(data) == 0 || len(data) > maximum || bytes.IndexByte(data, '\n') >= 0 {
		return errors.New("native broker journal payload has an invalid length or framing")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("native broker journal payload has trailing JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("native broker journal payload has trailing data")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, data) {
		return errors.New("native broker journal payload is not in canonical byte form")
	}
	return nil
}

func nativeBrokerJournalPhaseIndex(phases []nativeBrokerJournalPhase, phase nativeBrokerJournalPhase) int {
	return slices.Index(phases, phase)
}

func validateNativeBrokerJournalTransition(previous, next nativeBrokerJournalPhase) error {
	if next == nativeBrokerPhaseManual {
		if previous == nativeBrokerPhaseOuterSettled || previous == nativeBrokerPhaseRollbackSettled {
			return errors.New("settled native broker journal cannot become manual")
		}
		return nil
	}
	if previous == nativeBrokerPhaseManual || previous == nativeBrokerPhaseOuterSettled ||
		previous == nativeBrokerPhaseRollbackSettled {
		return fmt.Errorf("native broker journal phase %s is terminal", previous)
	}
	if next == nativeBrokerPhaseOuterSettlementPending && previous != nativeBrokerPhaseNestedReady {
		return errors.New("native broker outer settlement must be armed directly from nested-ready")
	}
	if next == nativeBrokerPhaseOuterSettled && previous != nativeBrokerPhaseOuterSettlementPending {
		return errors.New("native broker outer settlement requires the durable pending handshake")
	}
	previousForward := nativeBrokerJournalPhaseIndex(nativeBrokerForwardPhaseOrder, previous)
	nextForward := nativeBrokerJournalPhaseIndex(nativeBrokerForwardPhaseOrder, next)
	previousRollback := nativeBrokerJournalPhaseIndex(nativeBrokerRollbackPhaseOrder, previous)
	nextRollback := nativeBrokerJournalPhaseIndex(nativeBrokerRollbackPhaseOrder, next)
	if next == nativeBrokerPhaseRollbackIntent && previousForward >= 0 {
		return nil
	}
	if previousRollback >= 0 && nextRollback > previousRollback {
		return nil
	}
	if previousForward >= 0 && nextForward > previousForward {
		return nil
	}
	return fmt.Errorf("invalid native broker journal transition %s -> %s", previous, next)
}

func (j *nativeBrokerJournal) lastPhase() nativeBrokerJournalPhase {
	if len(j.records) == 0 {
		return ""
	}
	return j.records[len(j.records)-1].Phase
}

func (j *nativeBrokerJournal) proof() nativeBrokerJournalProof {
	digest := ""
	if len(j.records) != 0 {
		digest = j.records[len(j.records)-1].RecordSHA256
	}
	return nativeBrokerJournalProof{
		TransactionID:      j.snapshot.TransactionID,
		OuterTransactionID: j.snapshot.OuterTransactionID,
		CandidateSHA256:    j.snapshot.CandidateSHA256,
		State:              string(j.lastPhase()),
		Digest:             digest,
	}
}

func (j *nativeBrokerJournal) appendPhase(phase nativeBrokerJournalPhase, detailSHA256 string) error {
	if j == nil {
		return nil
	}
	if detailSHA256 != "" && !isCanonicalNativeBrokerJournalSHA256(detailSHA256) {
		return errors.New("native broker journal detail digest is malformed")
	}
	if (phase == nativeBrokerPhaseOuterSettlementPending ||
		phase == nativeBrokerPhaseOuterSettled) && detailSHA256 == "" {
		return errors.New("native broker two-phase settlement record requires a bound detail digest")
	}
	if len(j.records) == 0 {
		if phase != nativeBrokerPhasePrepared {
			return errors.New("native broker journal must begin with prepared")
		}
	} else {
		if j.lastPhase() == phase && j.records[len(j.records)-1].DetailSHA256 == detailSHA256 {
			return nil
		}
		previousRollback := nativeBrokerJournalPhaseIndex(
			nativeBrokerRollbackPhaseOrder, j.lastPhase(),
		)
		nextRollback := nativeBrokerJournalPhaseIndex(nativeBrokerRollbackPhaseOrder, phase)
		if previousRollback >= 0 && nextRollback >= 0 && nextRollback <= previousRollback {
			return nil
		}
		if err := validateNativeBrokerJournalTransition(j.lastPhase(), phase); err != nil {
			return err
		}
	}
	if len(j.records) >= nativeBrokerJournalMaximumRecords {
		return errors.New("native broker journal record bound exhausted")
	}
	if j.cutpoint != nil {
		if err := j.cutpoint("before-record-" + string(phase)); err != nil {
			return err
		}
	}
	previous := strings.Repeat("0", 64)
	if len(j.records) != 0 {
		previous = j.records[len(j.records)-1].RecordSHA256
	}
	unsigned := nativeBrokerJournalRecordUnsigned{
		Schema: nativeBrokerJournalSchema, Sequence: uint32(len(j.records) + 1),
		TransactionID: j.snapshot.TransactionID, Phase: phase,
		PreviousSHA256: previous, SnapshotSHA256: j.snapshotDigest,
		DetailSHA256: detailSHA256,
	}
	unsignedData, err := nativeBrokerJournalCanonicalJSON(unsigned)
	if err != nil {
		return err
	}
	record := nativeBrokerJournalRecord{
		Schema: unsigned.Schema, Sequence: unsigned.Sequence,
		TransactionID: unsigned.TransactionID, Phase: unsigned.Phase,
		PreviousSHA256: unsigned.PreviousSHA256, SnapshotSHA256: unsigned.SnapshotSHA256,
		DetailSHA256: unsigned.DetailSHA256, RecordSHA256: nativeBrokerJournalHash(unsignedData),
	}
	line, err := nativeBrokerJournalCanonicalJSON(record)
	if err != nil {
		return err
	}
	if len(line)+1 > nativeBrokerJournalMaximumLine {
		return errors.New("native broker journal record exceeds its bound")
	}
	appendRecord := j.appendRecord
	if appendRecord == nil {
		appendRecord = func(record []byte) error {
			return appendNativeBrokerJournalRecord(
				filepath.Join(j.directory, nativeBrokerJournalRecordsName), record, j.cutpoint,
			)
		}
	}
	if err := appendRecord(line); err != nil {
		return err
	}
	j.records = append(j.records, record)
	if j.cutpoint != nil {
		if err := j.cutpoint("after-record-" + string(phase)); err != nil {
			return err
		}
	}
	return nil
}

func nativeBrokerJournalPaths(userSID string) (string, string, error) {
	if _, err := validateNativeInstallingUserSID(userSID); err != nil {
		return "", "", err
	}
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", "", fmt.Errorf("resolve ProgramData for native broker journal: %w", err)
	}
	programData = filepath.Clean(programData)
	product := filepath.Join(programData, "VIIPER")
	root := filepath.Join(product, nativeBrokerJournalRootName)
	active := filepath.Join(root, nativeBrokerJournalActiveName)
	if !strings.EqualFold(filepath.Dir(root), product) || !strings.EqualFold(filepath.Dir(active), root) {
		return "", "", errors.New("native broker journal path escaped its fixed ProgramData root")
	}
	return root, active, nil
}

func nativeBrokerJournalActivePathUnbound() (string, error) {
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", err
	}
	programData = filepath.Clean(programData)
	root := filepath.Join(programData, "VIIPER", nativeBrokerJournalRootName)
	active := filepath.Join(root, nativeBrokerJournalActiveName)
	if !strings.EqualFold(filepath.Dir(active), root) {
		return "", errors.New("native broker active journal escaped its fixed root")
	}
	return active, nil
}

func createOrOpenProtectedNativeBrokerJournalDirectory(path string, create bool) (windows.Handle, bool, error) {
	security, err := nativeSecurityAttributes(nativeBrokerJournalSDDL)
	if err != nil {
		return 0, false, err
	}
	created := false
	if create {
		pointer, pointerErr := windows.UTF16PtrFromString(path)
		if pointerErr != nil {
			return 0, false, pointerErr
		}
		if createErr := windows.CreateDirectory(pointer, security); createErr == nil {
			created = true
		} else if !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
			return 0, false, createErr
		}
	}
	handle, err := openNativePathWithoutReparse(
		path, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, true,
	)
	if err != nil {
		return 0, created, err
	}
	if err := validateNativeSecurityDescriptor(handle, nativeBrokerJournalSDDL); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, created, err
	}
	return handle, created, nil
}

func ensureNativeBrokerJournalRoot(userSID string) (string, error) {
	root, _, err := nativeBrokerJournalPaths(userSID)
	if err != nil {
		return "", err
	}
	product := filepath.Dir(root)
	productHandle, err := secureNativeCredentialDirectory(product, userSID)
	if err != nil {
		return "", fmt.Errorf("validate native broker journal product root: %w", err)
	}
	defer windows.CloseHandle(productHandle) //nolint:errcheck
	rootHandle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(root, true)
	if err != nil {
		return "", fmt.Errorf("create or validate native broker journal root: %w", err)
	}
	windows.CloseHandle(rootHandle) //nolint:errcheck
	return root, nil
}

func isNativeBrokerJournalTransactionID(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func nativeBrokerJournalSiblingPath(root, prefix, transactionID string) (string, error) {
	if !isNativeBrokerJournalTransactionID(transactionID) {
		return "", errors.New("native broker journal directory transaction identifier is malformed")
	}
	path := filepath.Join(root, prefix+transactionID)
	if !strings.EqualFold(filepath.Dir(path), root) || filepath.Base(path) != prefix+transactionID {
		return "", errors.New("native broker journal transaction directory escaped its fixed root")
	}
	return path, nil
}

func createNativeBrokerJournalPreparingDirectory(userSID, transactionID string) (string, error) {
	root, err := ensureNativeBrokerJournalRoot(userSID)
	if err != nil {
		return "", err
	}
	_, active, err := nativeBrokerJournalPaths(userSID)
	if err != nil {
		return "", err
	}
	if _, err := nativePathAttributes(active); err == nil {
		return "", &nativeBrokerJournalManualError{cause: errors.New(
			"an active protected broker journal already exists",
		)}
	} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return "", err
	}
	preparing, err := nativeBrokerJournalSiblingPath(
		root, nativeBrokerJournalPreparingPrefix, transactionID,
	)
	if err != nil {
		return "", err
	}
	handle, created, err := createOrOpenProtectedNativeBrokerJournalDirectory(preparing, true)
	if err != nil {
		return "", fmt.Errorf("create native broker preparing journal: %w", err)
	}
	windows.CloseHandle(handle) //nolint:errcheck
	if !created {
		return "", &nativeBrokerJournalManualError{cause: errors.New(
			"a transaction-identical protected preparing journal already exists",
		)}
	}
	return preparing, nil
}

func validateNativeBrokerJournalPublishedArtifacts(j *nativeBrokerJournal) error {
	if j == nil || !isNativeBrokerJournalTransactionID(j.snapshot.TransactionID) {
		return errors.New("native broker journal publication lacks an exact transaction")
	}
	expected := map[string]bool{
		nativeBrokerJournalSnapshotName:   true,
		nativeBrokerJournalRecordsName:    true,
		nativeBrokerJournalCredentialName: true,
		nativeBrokerJournalLegacyName:     true,
	}
	if j.snapshot.PriorImageExists {
		expected[nativeBrokerJournalPriorImageName] = true
	}
	entries, err := os.ReadDir(j.directory)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return errors.New("native broker journal publication contains missing or extra artifacts")
	}
	for _, entry := range entries {
		if entry.IsDir() || !expected[entry.Name()] {
			return fmt.Errorf("native broker journal publication contains unexpected artifact %q", entry.Name())
		}
		handle, err := openNativeBrokerJournalFile(
			filepath.Join(j.directory, entry.Name()), windows.GENERIC_READ, windows.OPEN_EXISTING,
		)
		if err != nil {
			return err
		}
		windows.CloseHandle(handle) //nolint:errcheck
	}
	return nil
}

func publishNativeBrokerJournalActive(
	userSID string,
	j *nativeBrokerJournal,
) error {
	if j == nil || j.lastPhase() != nativeBrokerPhasePrepared {
		return errors.New("native broker journal is not durably prepared for publication")
	}
	root, active, err := nativeBrokerJournalPaths(userSID)
	if err != nil {
		return err
	}
	expectedPreparing, err := nativeBrokerJournalSiblingPath(
		root, nativeBrokerJournalPreparingPrefix, j.snapshot.TransactionID,
	)
	if err != nil || !strings.EqualFold(filepath.Clean(j.directory), filepath.Clean(expectedPreparing)) {
		return errors.Join(err, errors.New("native broker preparing directory identity changed before publication"))
	}
	loaded, err := loadNativeBrokerJournal(j.directory)
	if err != nil {
		return fmt.Errorf("read back prepared native broker journal: %w", err)
	}
	if loaded.lastPhase() != nativeBrokerPhasePrepared ||
		loaded.proof() != j.proof() || loaded.snapshotDigest != j.snapshotDigest {
		return errors.New("prepared native broker journal readback changed before publication")
	}
	if _, _, err := loaded.loadProtectedArtifacts(); err != nil {
		return fmt.Errorf("verify protected native broker recovery artifacts before publication: %w", err)
	}
	if err := validateNativeBrokerJournalPublishedArtifacts(loaded); err != nil {
		return err
	}
	if err := moveNativePackageFile(j.directory, active, false); err != nil {
		return fmt.Errorf("atomically publish prepared native broker journal: %w", err)
	}
	j.directory = active
	handle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(active, false)
	if err != nil {
		return fmt.Errorf("validate published native broker journal directory: %w", err)
	}
	windows.CloseHandle(handle) //nolint:errcheck
	reloaded, err := loadNativeBrokerJournal(active)
	if err != nil {
		return fmt.Errorf("read back published native broker journal: %w", err)
	}
	if reloaded.proof() != j.proof() || reloaded.snapshotDigest != j.snapshotDigest {
		return errors.New("published native broker journal differs from its prepared receipt")
	}
	return nil
}

func openNativeBrokerJournalFile(path string, access uint32, disposition uint32) (windows.Handle, error) {
	security, err := nativeSecurityAttributes(nativeBrokerJournalFileSDDL)
	if err != nil {
		return 0, err
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(
		pointer, access|windows.READ_CONTROL, windows.FILE_SHARE_READ,
		security, disposition,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT|
			windows.FILE_FLAG_WRITE_THROUGH,
		0,
	)
	if err != nil {
		return 0, err
	}
	fail := func(failErr error) (windows.Handle, error) {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, failErr
	}
	attribute := nativeFileAttributeTagInfo{}
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&attribute)),
		uint32(unsafe.Sizeof(attribute)),
	); err != nil {
		return fail(err)
	}
	if attribute.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|
		windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return fail(errors.New("native broker journal artifact is not a regular file"))
	}
	if err := requireSingleNativeFileLink(handle); err != nil {
		return fail(err)
	}
	if err := validateNativeSecurityDescriptor(handle, nativeBrokerJournalFileSDDL); err != nil {
		return fail(err)
	}
	return handle, nil
}

func writeNativeBrokerJournalFile(path string, contents []byte, maximum int) error {
	if len(contents) == 0 || len(contents) > maximum {
		return errors.New("native broker journal artifact has an invalid length")
	}
	next := path + ".next"
	handle, err := openNativeBrokerJournalFile(
		next, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.CREATE_NEW,
	)
	if err != nil {
		return fmt.Errorf("create native broker journal staging artifact: %w", err)
	}
	file := os.NewFile(uintptr(handle), next)
	if file == nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return errors.New("wrap native broker journal staging artifact")
	}
	cleanup := true
	defer func() {
		file.Close() //nolint:errcheck
		if cleanup {
			os.Remove(next) //nolint:errcheck
		}
	}()
	if _, err := file.Write(contents); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	readback, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(readback, contents) {
		return errors.New("native broker journal artifact failed write-through readback")
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := moveNativePackageFile(next, path, false); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func buildNativeBrokerJournalRecordStream(current, line []byte) ([]byte, error) {
	if len(line) == 0 || len(line)+1 > nativeBrokerJournalMaximumLine || bytes.IndexByte(line, '\n') >= 0 {
		return nil, errors.New("native broker journal record framing is invalid")
	}
	if len(current) > nativeBrokerJournalMaximumRecords*nativeBrokerJournalMaximumLine ||
		(len(current) != 0 && current[len(current)-1] != '\n') {
		return nil, errors.New("native broker journal published record stream is not exactly framed")
	}
	if bytes.Count(current, []byte{'\n'}) >= nativeBrokerJournalMaximumRecords {
		return nil, errors.New("native broker journal record stream exceeds its bound")
	}
	next := make([]byte, 0, len(current)+len(line)+1)
	next = append(next, current...)
	next = append(next, line...)
	next = append(next, '\n')
	if len(next) > nativeBrokerJournalMaximumRecords*nativeBrokerJournalMaximumLine {
		return nil, errors.New("native broker journal record stream exceeds its bound")
	}
	return next, nil
}

func discardUnpublishedNativeBrokerJournalFile(path string) error {
	handle, err := openNativeBrokerJournalFile(
		path, windows.GENERIC_READ|windows.DELETE, windows.OPEN_EXISTING,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	windows.CloseHandle(handle) //nolint:errcheck
	if err := deleteNativePackageFile(path); err != nil &&
		!errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return err
	}
	return nil
}

func stageNativeBrokerJournalRecordStream(
	path string,
	contents []byte,
	cutpoint func(string) error,
) (resultErr error) {
	if len(contents) == 0 || len(contents) > nativeBrokerJournalMaximumRecords*nativeBrokerJournalMaximumLine {
		return errors.New("native broker journal staged record stream has an invalid length")
	}
	handle, err := openNativeBrokerJournalFile(
		path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.CREATE_NEW,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return errors.New("wrap native broker journal staged record stream")
	}
	defer func() {
		if closeErr := file.Close(); resultErr == nil && closeErr != nil {
			resultErr = closeErr
		}
	}()
	partial := len(contents) / 2
	if partial == 0 {
		partial = len(contents)
	}
	if written, err := file.Write(contents[:partial]); err != nil || written != partial {
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	if cutpoint != nil {
		if err := cutpoint(nativeBrokerCutRecordPartialWrite); err != nil {
			return err
		}
	}
	if partial < len(contents) {
		if written, err := file.Write(contents[partial:]); err != nil || written != len(contents)-partial {
			if err == nil {
				err = io.ErrShortWrite
			}
			return err
		}
	}
	if cutpoint != nil {
		if err := cutpoint(nativeBrokerCutRecordWriteDone); err != nil {
			return err
		}
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if cutpoint != nil {
		if err := cutpoint(nativeBrokerCutRecordSyncDone); err != nil {
			return err
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	readback := make([]byte, len(contents))
	if _, err := io.ReadFull(file, readback); err != nil {
		return err
	}
	if !bytes.Equal(readback, contents) {
		return errors.New("native broker journal record failed write-through readback")
	}
	if cutpoint != nil {
		if err := cutpoint(nativeBrokerCutRecordReadbackDone); err != nil {
			return err
		}
	}
	return nil
}

func executeNativeBrokerJournalRecordPublication(
	line []byte,
	operations nativeBrokerJournalRecordPublicationOperations,
) error {
	if operations.loadCurrent == nil || operations.discardStaging == nil ||
		operations.stage == nil || operations.beforePublish == nil || operations.publish == nil {
		return errors.New("native broker journal record publication operations are incomplete")
	}
	current, err := operations.loadCurrent()
	if err != nil {
		return err
	}
	next, err := buildNativeBrokerJournalRecordStream(current, line)
	if err != nil {
		return err
	}
	if err := operations.discardStaging(); err != nil {
		return err
	}
	if err := operations.stage(next); err != nil {
		return err
	}
	if err := operations.beforePublish(); err != nil {
		return err
	}
	return operations.publish()
}

func appendNativeBrokerJournalRecord(
	path string,
	line []byte,
	cutpoint func(string) error,
) error {
	maximum := nativeBrokerJournalMaximumRecords * nativeBrokerJournalMaximumLine
	staging := path + ".next"
	published := false
	defer func() {
		if !published {
			discardUnpublishedNativeBrokerJournalFile(staging) //nolint:errcheck
		}
	}()
	return executeNativeBrokerJournalRecordPublication(
		line,
		nativeBrokerJournalRecordPublicationOperations{
			loadCurrent: func() ([]byte, error) {
				handle, err := openNativeBrokerJournalFile(
					path, windows.GENERIC_READ, windows.OPEN_EXISTING,
				)
				if err != nil {
					return nil, fmt.Errorf("open native broker journal record stream: %w", err)
				}
				file := os.NewFile(uintptr(handle), path)
				if file == nil {
					windows.CloseHandle(handle) //nolint:errcheck
					return nil, errors.New("wrap native broker journal record stream")
				}
				current, readErr := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
				closeErr := file.Close()
				return current, errors.Join(readErr, closeErr)
			},
			discardStaging: func() error {
				if err := discardUnpublishedNativeBrokerJournalFile(staging); err != nil {
					return fmt.Errorf("discard stale unpublished broker journal record stream: %w", err)
				}
				return nil
			},
			stage: func(next []byte) error {
				return stageNativeBrokerJournalRecordStream(staging, next, cutpoint)
			},
			beforePublish: func() error {
				if cutpoint == nil {
					return nil
				}
				return cutpoint(nativeBrokerCutRecordBeforePublish)
			},
			publish: func() error {
				if err := replaceNativePackageFileAtomically(staging, path, true); err != nil {
					return fmt.Errorf("atomically publish native broker journal record stream: %w", err)
				}
				published = true
				return nil
			},
		},
	)
}

func readNativeBrokerJournalFile(path string, maximum int) ([]byte, error) {
	handle, err := openNativeBrokerJournalFile(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return nil, errors.New("wrap native broker journal artifact")
	}
	defer file.Close() //nolint:errcheck
	contents, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 || len(contents) > maximum {
		return nil, errors.New("native broker journal artifact exceeds its bound")
	}
	return contents, nil
}

func loadNativeBrokerJournal(directory string) (*nativeBrokerJournal, error) {
	snapshotBytes, err := readNativeBrokerJournalFile(
		filepath.Join(directory, nativeBrokerJournalSnapshotName), nativeBrokerJournalMaximumSnapshot,
	)
	if err != nil {
		return nil, fmt.Errorf("read native broker journal snapshot: %w", err)
	}
	var envelope nativeBrokerJournalSnapshotEnvelope
	if err := decodeCanonicalNativeBrokerJSON(snapshotBytes, &envelope, nativeBrokerJournalMaximumSnapshot); err != nil {
		return nil, fmt.Errorf("decode native broker journal snapshot: %w", err)
	}
	payloadBytes, err := nativeBrokerJournalCanonicalJSON(envelope.Payload)
	if err != nil {
		return nil, err
	}
	if envelope.Schema != nativeBrokerJournalSchema || envelope.Payload.Schema != nativeBrokerJournalSchema ||
		!isCanonicalNativeBrokerJournalSHA256(envelope.PayloadSHA256) ||
		envelope.PayloadSHA256 != nativeBrokerJournalHash(payloadBytes) {
		return nil, errors.New("native broker journal snapshot digest or schema is invalid")
	}
	if err := validateNativeBrokerJournalSnapshot(envelope.Payload); err != nil {
		return nil, err
	}
	recordBytes, err := readNativeBrokerJournalFile(
		filepath.Join(directory, nativeBrokerJournalRecordsName),
		nativeBrokerJournalMaximumRecords*nativeBrokerJournalMaximumLine,
	)
	if err != nil {
		return nil, fmt.Errorf("read native broker journal records: %w", err)
	}
	if recordBytes[len(recordBytes)-1] != '\n' {
		return nil, errors.New("native broker journal published record stream has a torn trailing record")
	}
	j := &nativeBrokerJournal{
		directory: directory, snapshot: envelope.Payload, snapshotDigest: envelope.PayloadSHA256,
	}
	scanner := bufio.NewScanner(bytes.NewReader(recordBytes))
	scanner.Buffer(make([]byte, 1024), nativeBrokerJournalMaximumLine)
	for scanner.Scan() {
		if len(j.records) >= nativeBrokerJournalMaximumRecords {
			return nil, errors.New("native broker journal contains too many records")
		}
		line := append([]byte(nil), scanner.Bytes()...)
		var record nativeBrokerJournalRecord
		if err := decodeCanonicalNativeBrokerJSON(line, &record, nativeBrokerJournalMaximumLine); err != nil {
			return nil, fmt.Errorf("decode native broker journal record: %w", err)
		}
		if err := j.validateLoadedRecord(record); err != nil {
			return nil, err
		}
		j.records = append(j.records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(j.records) == 0 || j.records[0].Phase != nativeBrokerPhasePrepared {
		return nil, errors.New("native broker journal has no durable prepared record")
	}
	return j, nil
}

func (j *nativeBrokerJournal) validateLoadedRecord(record nativeBrokerJournalRecord) error {
	if record.Schema != nativeBrokerJournalSchema ||
		record.Sequence != uint32(len(j.records)+1) ||
		record.TransactionID != j.snapshot.TransactionID ||
		record.SnapshotSHA256 != j.snapshotDigest {
		return errors.New("native broker journal record identity is inconsistent")
	}
	previous := strings.Repeat("0", 64)
	if len(j.records) != 0 {
		previous = j.records[len(j.records)-1].RecordSHA256
		if err := validateNativeBrokerJournalTransition(j.records[len(j.records)-1].Phase, record.Phase); err != nil {
			return err
		}
	}
	if record.PreviousSHA256 != previous ||
		(record.DetailSHA256 != "" && !isCanonicalNativeBrokerJournalSHA256(record.DetailSHA256)) {
		return errors.New("native broker journal hash chain is inconsistent")
	}
	if (record.Phase == nativeBrokerPhaseOuterSettlementPending ||
		record.Phase == nativeBrokerPhaseOuterSettled) && record.DetailSHA256 == "" {
		return errors.New("native broker two-phase settlement record is unbound")
	}
	unsigned := nativeBrokerJournalRecordUnsigned{
		Schema: record.Schema, Sequence: record.Sequence, TransactionID: record.TransactionID,
		Phase: record.Phase, PreviousSHA256: record.PreviousSHA256,
		SnapshotSHA256: record.SnapshotSHA256, DetailSHA256: record.DetailSHA256,
	}
	data, err := nativeBrokerJournalCanonicalJSON(unsigned)
	if err != nil {
		return err
	}
	if !isCanonicalNativeBrokerJournalSHA256(record.RecordSHA256) ||
		record.RecordSHA256 != nativeBrokerJournalHash(data) {
		return errors.New("native broker journal record digest is invalid")
	}
	return nil
}

func validateNativeBrokerJournalOuterTokenPath(snapshot nativeBrokerJournalSnapshot) error {
	tokenBase := strings.ToLower(filepath.Base(snapshot.OuterTokenPath))
	if !filepath.IsAbs(snapshot.OuterTokenPath) || strings.IndexByte(snapshot.OuterTokenPath, 0) >= 0 ||
		!strings.EqualFold(filepath.Dir(snapshot.OuterTokenPath), filepath.Dir(snapshot.CandidatePath)) ||
		!strings.HasPrefix(tokenBase, ".viiper.transaction.") ||
		!strings.HasSuffix(tokenBase, ".token") {
		return errors.New("native broker journal outer token path is malformed")
	}
	return nil
}

func validateNativeBrokerJournalSnapshot(snapshot nativeBrokerJournalSnapshot) error {
	if snapshot.Schema != nativeBrokerJournalSchema ||
		!isNativeBrokerJournalTransactionID(snapshot.TransactionID) ||
		!isCanonicalNativeBrokerJournalSHA256(snapshot.OuterTransactionID) ||
		!isCanonicalNativeBrokerJournalSHA256(snapshot.CandidateSHA256) ||
		!filepath.IsAbs(snapshot.CandidatePath) || strings.IndexByte(snapshot.CandidatePath, 0) >= 0 {
		return errors.New("native broker journal snapshot identity is malformed")
	}
	if err := validateNativeBrokerJournalOuterTokenPath(snapshot); err != nil {
		return err
	}
	if _, err := validateNativeInstallingUserSID(snapshot.TargetUserSID); err != nil {
		return fmt.Errorf("validate journal target user SID: %w", err)
	}
	for _, digest := range []string{
		snapshot.PriorCredentialArtifact, snapshot.PriorLegacyArtifact,
	} {
		if !isCanonicalNativeBrokerJournalSHA256(digest) {
			return errors.New("native broker journal artifact digest is malformed")
		}
	}
	if snapshot.PriorCredentialExists && !isCanonicalNativeBrokerJournalSHA256(snapshot.PriorCredentialSHA256) {
		return errors.New("native broker journal prior credential digest is malformed")
	}
	if !snapshot.PriorCredentialExists && snapshot.PriorCredentialSHA256 != "" {
		return errors.New("absent prior credential carried a digest")
	}
	if snapshot.PriorImageExists {
		if !filepath.IsAbs(snapshot.PriorImagePath) ||
			!isCanonicalNativeBrokerJournalSHA256(snapshot.PriorImageSHA256) {
			return errors.New("native broker journal prior image identity is malformed")
		}
	} else if snapshot.PriorImagePath != "" || snapshot.PriorImageSHA256 != "" {
		return errors.New("absent prior image carried identity")
	}
	if snapshot.Service.Exists {
		if strings.TrimSpace(snapshot.Service.Config.BinaryPathName) == "" ||
			strings.IndexByte(snapshot.Service.Config.BinaryPathName, 0) >= 0 ||
			strings.TrimSpace(snapshot.Service.SecurityDescriptor) == "" ||
			snapshot.Service.Config.Password != "" ||
			len(snapshot.Service.Config.BinaryPathName) > 32767 ||
			len(snapshot.Service.SecurityDescriptor) > 64*1024 ||
			len(snapshot.Service.Config.Dependencies) > 64 ||
			len(snapshot.Service.RecoveryActions) > 16 {
			return errors.New("native broker journal prior service snapshot is incomplete")
		}
		for _, value := range append(
			append([]string(nil), snapshot.Service.Config.Dependencies...),
			snapshot.Service.Config.LoadOrderGroup,
			snapshot.Service.Config.ServiceStartName,
			snapshot.Service.Config.DisplayName,
			snapshot.Service.Config.Description,
		) {
			if strings.IndexByte(value, 0) >= 0 || len(value) > 32767 {
				return errors.New("native broker journal prior service string is malformed")
			}
		}
	} else if snapshot.Service.WasRunning || snapshot.Service.Config.BinaryPathName != "" ||
		snapshot.Service.SecurityDescriptor != "" || len(snapshot.Service.RecoveryActions) != 0 ||
		snapshot.Service.RecoveryResetSeconds != 0 || snapshot.Service.RecoverNonCrash {
		return errors.New("absent prior service carried mutable state")
	}
	return nil
}

func protectNativeBrokerJournalData(transactionID, outerID, kind string, plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 || len(plaintext) > nativeBrokerJournalMaximumSecret {
		return nil, errors.New("native broker recovery secret has an invalid length")
	}
	entropy := sha256.Sum256([]byte("VIIPER/native-broker-journal/v1\x00" + transactionID + "\x00" + outerID + "\x00" + kind))
	input := windows.DataBlob{Size: uint32(len(plaintext)), Data: &plaintext[0]}
	entropyBlob := windows.DataBlob{Size: uint32(len(entropy)), Data: &entropy[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(
		&input, nil, &entropyBlob, 0, nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE|windows.CRYPTPROTECT_UI_FORBIDDEN,
		&output,
	); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data))) //nolint:errcheck
	if output.Size == 0 || output.Size > nativeBrokerJournalMaximumSecret || output.Data == nil {
		return nil, errors.New("DPAPI returned an invalid native broker recovery artifact")
	}
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}

func unprotectNativeBrokerJournalData(transactionID, outerID, kind string, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 || len(ciphertext) > nativeBrokerJournalMaximumSecret {
		return nil, errors.New("native broker recovery ciphertext has an invalid length")
	}
	entropy := sha256.Sum256([]byte("VIIPER/native-broker-journal/v1\x00" + transactionID + "\x00" + outerID + "\x00" + kind))
	input := windows.DataBlob{Size: uint32(len(ciphertext)), Data: &ciphertext[0]}
	entropyBlob := windows.DataBlob{Size: uint32(len(entropy)), Data: &entropy[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(
		&input, nil, &entropyBlob, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output,
	); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data))) //nolint:errcheck
	if output.Size == 0 || output.Size > nativeBrokerJournalMaximumSecret || output.Data == nil {
		return nil, errors.New("DPAPI returned invalid native broker recovery plaintext")
	}
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}

func snapshotNativeBrokerCredentialReadOnly(userSID string) ([]byte, bool, error) {
	path, err := nativeServiceKeyFilePath()
	if err != nil {
		return nil, false, err
	}
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return nil, false, err
	}
	programData = filepath.Clean(programData)
	product := filepath.Join(programData, "VIIPER")
	if !strings.EqualFold(filepath.Dir(path), product) {
		return nil, false, errors.New("native broker credential escaped its fixed ProgramData root")
	}
	if _, err := nativePathAttributes(product); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, false, nil
		}
		return nil, false, err
	}
	productHandle, err := openNativePathWithoutReparse(
		product, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, true,
	)
	if err != nil {
		return nil, false, err
	}
	defer windows.CloseHandle(productHandle) //nolint:errcheck
	if err := validateNativeSecurityDescriptor(productHandle, nativeCredentialDirectorySDDL(userSID)); err != nil {
		return nil, false, fmt.Errorf("validate credential directory before recovery snapshot: %w", err)
	}
	if _, err := nativePathAttributes(path); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, false, nil
		}
		return nil, false, err
	}
	handle, err := openNativePathWithoutReparse(path, windows.GENERIC_READ|windows.READ_CONTROL, false)
	if err != nil {
		return nil, false, err
	}
	if err := requireSingleNativeFileLink(handle); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return nil, false, err
	}
	if err := validateNativeSecurityDescriptor(handle, nativeCredentialFileSDDL(userSID)); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return nil, false, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return nil, false, errors.New("wrap prior native broker credential")
	}
	defer file.Close() //nolint:errcheck
	contents, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil {
		return nil, false, err
	}
	if len(contents) == 0 || len(contents) > 64*1024 {
		return nil, false, errors.New("prior native broker credential has an invalid length")
	}
	return contents, true, nil
}

func captureNativeBrokerLegacySnapshot(ctx context.Context, userSID string) (nativeBrokerJournalLegacySnapshot, error) {
	legacy, err := snapshotNativeLegacyStartup(ctx, userSID)
	if err != nil {
		return nativeBrokerJournalLegacySnapshot{}, err
	}
	if legacy.release != nil {
		defer legacy.release()
	}
	for index := range legacy.commands {
		processes, processErr := openLegacyProcessesByExecutable(
			legacy.commands[index].executable, legacy.userSID,
		)
		if processErr != nil {
			return nativeBrokerJournalLegacySnapshot{}, processErr
		}
		legacy.commands[index].running = len(processes) != 0
		for _, process := range processes {
			windows.CloseHandle(process.handle) //nolint:errcheck
		}
	}
	result := nativeBrokerJournalLegacySnapshot{
		Schema: nativeBrokerJournalSchema, UserSID: legacy.userSID,
		RunKeyExisted: legacy.runKeyExisted, ScheduledActive: legacy.scheduledActive,
		ScheduledEnabled: legacy.scheduledEnabled,
	}
	if legacy.runValue != nil {
		value := legacy.runValue.value
		result.RunValueText = &value
		result.RunValueType = legacy.runValue.valueType
	}
	if legacy.scheduledXML != nil {
		value := *legacy.scheduledXML
		result.ScheduledXML = &value
	}
	for _, command := range legacy.commands {
		result.SerializableCmds = append(result.SerializableCmds, nativeBrokerCommand{
			Executable: command.executable, Arguments: append([]string(nil), command.arguments...),
			WorkingDirectory: command.workingDirectory, Source: uint8(command.source),
			WasRunning: command.running,
		})
	}
	if err := validateNativeBrokerLegacySnapshot(result); err != nil {
		return nativeBrokerJournalLegacySnapshot{}, err
	}
	return result, nil
}

func validateNativeBrokerLegacySnapshot(snapshot nativeBrokerJournalLegacySnapshot) error {
	if snapshot.Schema != nativeBrokerJournalSchema {
		return errors.New("native broker legacy snapshot schema is invalid")
	}
	if _, err := validateNativeInstallingUserSID(snapshot.UserSID); err != nil {
		return err
	}
	if snapshot.RunValueText == nil {
		if snapshot.RunValueType != 0 {
			return errors.New("absent legacy Run value carried a type")
		}
	} else if snapshot.RunValueType != registry.SZ && snapshot.RunValueType != registry.EXPAND_SZ {
		return errors.New("legacy Run value type is unsupported")
	}
	if snapshot.ScheduledActive && (!snapshot.ScheduledEnabled || snapshot.ScheduledXML == nil) {
		return errors.New("active legacy task snapshot is not restorable")
	}
	if snapshot.ScheduledXML != nil && strings.TrimSpace(*snapshot.ScheduledXML) == "" {
		return errors.New("legacy task snapshot contains empty XML")
	}
	if len(snapshot.SerializableCmds) > 8 {
		return errors.New("legacy command snapshot exceeds its bound")
	}
	for _, command := range snapshot.SerializableCmds {
		if !filepath.IsAbs(command.Executable) || strings.IndexByte(command.Executable, 0) >= 0 ||
			!strings.EqualFold(filepath.Base(command.Executable), "viiper.exe") ||
			command.Source != uint8(legacyCommandRun) || len(command.Arguments) > 64 {
			return errors.New("legacy command snapshot is malformed")
		}
	}
	return nil
}

func copyNativeBrokerJournalImage(source windows.Handle, destination, expectedHash string) (resultErr error) {
	security, err := nativeSecurityAttributes(nativeBrokerJournalFileSDDL)
	if err != nil {
		return err
	}
	pointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	target, err := windows.CreateFile(
		pointer, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		windows.FILE_SHARE_READ, security, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT|
			windows.FILE_FLAG_WRITE_THROUGH,
		0,
	)
	if err != nil {
		return err
	}
	defer func() {
		windows.CloseHandle(target) //nolint:errcheck
		if resultErr != nil {
			deleteNativePackageFile(destination) //nolint:errcheck
		}
	}()
	if _, err := windows.SetFilePointer(source, 0, nil, windows.FILE_BEGIN); err != nil {
		return err
	}
	var total int64
	buffer := make([]byte, 64*1024)
	for {
		var read uint32
		if err := windows.ReadFile(source, buffer, &read, nil); err != nil {
			return err
		}
		if read == 0 {
			break
		}
		total += int64(read)
		if total > nativeBrokerJournalMaximumImage {
			return errors.New("prior native broker image exceeds the recovery bound")
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
	if err := validateNativeSecurityDescriptor(target, nativeBrokerJournalFileSDDL); err != nil {
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
		return errors.New("prior native broker image changed during durable capture")
	}
	return nil
}

func createEmptyNativeBrokerJournalRecords(path string) error {
	handle, err := openNativeBrokerJournalFile(
		path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.CREATE_NEW,
	)
	if err != nil {
		return err
	}
	if err := windows.FlushFileBuffers(handle); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return err
	}
	return windows.CloseHandle(handle)
}

func beginNativeBrokerJournal(
	ctx context.Context,
	t *windowsNativePackageTransaction,
) (_ *nativeBrokerJournal, resultErr error) {
	if t == nil || !t.nestedBrokerCommit || t.tokenSHA256 == "" ||
		t.boundOuterTokenPath == "" {
		return nil, errors.New("native broker journal requires a bound nested package transaction")
	}
	if t.serviceSnapshot.disposition == nativePackageServiceWeakExactOwned {
		return nil, &nativeBrokerJournalManualError{cause: errors.New(
			"weak service or image ownership is not a trustworthy recovery source",
		)}
	}
	if t.serviceSnapshot.disposition == nativePackageServiceTrusted &&
		!strings.EqualFold(filepath.Clean(t.priorServiceExecutable), filepath.Clean(t.destination)) {
		return nil, &nativeBrokerJournalManualError{cause: errors.New(
			"prior service uses a noncanonical image path that cannot be durably switched",
		)}
	}
	priorCredential, credentialExists, err := snapshotNativeBrokerCredentialReadOnly(t.request.targetUserSID)
	if err != nil {
		return nil, fmt.Errorf("snapshot prior native broker credential: %w", err)
	}
	legacy, err := captureNativeBrokerLegacySnapshot(ctx, t.request.targetUserSID)
	if err != nil {
		return nil, fmt.Errorf("snapshot prior legacy ownership for recovery: %w", err)
	}
	var transactionBytes [16]byte
	if _, err := io.ReadFull(rand.Reader, transactionBytes[:]); err != nil {
		return nil, err
	}
	transactionID := hex.EncodeToString(transactionBytes[:])

	credentialPlain, err := nativeBrokerJournalCanonicalJSON(nativeBrokerJournalCredentialSnapshot{
		Schema: nativeBrokerJournalSchema, Exists: credentialExists,
		Bytes: append([]byte(nil), priorCredential...),
	})
	if err != nil {
		return nil, err
	}
	credentialCipher, err := protectNativeBrokerJournalData(
		transactionID, t.tokenSHA256, "prior-key", credentialPlain,
	)
	if err != nil {
		return nil, fmt.Errorf("protect prior native broker credential: %w", err)
	}
	legacyPlain, err := nativeBrokerJournalCanonicalJSON(legacy)
	if err != nil {
		return nil, err
	}
	legacyCipher, err := protectNativeBrokerJournalData(
		transactionID, t.tokenSHA256, "prior-legacy", legacyPlain,
	)
	if err != nil {
		return nil, fmt.Errorf("protect prior legacy recovery state: %w", err)
	}

	priorImageExists := false
	priorImageHash := ""
	var priorImageHandle windows.Handle
	if handle, openErr := openNativePathWithoutReparse(
		t.destination, windows.GENERIC_READ|windows.READ_CONTROL, false,
	); openErr == nil {
		priorImageHandle = handle
		defer windows.CloseHandle(priorImageHandle) //nolint:errcheck
		if err := requireSingleNativeFileLink(handle); err != nil {
			return nil, err
		}
		if err := validateNativeSecurityDescriptor(handle, nativeBrokerExecutableSDDL); err != nil {
			return nil, fmt.Errorf("validate prior broker image before durable capture: %w", err)
		}
		priorImageHash, err = hashNativePackageHandle(handle)
		if err != nil {
			return nil, err
		}
		priorImageExists = true
	} else if !errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) &&
		!errors.Is(openErr, windows.ERROR_PATH_NOT_FOUND) {
		return nil, openErr
	}
	if t.serviceSnapshot.disposition == nativePackageServiceTrusted &&
		(!priorImageExists || !strings.EqualFold(priorImageHash, t.priorExecutableSHA256)) {
		return nil, errors.New("prior service image identity differs from the canonical image snapshot")
	}

	priorImagePath := ""
	if priorImageExists {
		priorImagePath = t.destination
	}
	serviceSnapshot := nativeBrokerJournalService{}
	if t.serviceSnapshot.disposition == nativePackageServiceTrusted {
		serviceSnapshot = nativeBrokerJournalService{
			Exists: true, WasRunning: t.serviceSnapshot.wasRunning,
			Config: t.priorServiceConfig, SecurityDescriptor: t.priorServiceDACL,
			RecoveryActions:      append([]mgr.RecoveryAction(nil), t.priorServiceRecovery...),
			RecoveryResetSeconds: t.priorServiceReset,
			RecoverNonCrash:      t.priorServiceNonCrash,
		}
	}
	credentialHash := ""
	if credentialExists {
		credentialHash = nativeBrokerJournalHash(priorCredential)
	}
	snapshot := nativeBrokerJournalSnapshot{
		Schema: nativeBrokerJournalSchema, TransactionID: transactionID,
		OuterTransactionID: t.tokenSHA256, OuterTokenPath: t.boundOuterTokenPath,
		TargetUserSID: t.request.targetUserSID,
		CandidatePath: t.destination, CandidateSHA256: t.request.expectedBrokerSHA256,
		PriorImageExists: priorImageExists, PriorImagePath: priorImagePath,
		PriorImageSHA256: priorImageHash, PriorCredentialExists: credentialExists,
		PriorCredentialSHA256:   credentialHash,
		PriorCredentialArtifact: nativeBrokerJournalHash(credentialCipher),
		PriorLegacyArtifact:     nativeBrokerJournalHash(legacyCipher), Service: serviceSnapshot,
	}
	if err := validateNativeBrokerJournalSnapshot(snapshot); err != nil {
		return nil, err
	}
	payload, err := nativeBrokerJournalCanonicalJSON(snapshot)
	if err != nil {
		return nil, err
	}
	snapshotDigest := nativeBrokerJournalHash(payload)
	envelope, err := nativeBrokerJournalCanonicalJSON(nativeBrokerJournalSnapshotEnvelope{
		Schema: nativeBrokerJournalSchema, PayloadSHA256: snapshotDigest, Payload: snapshot,
	})
	if err != nil {
		return nil, err
	}

	directory := ""
	j := &nativeBrokerJournal{
		snapshot: snapshot, snapshotDigest: snapshotDigest,
		priorLegacy: &legacy, cutpoint: t.brokerJournalCutpoint,
	}
	cleanup := true
	defer func() {
		if cleanup && directory != "" {
			if cleanupErr := discardNativeBrokerJournalDirectory(directory); cleanupErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("clean incomplete native broker journal: %w", cleanupErr))
			}
		}
	}()
	if err := executeNativeBrokerJournalPreparation(nativeBrokerJournalPreparationOperations{
		createDirectory: func() error {
			var createErr error
			directory, createErr = createNativeBrokerJournalPreparingDirectory(
				t.request.targetUserSID, transactionID,
			)
			j.directory = directory
			return createErr
		},
		writeCredential: func() error {
			return writeNativeBrokerJournalFile(
				filepath.Join(directory, nativeBrokerJournalCredentialName), credentialCipher,
				nativeBrokerJournalMaximumSecret,
			)
		},
		writeLegacy: func() error {
			return writeNativeBrokerJournalFile(
				filepath.Join(directory, nativeBrokerJournalLegacyName), legacyCipher,
				nativeBrokerJournalMaximumSecret,
			)
		},
		writePriorImage: func() error {
			if !priorImageExists {
				return nil
			}
			return copyNativeBrokerJournalImage(
				priorImageHandle, filepath.Join(directory, nativeBrokerJournalPriorImageName),
				priorImageHash,
			)
		},
		writeSnapshot: func() error {
			return writeNativeBrokerJournalFile(
				filepath.Join(directory, nativeBrokerJournalSnapshotName), envelope,
				nativeBrokerJournalMaximumSnapshot,
			)
		},
		createRecordStream: func() error {
			return createEmptyNativeBrokerJournalRecords(
				filepath.Join(directory, nativeBrokerJournalRecordsName),
			)
		},
		writePrepared: func() error {
			return j.appendPhase(nativeBrokerPhasePrepared, "")
		},
		publishActive: func() error {
			return publishNativeBrokerJournalActive(t.request.targetUserSID, j)
		},
		cutpoint: t.brokerJournalCutpoint,
	}); err != nil {
		return nil, err
	}
	cleanup = false
	return j, nil
}

func (j *nativeBrokerJournal) validatePriorCredential(exists bool, contents []byte) error {
	if j == nil {
		return nil
	}
	if exists != j.snapshot.PriorCredentialExists {
		return errors.New("native broker credential existence changed after durable snapshot")
	}
	if exists && !strings.EqualFold(
		nativeBrokerJournalHash(contents), j.snapshot.PriorCredentialSHA256,
	) {
		return errors.New("native broker credential changed after durable snapshot")
	}
	return nil
}

func (j *nativeBrokerJournal) validatePriorOwnership(
	service nativeServiceSnapshot,
	legacy nativeLegacyState,
) error {
	if j == nil {
		return nil
	}
	expected := j.snapshot.Service
	if service.exists != expected.Exists {
		return errors.New("native broker service existence changed after durable snapshot")
	}
	if service.exists {
		expectedOperational := expected.WasRunning
		if nativeBrokerJournalPhaseIndex(nativeBrokerForwardPhaseOrder, j.lastPhase()) >=
			nativeBrokerJournalPhaseIndex(nativeBrokerForwardPhaseOrder, nativeBrokerPhaseServiceStopIntent) {
			expectedOperational = false
		}
		if serviceWasOperational(service.status.State) != expectedOperational ||
			!nativeServiceConfigsEqual(service.config, expected.Config) ||
			compareNativeSecurityDescriptorStrings(
				service.securityDescriptor, expected.SecurityDescriptor,
			) != nil || !slices.Equal(service.recoveryActions, expected.RecoveryActions) ||
			service.recoveryResetSeconds != expected.RecoveryResetSeconds ||
			service.recoverNonCrash != expected.RecoverNonCrash {
			return errors.New("native broker service changed after durable snapshot")
		}
	}
	if j.priorLegacy == nil {
		return errors.New("native broker journal lost its retained prior legacy snapshot")
	}
	prior := j.priorLegacy
	if !strings.EqualFold(legacy.userSID, prior.UserSID) ||
		legacy.runKeyExisted != prior.RunKeyExisted ||
		legacy.scheduledActive != prior.ScheduledActive ||
		legacy.scheduledEnabled != prior.ScheduledEnabled {
		return errors.New("legacy startup ownership changed after durable snapshot")
	}
	if (legacy.runValue == nil) != (prior.RunValueText == nil) {
		return errors.New("legacy Run registration changed after durable snapshot")
	}
	if legacy.runValue != nil && (legacy.runValue.value != *prior.RunValueText ||
		legacy.runValue.valueType != prior.RunValueType) {
		return errors.New("legacy Run registration changed after durable snapshot")
	}
	if (legacy.scheduledXML == nil) != (prior.ScheduledXML == nil) ||
		(legacy.scheduledXML != nil && *legacy.scheduledXML != *prior.ScheduledXML) {
		return errors.New("legacy scheduled task changed after durable snapshot")
	}
	if len(legacy.commands) != len(prior.SerializableCmds) {
		return errors.New("legacy startup command set changed after durable snapshot")
	}
	for index := range legacy.commands {
		command := prior.SerializableCmds[index]
		if !nativeLegacyCommandsEqual(legacy.commands[index], nativeLegacyCommand{
			executable: command.Executable, arguments: command.Arguments,
			workingDirectory: command.WorkingDirectory, source: nativeLegacyCommandSource(command.Source),
		}) {
			return errors.New("legacy startup command changed after durable snapshot")
		}
	}
	return nil
}

func isNativeBrokerJournalInactiveDirectoryName(name, prefix string) bool {
	return strings.HasPrefix(name, prefix) &&
		isNativeBrokerJournalTransactionID(strings.TrimPrefix(name, prefix))
}

func discardNativeBrokerJournalDirectory(directory string) error {
	name := filepath.Base(filepath.Clean(directory))
	if name != nativeBrokerJournalActiveName &&
		!isNativeBrokerJournalInactiveDirectoryName(name, nativeBrokerJournalPreparingPrefix) &&
		!isNativeBrokerJournalInactiveDirectoryName(name, nativeBrokerJournalSettledPrefix) {
		return errors.New("refusing to discard an unrecognized native broker journal directory")
	}
	handle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(directory, false)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	windows.CloseHandle(handle) //nolint:errcheck
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		nativeBrokerJournalSnapshotName: true, nativeBrokerJournalRecordsName: true,
		nativeBrokerJournalCredentialName: true, nativeBrokerJournalLegacyName: true,
		nativeBrokerJournalPriorImageName:               true,
		nativeBrokerJournalSettlementName:               true,
		nativeBrokerJournalSettledReceiptName:           true,
		nativeBrokerJournalSnapshotName + ".next":       true,
		nativeBrokerJournalRecordsName + ".next":        true,
		nativeBrokerJournalCredentialName + ".next":     true,
		nativeBrokerJournalLegacyName + ".next":         true,
		nativeBrokerJournalPriorImageName + ".next":     true,
		nativeBrokerJournalSettlementName + ".next":     true,
		nativeBrokerJournalSettledReceiptName + ".next": true,
	}
	for _, entry := range entries {
		if entry.IsDir() || !allowed[entry.Name()] {
			return &nativeBrokerJournalManualError{cause: fmt.Errorf(
				"protected broker journal contains unexpected artifact %q", entry.Name(),
			)}
		}
	}
	var cleanupErrors []error
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		file, openErr := openNativeBrokerJournalFile(path, windows.DELETE|windows.READ_CONTROL, windows.OPEN_EXISTING)
		if openErr != nil {
			cleanupErrors = append(cleanupErrors, openErr)
			continue
		}
		windows.CloseHandle(file) //nolint:errcheck
		if deleteErr := deleteNativePackageFile(path); deleteErr != nil &&
			!errors.Is(deleteErr, windows.ERROR_FILE_NOT_FOUND) {
			cleanupErrors = append(cleanupErrors, deleteErr)
		}
	}
	if len(cleanupErrors) != 0 {
		return errors.Join(cleanupErrors...)
	}
	pointer, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return err
	}
	if err := windows.RemoveDirectory(pointer); err != nil &&
		!errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return err
	}
	return nil
}

func executeNativeBrokerJournalRetirement(operations nativeBrokerJournalRetirementOperations) error {
	if operations.rename == nil || operations.proveActiveAbsent == nil ||
		operations.proveTombstone == nil || operations.discardTombstone == nil {
		return errors.New("native broker journal retirement operations are incomplete")
	}
	cut := func(name string) error {
		if operations.cutpoint == nil {
			return nil
		}
		return operations.cutpoint(name)
	}
	if err := cut("retire-before-rename"); err != nil {
		return err
	}
	if err := operations.rename(); err != nil {
		return err
	}
	if err := cut("retire-after-rename"); err != nil {
		return err
	}
	if err := operations.proveActiveAbsent(); err != nil {
		return err
	}
	if err := cut("retire-active-absence-proven"); err != nil {
		return err
	}
	if err := operations.proveTombstone(); err != nil {
		return err
	}
	if err := cut("retire-tombstone-proven"); err != nil {
		return err
	}
	// Admission no longer observes this transaction. Deletion is deliberately
	// non-authoritative and may be retried by inactive-directory discovery.
	_ = operations.discardTombstone()
	return nil
}

func retireNativeBrokerJournal(j *nativeBrokerJournal) error {
	if j == nil || (j.lastPhase() != nativeBrokerPhaseOuterSettled &&
		j.lastPhase() != nativeBrokerPhaseRollbackSettled) {
		return errors.New("native broker journal cannot retire before exact terminal settlement")
	}
	active := filepath.Clean(j.directory)
	root := filepath.Dir(active)
	if filepath.Base(active) != nativeBrokerJournalActiveName ||
		filepath.Base(root) != nativeBrokerJournalRootName {
		return errors.New("native broker active directory identity changed before retirement")
	}
	tombstone, err := nativeBrokerJournalSiblingPath(
		root, nativeBrokerJournalSettledPrefix, j.snapshot.TransactionID,
	)
	if err != nil {
		return err
	}
	return executeNativeBrokerJournalRetirement(nativeBrokerJournalRetirementOperations{
		cutpoint: j.cutpoint,
		rename: func() error {
			if err := moveNativePackageFile(active, tombstone, false); err != nil {
				return fmt.Errorf("atomically retire native broker active journal: %w", err)
			}
			j.directory = tombstone
			return nil
		},
		proveActiveAbsent: func() error {
			if _, err := nativePathAttributes(active); err == nil {
				return errors.New("native broker active journal still exists after terminal retirement")
			} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
				!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return err
			}
			return nil
		},
		proveTombstone: func() error {
			handle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(tombstone, false)
			if err != nil {
				return fmt.Errorf("validate settled native broker journal tombstone: %w", err)
			}
			return windows.CloseHandle(handle)
		},
		discardTombstone: func() error {
			return discardNativeBrokerJournalDirectory(tombstone)
		},
	})
}

func reconcileNativeBrokerJournalInactiveDirectories(
	logger *slog.Logger,
	userSID string,
) error {
	root, _, err := nativeBrokerJournalPaths(userSID)
	if err != nil {
		return err
	}
	rootHandle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(root, false)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	windows.CloseHandle(rootHandle) //nolint:errcheck
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == nativeBrokerJournalActiveName {
			if !entry.IsDir() {
				return &nativeBrokerJournalManualError{cause: errors.New(
					"native broker active journal is not a protected directory",
				)}
			}
			continue
		}
		preparing := isNativeBrokerJournalInactiveDirectoryName(
			name, nativeBrokerJournalPreparingPrefix,
		)
		settled := isNativeBrokerJournalInactiveDirectoryName(
			name, nativeBrokerJournalSettledPrefix,
		)
		if (!preparing && !settled) || !entry.IsDir() {
			return &nativeBrokerJournalManualError{cause: fmt.Errorf(
				"native broker journal root contains unknown transaction artifact %q", name,
			)}
		}
		path := filepath.Join(root, name)
		handle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(path, false)
		if err != nil {
			return &nativeBrokerJournalManualError{cause: err}
		}
		windows.CloseHandle(handle) //nolint:errcheck
		if err := discardNativeBrokerJournalDirectory(path); err != nil {
			if preparing {
				return &nativeBrokerJournalManualError{cause: fmt.Errorf(
					"discard incomplete unpublished broker preparation: %w", err,
				)}
			}
			logger.Warn("Retaining protected settled broker journal tombstone",
				"transactionDirectory", name, "error", err)
		}
	}
	return nil
}

func nativeBrokerJournalPathHash(path string) (string, bool, error) {
	handle, err := openNativePathWithoutReparse(
		path, windows.GENERIC_READ|windows.READ_CONTROL, false,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return "", false, nil
		}
		return "", false, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	if err := requireSingleNativeFileLink(handle); err != nil {
		return "", false, err
	}
	if err := validateNativeSecurityDescriptor(handle, nativeBrokerExecutableSDDL); err != nil {
		return "", false, err
	}
	hash, err := hashNativePackageHandle(handle)
	return hash, true, err
}

func restoreNativeBrokerJournalImage(j *nativeBrokerJournal) error {
	if j == nil {
		return errors.New("native broker image recovery has no journal")
	}
	snapshot := j.snapshot
	transactionStaging := filepath.Join(
		filepath.Dir(snapshot.CandidatePath),
		".viiper.staging."+snapshot.TransactionID+".tmp",
	)
	if stagingHash, stagingExists, err := nativeBrokerJournalPathHash(transactionStaging); err != nil {
		return err
	} else if stagingExists {
		if !strings.EqualFold(stagingHash, snapshot.CandidateSHA256) {
			return &nativeBrokerJournalManualError{cause: errors.New(
				"broker staging artifact differs from the transaction candidate identity",
			)}
		}
		if err := deleteNativePackageFile(transactionStaging); err != nil {
			return err
		}
	}
	recoveryStaging := filepath.Join(
		filepath.Dir(snapshot.CandidatePath),
		".viiper.recovery."+snapshot.TransactionID+".tmp",
	)
	if _, stagingExists, err := nativeBrokerJournalPathHash(recoveryStaging); err != nil {
		return err
	} else if stagingExists {
		if err := deleteNativePackageFile(recoveryStaging); err != nil {
			return err
		}
	}
	currentHash, currentExists, err := nativeBrokerJournalPathHash(snapshot.CandidatePath)
	if err != nil {
		return err
	}
	if snapshot.PriorImageExists && currentExists &&
		strings.EqualFold(currentHash, snapshot.PriorImageSHA256) {
		return nil
	}
	if currentExists && !strings.EqualFold(currentHash, snapshot.CandidateSHA256) {
		return &nativeBrokerJournalManualError{cause: errors.New(
			"canonical broker image differs from both durable prior and candidate identities",
		)}
	}
	if !snapshot.PriorImageExists {
		if !currentExists {
			return nil
		}
		return deleteNativePackageFile(snapshot.CandidatePath)
	}
	artifact := filepath.Join(j.directory, nativeBrokerJournalPriorImageName)
	artifactHandle, err := openNativeBrokerJournalFile(
		artifact, windows.GENERIC_READ, windows.OPEN_EXISTING,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(artifactHandle) //nolint:errcheck
	artifactHash, err := hashNativePackageHandle(artifactHandle)
	if err != nil {
		return err
	}
	if !strings.EqualFold(artifactHash, snapshot.PriorImageSHA256) {
		return &nativeBrokerJournalManualError{cause: errors.New(
			"protected prior broker image artifact failed identity validation",
		)}
	}
	staging := recoveryStaging
	if err := copyNativePackageHandleAtomically(artifactHandle, staging, artifactHash); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			deleteNativePackageFile(staging) //nolint:errcheck
		}
	}()
	if err := replaceNativePackageFileAtomically(staging, snapshot.CandidatePath, currentExists); err != nil {
		return err
	}
	cleanup = false
	restoredHash, restoredExists, err := nativeBrokerJournalPathHash(snapshot.CandidatePath)
	if err != nil {
		return err
	}
	if !restoredExists || !strings.EqualFold(restoredHash, snapshot.PriorImageSHA256) {
		return errors.New("restored prior broker image did not verify")
	}
	return nil
}

func (j *nativeBrokerJournal) loadProtectedArtifacts() (
	nativeBrokerJournalCredentialSnapshot,
	nativeBrokerJournalLegacySnapshot,
	error,
) {
	credentialCipher, err := readNativeBrokerJournalFile(
		filepath.Join(j.directory, nativeBrokerJournalCredentialName),
		nativeBrokerJournalMaximumSecret,
	)
	if err != nil {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{}, err
	}
	if !strings.EqualFold(
		nativeBrokerJournalHash(credentialCipher), j.snapshot.PriorCredentialArtifact,
	) {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{},
			errors.New("protected prior credential artifact digest is invalid")
	}
	credentialPlain, err := unprotectNativeBrokerJournalData(
		j.snapshot.TransactionID, j.snapshot.OuterTransactionID, "prior-key", credentialCipher,
	)
	if err != nil {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{}, err
	}
	var credential nativeBrokerJournalCredentialSnapshot
	if err := decodeCanonicalNativeBrokerJSON(
		credentialPlain, &credential, nativeBrokerJournalMaximumSecret,
	); err != nil {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{}, err
	}
	if credential.Schema != nativeBrokerJournalSchema ||
		credential.Exists != j.snapshot.PriorCredentialExists ||
		(credential.Exists && (!strings.EqualFold(
			nativeBrokerJournalHash(credential.Bytes), j.snapshot.PriorCredentialSHA256,
		) || len(credential.Bytes) == 0)) ||
		(!credential.Exists && len(credential.Bytes) != 0) {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{},
			errors.New("protected prior credential plaintext does not match the journal snapshot")
	}

	legacyCipher, err := readNativeBrokerJournalFile(
		filepath.Join(j.directory, nativeBrokerJournalLegacyName),
		nativeBrokerJournalMaximumSecret,
	)
	if err != nil {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{}, err
	}
	if !strings.EqualFold(nativeBrokerJournalHash(legacyCipher), j.snapshot.PriorLegacyArtifact) {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{},
			errors.New("protected prior legacy artifact digest is invalid")
	}
	legacyPlain, err := unprotectNativeBrokerJournalData(
		j.snapshot.TransactionID, j.snapshot.OuterTransactionID, "prior-legacy", legacyCipher,
	)
	if err != nil {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{}, err
	}
	var legacy nativeBrokerJournalLegacySnapshot
	if err := decodeCanonicalNativeBrokerJSON(
		legacyPlain, &legacy, nativeBrokerJournalMaximumSecret,
	); err != nil {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{}, err
	}
	if err := validateNativeBrokerLegacySnapshot(legacy); err != nil {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{}, err
	}
	if !strings.EqualFold(legacy.UserSID, j.snapshot.TargetUserSID) {
		return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{},
			errors.New("protected legacy artifact target SID differs from the journal")
	}
	j.priorLegacy = &legacy
	if j.snapshot.PriorImageExists {
		artifact, err := openNativeBrokerJournalFile(
			filepath.Join(j.directory, nativeBrokerJournalPriorImageName),
			windows.GENERIC_READ, windows.OPEN_EXISTING,
		)
		if err != nil {
			return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{}, err
		}
		artifactHash, hashErr := hashNativePackageHandle(artifact)
		closeErr := windows.CloseHandle(artifact)
		if hashErr != nil || closeErr != nil ||
			!strings.EqualFold(artifactHash, j.snapshot.PriorImageSHA256) {
			return nativeBrokerJournalCredentialSnapshot{}, nativeBrokerJournalLegacySnapshot{},
				errors.Join(hashErr, closeErr, errors.New("protected prior image artifact digest is invalid"))
		}
	}
	return credential, legacy, nil
}

func currentNativeBrokerJournalCandidateCredentialDigest(j *nativeBrokerJournal) string {
	if j == nil {
		return ""
	}
	for index := len(j.records) - 1; index >= 0; index-- {
		switch j.records[index].Phase {
		case nativeBrokerPhaseCredentialWriteIntent, nativeBrokerPhaseCredentialWritten:
			return j.records[index].DetailSHA256
		}
	}
	return ""
}

func restoreNativeBrokerJournalCredential(
	j *nativeBrokerJournal,
	prior nativeBrokerJournalCredentialSnapshot,
) error {
	current, exists, err := snapshotNativeBrokerCredentialReadOnly(j.snapshot.TargetUserSID)
	if err != nil {
		return err
	}
	currentDigest := ""
	if exists {
		currentDigest = nativeBrokerJournalHash(current)
	}
	if exists == prior.Exists && (!exists || strings.EqualFold(
		currentDigest, j.snapshot.PriorCredentialSHA256,
	)) {
		return nil
	}
	candidateDigest := currentNativeBrokerJournalCandidateCredentialDigest(j)
	if candidateDigest == "" || !exists || !strings.EqualFold(currentDigest, candidateDigest) {
		return &nativeBrokerJournalManualError{cause: errors.New(
			"native broker credential differs from durable prior and candidate identities",
		)}
	}
	path, err := nativeServiceKeyFilePath()
	if err != nil {
		return err
	}
	if prior.Exists {
		if err := writeNativeCredentialAtomically(path, prior.Bytes, j.snapshot.TargetUserSID); err != nil {
			return err
		}
	} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	verified, verifiedExists, err := snapshotNativeBrokerCredentialReadOnly(j.snapshot.TargetUserSID)
	if err != nil {
		return err
	}
	if verifiedExists != prior.Exists || (verifiedExists && !strings.EqualFold(
		nativeBrokerJournalHash(verified), j.snapshot.PriorCredentialSHA256,
	)) {
		return errors.New("prior native broker credential did not verify after durable restoration")
	}
	return nil
}

func exactNativeBrokerJournalServiceState(service nativeManagedService) (
	mgr.Config,
	string,
	[]mgr.RecoveryAction,
	uint32,
	bool,
	svc.Status,
	error,
) {
	config, err := service.Config()
	if err != nil {
		return mgr.Config{}, "", nil, 0, false, svc.Status{}, err
	}
	dacl, err := service.SecurityDescriptor()
	if err != nil {
		return mgr.Config{}, "", nil, 0, false, svc.Status{}, err
	}
	recovery, err := service.RecoveryActions()
	if err != nil {
		return mgr.Config{}, "", nil, 0, false, svc.Status{}, err
	}
	reset, err := service.ResetPeriod()
	if err != nil {
		return mgr.Config{}, "", nil, 0, false, svc.Status{}, err
	}
	nonCrash, err := service.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return mgr.Config{}, "", nil, 0, false, svc.Status{}, err
	}
	status, err := service.Query()
	return config, dacl, recovery, reset, nonCrash, status, err
}

func restoreNativeBrokerJournalService(
	ctx context.Context,
	j *nativeBrokerJournal,
) (nativeManagedService, nativeSCM, error) {
	managerRaw, err := mgr.Connect()
	if err != nil {
		return nil, nil, err
	}
	manager := &windowsNativeSCM{manager: managerRaw}
	service, err := manager.OpenService(NativeBrokerServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		if j.snapshot.Service.Exists {
			manager.Close() //nolint:errcheck
			return nil, nil, &nativeBrokerJournalManualError{cause: errors.New(
				"prior native broker service disappeared during recovery",
			)}
		}
		return nil, manager, nil
	}
	if err != nil {
		manager.Close() //nolint:errcheck
		return nil, nil, err
	}
	config, dacl, recovery, reset, nonCrash, status, err :=
		exactNativeBrokerJournalServiceState(service)
	if err != nil {
		service.Close() //nolint:errcheck
		manager.Close() //nolint:errcheck
		return nil, nil, err
	}
	executable, err := nativeServiceExecutableFromCommandLine(config.BinaryPathName)
	if err != nil || !strings.EqualFold(filepath.Clean(executable), filepath.Clean(j.snapshot.CandidatePath)) ||
		!isLocalSystemServiceAccount(config.ServiceStartName) ||
		compareNativeSecurityDescriptorStrings(dacl, nativeBrokerServiceSDDL) != nil {
		service.Close() //nolint:errcheck
		manager.Close() //nolint:errcheck
		return nil, nil, &nativeBrokerJournalManualError{cause: errors.New(
			"current service is not an exact transaction-owned broker service",
		)}
	}
	if status.State != svc.Stopped {
		if err := stopNativeService(ctx, service, waitContext); err != nil {
			service.Close() //nolint:errcheck
			manager.Close() //nolint:errcheck
			return nil, nil, err
		}
	}
	prior := j.snapshot.Service
	if !prior.Exists {
		candidateConfig, _, err := nativeBrokerServiceConfiguration(
			j.snapshot.CandidatePath, mustNativeBrokerJournalCredentialPath(),
		)
		if err != nil || !nativeServiceConfigsEqual(config, candidateConfig) ||
			!slices.Equal(recovery, nativeServiceRecoveryActions) ||
			reset != nativeServiceRecoveryResetSecond || !nonCrash {
			service.Close() //nolint:errcheck
			manager.Close() //nolint:errcheck
			return nil, nil, &nativeBrokerJournalManualError{cause: errors.New(
				"new broker service is only partially configured; exact deletion is unsafe",
			)}
		}
		if err := service.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			service.Close() //nolint:errcheck
			manager.Close() //nolint:errcheck
			return nil, nil, err
		}
		service.Close() //nolint:errcheck
		if err := waitForNativePackageServiceDeletion(ctx, manager); err != nil {
			manager.Close() //nolint:errcheck
			return nil, nil, err
		}
		return nil, manager, nil
	}
	if !nativeServiceConfigsEqual(config, prior.Config) {
		service.Close() //nolint:errcheck
		manager.Close() //nolint:errcheck
		return nil, nil, &nativeBrokerJournalManualError{cause: errors.New(
			"existing broker service configuration is outside the durable prior identity",
		)}
	}
	if err := service.UpdateConfig(prior.Config); err != nil {
		service.Close() //nolint:errcheck
		manager.Close() //nolint:errcheck
		return nil, nil, err
	}
	if err := service.SetSecurityDescriptor(prior.SecurityDescriptor); err != nil {
		service.Close() //nolint:errcheck
		manager.Close() //nolint:errcheck
		return nil, nil, err
	}
	if err := service.SetRecoveryActionsExact(
		prior.RecoveryActions, prior.RecoveryResetSeconds,
	); err != nil {
		service.Close() //nolint:errcheck
		manager.Close() //nolint:errcheck
		return nil, nil, err
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(prior.RecoverNonCrash); err != nil {
		service.Close() //nolint:errcheck
		manager.Close() //nolint:errcheck
		return nil, nil, err
	}
	verifiedConfig, verifiedDACL, verifiedRecovery, verifiedReset, verifiedNonCrash, verifiedStatus, err :=
		exactNativeBrokerJournalServiceState(service)
	if err != nil || !nativeServiceConfigsEqual(verifiedConfig, prior.Config) ||
		compareNativeSecurityDescriptorStrings(verifiedDACL, prior.SecurityDescriptor) != nil ||
		!slices.Equal(verifiedRecovery, prior.RecoveryActions) ||
		verifiedReset != prior.RecoveryResetSeconds || verifiedNonCrash != prior.RecoverNonCrash ||
		verifiedStatus.State != svc.Stopped {
		service.Close() //nolint:errcheck
		manager.Close() //nolint:errcheck
		return nil, nil, errors.Join(err, errors.New(
			"prior native broker service did not verify after durable restoration",
		))
	}
	return service, manager, nil
}

func mustNativeBrokerJournalCredentialPath() string {
	path, _ := nativeServiceKeyFilePath()
	return path
}

func restoreNativeBrokerJournalLegacy(
	ctx context.Context,
	j *nativeBrokerJournal,
	prior nativeBrokerJournalLegacySnapshot,
) error {
	hive, err := registry.OpenKey(registry.USERS, prior.UserSID, registry.READ)
	if err != nil {
		return fmt.Errorf("open target user hive for broker recovery: %w", err)
	}
	defer hive.Close() //nolint:errcheck
	runKey, err := registry.OpenKey(hive, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		if prior.RunValueText != nil || prior.RunKeyExisted {
			return &nativeBrokerJournalManualError{cause: errors.New(
				"prior target-user Run key disappeared during broker recovery",
			)}
		}
	} else if err != nil {
		return err
	}
	if runKey != 0 {
		defer runKey.Close() //nolint:errcheck
		current, found, err := readNativeRunRegistration(runKey)
		if err != nil {
			return err
		}
		if prior.RunValueText == nil {
			if found {
				return &nativeBrokerJournalManualError{cause: errors.New(
					"legacy Run registration appeared during broker recovery",
				)}
			}
		} else {
			expected := nativeRunRegistration{value: *prior.RunValueText, valueType: prior.RunValueType}
			if found && !nativeRunRegistrationsEqual(current, expected) {
				return &nativeBrokerJournalManualError{cause: errors.New(
					"legacy Run registration changed outside the broker transaction",
				)}
			}
			if !found {
				if err := setNativeRunRegistration(runKey, expected); err != nil {
					return err
				}
			}
		}
	}
	_, currentXML, currentActive, _, found, err := currentScheduledTaskCommand(ctx)
	if err != nil {
		return err
	}
	if prior.ScheduledXML == nil {
		if found {
			return &nativeBrokerJournalManualError{cause: errors.New(
				"legacy scheduled task appeared during broker recovery",
			)}
		}
	} else {
		if !found {
			return &nativeBrokerJournalManualError{cause: errors.New(
				"legacy scheduled task disappeared during broker recovery",
			)}
		}
		if currentXML != *prior.ScheduledXML {
			if err := validateNativeTaskDisabledOnly(*prior.ScheduledXML, currentXML); err != nil {
				return &nativeBrokerJournalManualError{cause: err}
			}
			if err := restoreNativeScheduledTask(ctx, *prior.ScheduledXML, currentXML); err != nil {
				return err
			}
		}
		if prior.ScheduledActive && !currentActive {
			if err := startNativeScheduledTask(ctx, *prior.ScheduledXML); err != nil {
				return err
			}
		}
	}
	for _, command := range prior.SerializableCmds {
		if !command.WasRunning || command.Source != uint8(legacyCommandRun) {
			continue
		}
		processes, err := openLegacyProcessesByExecutable(command.Executable, prior.UserSID)
		if err != nil {
			return err
		}
		alreadyRunning := len(processes) != 0
		for _, process := range processes {
			windows.CloseHandle(process.handle) //nolint:errcheck
		}
		if alreadyRunning {
			continue
		}
		verify, release, err := lockNativeLegacyTaskExecutable(command.Executable)
		if err != nil {
			return err
		}
		if err := verify(); err != nil {
			release()
			return err
		}
		err = startNativeLegacyCommandAsShellUser(nativeLegacyCommand{
			executable: command.Executable, arguments: command.Arguments,
			workingDirectory: command.WorkingDirectory, source: legacyCommandRun,
		}, prior.UserSID)
		release()
		if err != nil {
			return err
		}
	}
	return nil
}

func rollbackNativeBrokerJournal(ctx context.Context, j *nativeBrokerJournal) (resultErr error) {
	priorCredential, priorLegacy, err := j.loadProtectedArtifacts()
	if err != nil {
		return &nativeBrokerJournalManualError{cause: err}
	}
	if nativeBrokerJournalPhaseIndex(nativeBrokerForwardPhaseOrder, j.lastPhase()) >= 0 {
		if err := j.appendPhase(nativeBrokerPhaseRollbackIntent, ""); err != nil {
			return err
		}
	}
	service, manager, err := restoreNativeBrokerJournalService(ctx, j)
	if err != nil {
		j.appendPhase(nativeBrokerPhaseManual, "") //nolint:errcheck
		return err
	}
	if manager != nil {
		defer manager.Close() //nolint:errcheck
	}
	if service != nil {
		defer service.Close() //nolint:errcheck
	}
	if err := j.appendPhase(nativeBrokerPhaseRollbackService, ""); err != nil {
		return err
	}
	if err := restoreNativeBrokerJournalCredential(j, priorCredential); err != nil {
		j.appendPhase(nativeBrokerPhaseManual, "") //nolint:errcheck
		return err
	}
	if err := j.appendPhase(nativeBrokerPhaseRollbackCredential, ""); err != nil {
		return err
	}
	if err := restoreNativeBrokerJournalImage(j); err != nil {
		j.appendPhase(nativeBrokerPhaseManual, "") //nolint:errcheck
		return err
	}
	if err := j.appendPhase(nativeBrokerPhaseRollbackImage, ""); err != nil {
		return err
	}
	if err := restoreNativeBrokerJournalLegacy(ctx, j, priorLegacy); err != nil {
		j.appendPhase(nativeBrokerPhaseManual, "") //nolint:errcheck
		return err
	}
	if err := j.appendPhase(nativeBrokerPhaseRollbackLegacy, ""); err != nil {
		return err
	}
	if service != nil && j.snapshot.Service.WasRunning {
		if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			j.appendPhase(nativeBrokerPhaseManual, "") //nolint:errcheck
			return err
		}
		if err := waitForNativeServiceState(ctx, service, svc.Running, waitContext); err != nil {
			j.appendPhase(nativeBrokerPhaseManual, "") //nolint:errcheck
			return err
		}
	}
	if err := j.appendPhase(nativeBrokerPhaseRollbackSettled, ""); err != nil {
		return err
	}
	return retireNativeBrokerJournal(j)
}

func reconcileNativeBrokerJournalBeforeAdmission(
	ctx context.Context,
	logger *slog.Logger,
	userSID string,
) error {
	_, err := reconcileNativeBrokerJournalBeforeAdmissionInternal(
		ctx, logger, userSID, false,
	)
	return err
}

func reconcileNativeBrokerJournalBeforeOuterPackage(
	ctx context.Context,
	logger *slog.Logger,
	userSID string,
) (bool, error) {
	return reconcileNativeBrokerJournalBeforeAdmissionInternal(
		ctx, logger, userSID, true,
	)
}

func reconcileNativeBrokerJournalBeforeAdmissionInternal(
	ctx context.Context,
	logger *slog.Logger,
	userSID string,
	allowNestedReady bool,
) (bool, error) {
	if err := reconcileNativeBrokerJournalInactiveDirectories(logger, userSID); err != nil {
		return false, err
	}
	_, active, err := nativeBrokerJournalPaths(userSID)
	if err != nil {
		return false, err
	}
	if _, err := nativePathAttributes(active); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return false, nil
		}
		return false, err
	}
	handle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(active, false)
	if err != nil {
		return false, &nativeBrokerJournalManualError{cause: err}
	}
	windows.CloseHandle(handle) //nolint:errcheck
	j, err := loadNativeBrokerJournal(active)
	if err != nil {
		return false, &nativeBrokerJournalManualError{cause: err}
	}
	if !strings.EqualFold(j.snapshot.TargetUserSID, userSID) {
		return false, &nativeBrokerJournalManualError{cause: errors.New(
			"active broker journal belongs to a different target user SID",
		)}
	}
	switch j.lastPhase() {
	case nativeBrokerPhaseRollbackSettled:
		return false, retireNativeBrokerJournal(j)
	case nativeBrokerPhaseNestedReady, nativeBrokerPhaseOuterSettlementPending,
		nativeBrokerPhaseOuterSettled:
		if !allowNestedReady {
			return false, &nativeBrokerJournalManualError{cause: errors.New(
				"broker readiness lacks a completed authoritative outer package settlement",
			)}
		}
		if _, _, err := j.loadProtectedArtifacts(); err != nil {
			return false, &nativeBrokerJournalManualError{cause: err}
		}
		if err := verifyNativeBrokerJournalForwardState(ctx, j); err != nil {
			return false, &nativeBrokerJournalManualError{cause: fmt.Errorf(
				"pending outer broker settlement failed exact forward verification: %w", err,
			)}
		}
		if j.lastPhase() == nativeBrokerPhaseOuterSettled {
			finalPath, err := nativeBrokerOuterSettlementFinalPath(j)
			if err != nil {
				return false, &nativeBrokerJournalManualError{cause: err}
			}
			if _, err := nativePathAttributes(finalPath); err == nil {
				if _, err := loadNativeBrokerOuterSettlementFinalForReconciliation(j); err != nil {
					return false, &nativeBrokerJournalManualError{cause: fmt.Errorf(
						"terminal broker settlement has an invalid protected final receipt: %w", err,
					)}
				}
			} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
				!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return false, &nativeBrokerJournalManualError{cause: fmt.Errorf(
					"inspect terminal broker settlement final receipt: %w", err,
				)}
			}
		}
		return true, nil
	case nativeBrokerPhaseManual:
		return false, &nativeBrokerJournalManualError{cause: errors.New(
			"prior broker recovery is latched manual",
		)}
	default:
		if _, _, err := j.loadProtectedArtifacts(); err != nil {
			return false, &nativeBrokerJournalManualError{cause: err}
		}
		logger.Warn("Recovering an interrupted native broker transaction",
			"transactionId", j.snapshot.TransactionID,
			"phase", j.lastPhase())
		return false, rollbackNativeBrokerJournal(ctx, j)
	}
}

func verifyNativeBrokerJournalForwardState(
	ctx context.Context,
	j *nativeBrokerJournal,
) error {
	imageHash, imageExists, err := nativeBrokerJournalPathHash(j.snapshot.CandidatePath)
	if err != nil {
		return err
	}
	if !imageExists || !strings.EqualFold(imageHash, j.snapshot.CandidateSHA256) {
		return errors.New("outer settlement found a different canonical broker image")
	}
	credential, exists, err := snapshotNativeBrokerCredentialReadOnly(j.snapshot.TargetUserSID)
	if err != nil {
		return err
	}
	candidateCredential := currentNativeBrokerJournalCandidateCredentialDigest(j)
	if !exists || candidateCredential == "" ||
		!strings.EqualFold(nativeBrokerJournalHash(credential), candidateCredential) {
		return errors.New("outer settlement found a different native broker credential")
	}
	managerRaw, err := mgr.Connect()
	if err != nil {
		return err
	}
	manager := &windowsNativeSCM{manager: managerRaw}
	defer manager.Close() //nolint:errcheck
	service, err := manager.OpenService(NativeBrokerServiceName)
	if err != nil {
		return err
	}
	defer service.Close() //nolint:errcheck
	config, dacl, recovery, reset, nonCrash, status, err := exactNativeBrokerJournalServiceState(service)
	if err != nil {
		return err
	}
	credentialPath, err := nativeServiceKeyFilePath()
	if err != nil {
		return err
	}
	expectedConfig, _, err := nativeBrokerServiceConfiguration(
		j.snapshot.CandidatePath, credentialPath,
	)
	if err != nil {
		return err
	}
	if !nativeServiceConfigsEqual(config, expectedConfig) ||
		compareNativeSecurityDescriptorStrings(dacl, nativeBrokerServiceSDDL) != nil ||
		!slices.Equal(recovery, nativeServiceRecoveryActions) ||
		reset != nativeServiceRecoveryResetSecond || !nonCrash || status.State != svc.Running {
		return errors.New("outer settlement found a noncanonical native broker service")
	}
	legacy, err := snapshotNativeLegacyStartup(ctx, j.snapshot.TargetUserSID)
	if err != nil {
		return err
	}
	if legacy.release != nil {
		defer legacy.release()
	}
	if nativeLegacyStartupOwnsRuntime(legacy) {
		return errors.New("outer settlement found active legacy startup ownership")
	}
	return nil
}

func validateNativeBrokerNestedReplayBinding(
	j *nativeBrokerJournal,
	userSID, outerTokenPath, outerTransactionID, candidateSHA256 string,
) error {
	if j == nil || j.lastPhase() != nativeBrokerPhaseNestedReady {
		return errors.New("active broker journal is not at nested-ready")
	}
	if !strings.EqualFold(j.snapshot.TargetUserSID, userSID) ||
		!strings.EqualFold(filepath.Clean(j.snapshot.OuterTokenPath), filepath.Clean(outerTokenPath)) ||
		!strings.EqualFold(j.snapshot.OuterTransactionID, outerTransactionID) ||
		!strings.EqualFold(j.snapshot.CandidateSHA256, candidateSHA256) {
		return errors.New("active nested broker journal does not match the exact outer token, candidate, and target user")
	}
	proof := j.proof()
	if proof.TransactionID != j.snapshot.TransactionID ||
		proof.State != string(nativeBrokerPhaseNestedReady) ||
		!isCanonicalNativeBrokerJournalSHA256(proof.Digest) {
		return errors.New("durable nested-ready proof is not canonical")
	}
	return nil
}

func replayNativeBrokerNestedReadyProof(
	ctx context.Context,
	logger *slog.Logger,
	userSID, outerTokenPath, outerTransactionID, candidateSHA256 string,
) (nativeBrokerJournalProof, bool, bool, error) {
	if err := reconcileNativeBrokerJournalInactiveDirectories(logger, userSID); err != nil {
		return nativeBrokerJournalProof{}, false, false, err
	}
	_, active, err := nativeBrokerJournalPaths(userSID)
	if err != nil {
		return nativeBrokerJournalProof{}, false, false, err
	}
	if _, err := nativePathAttributes(active); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nativeBrokerJournalProof{}, false, false, nil
		}
		return nativeBrokerJournalProof{}, false, false, err
	}
	handle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(active, false)
	if err != nil {
		return nativeBrokerJournalProof{}, false, true, &nativeBrokerJournalManualError{cause: err}
	}
	windows.CloseHandle(handle) //nolint:errcheck
	j, err := loadNativeBrokerJournal(active)
	if err != nil {
		return nativeBrokerJournalProof{}, false, true, &nativeBrokerJournalManualError{cause: err}
	}
	proof := j.proof()
	if !strings.EqualFold(j.snapshot.TargetUserSID, userSID) ||
		!strings.EqualFold(filepath.Clean(j.snapshot.OuterTokenPath), filepath.Clean(outerTokenPath)) ||
		!strings.EqualFold(j.snapshot.OuterTransactionID, outerTransactionID) ||
		!strings.EqualFold(j.snapshot.CandidateSHA256, candidateSHA256) {
		return proof, false, true, &nativeBrokerJournalManualError{cause: errors.New(
			"active broker journal does not match the exact outer token, candidate, and target user",
		)}
	}
	switch j.lastPhase() {
	case nativeBrokerPhaseManual, nativeBrokerPhaseOuterSettled:
		return proof, false, true, &nativeBrokerJournalManualError{cause: fmt.Errorf(
			"active broker journal phase %s cannot be replayed or rolled back by the child",
			j.lastPhase(),
		)}
	case nativeBrokerPhaseRollbackSettled:
		if err := retireNativeBrokerJournal(j); err != nil {
			return proof, false, true, err
		}
		return proof, false, true, nil
	case nativeBrokerPhaseNestedReady:
		// Exact forward replay is verified below without changing the journal.
	case nativeBrokerPhaseOuterSettlementPending:
		return proof, false, true, errors.New(
			"broker child journal is already pending outer acknowledgement; replay the retained outer binding",
		)
	default:
		if _, _, err := j.loadProtectedArtifacts(); err != nil {
			return proof, false, true, &nativeBrokerJournalManualError{cause: err}
		}
		if err := rollbackNativeBrokerJournal(ctx, j); err != nil {
			return j.proof(), false, true, err
		}
		return j.proof(), false, true, nil
	}
	if err := validateNativeBrokerNestedReplayBinding(
		j, userSID, outerTokenPath, outerTransactionID, candidateSHA256,
	); err != nil {
		return proof, false, true, &nativeBrokerJournalManualError{cause: err}
	}
	if _, _, err := j.loadProtectedArtifacts(); err != nil {
		return proof, false, true, &nativeBrokerJournalManualError{cause: err}
	}
	if err := verifyNativeBrokerJournalForwardState(ctx, j); err != nil {
		return proof, false, true, &nativeBrokerJournalManualError{cause: fmt.Errorf(
			"durable nested-ready state failed exact replay verification: %w", err,
		)}
	}
	return proof, true, true, nil
}

func nativeBrokerJournalOuterSettlementIdentity(
	outerTransactionID, candidateSHA256 string,
	proof nativePackageInstallProof,
) (string, string) {
	if proof.success && proof.journal.TransactionID != "" &&
		(proof.journalRecovery == "fresh" || proof.journalRecovery == "replayed") {
		return proof.journal.OuterTransactionID, proof.journal.CandidateSHA256
	}
	return outerTransactionID, candidateSHA256
}

func discardSettledNativeBrokerOuterToken(j *nativeBrokerJournal) error {
	if j == nil {
		return errors.New("settled broker token cleanup has no journal")
	}
	path := j.snapshot.OuterTokenPath
	handle, err := openNativePathWithoutReparse(
		path, windows.GENERIC_READ|windows.READ_CONTROL|windows.DELETE, false,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	closeWith := func(result error) error {
		return errors.Join(result, windows.CloseHandle(handle))
	}
	if err := requireSingleNativeFileLink(handle); err != nil {
		return closeWith(err)
	}
	if err := validateNativeSecurityDescriptor(handle, nativePackageTokenSDDL); err != nil {
		return closeWith(err)
	}
	hash, err := hashNativePackageHandle(handle)
	if err != nil {
		return closeWith(err)
	}
	if hash != j.snapshot.OuterTransactionID {
		return closeWith(errors.New("retained outer token no longer matches its settled transaction"))
	}
	if err := windows.CloseHandle(handle); err != nil {
		return err
	}
	if err := deleteNativePackageFile(path); err != nil &&
		!errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return err
	}
	return nil
}

func validateNativeBrokerOuterSettlementBinding(
	j *nativeBrokerJournal,
	proof nativePackageInstallProof,
) (nativeBrokerOuterSettlementBinding, string, error) {
	if j == nil || (j.lastPhase() != nativeBrokerPhaseNestedReady &&
		j.lastPhase() != nativeBrokerPhaseOuterSettlementPending &&
		j.lastPhase() != nativeBrokerPhaseOuterSettled) {
		return nativeBrokerOuterSettlementBinding{}, "", errors.New(
			"active broker journal is not eligible for outer settlement",
		)
	}
	nestedIndex := len(j.records) - 1
	if j.lastPhase() == nativeBrokerPhaseOuterSettlementPending {
		nestedIndex--
	} else if j.lastPhase() == nativeBrokerPhaseOuterSettled {
		nestedIndex -= 2
	}
	if nestedIndex < 0 || j.records[nestedIndex].Phase != nativeBrokerPhaseNestedReady {
		return nativeBrokerOuterSettlementBinding{}, "", errors.New(
			"outer settlement lacks the immediately preceding nested-ready record",
		)
	}
	nestedDigest := j.records[nestedIndex].RecordSHA256
	if !proof.success || !proof.changed || proof.exitCode != 0 ||
		(proof.journalRecovery != "fresh" && proof.journalRecovery != "replayed") ||
		proof.journal.TransactionID != j.snapshot.TransactionID ||
		proof.journal.OuterTransactionID != j.snapshot.OuterTransactionID ||
		proof.journal.CandidateSHA256 != j.snapshot.CandidateSHA256 ||
		proof.journal.State != string(nativeBrokerPhaseNestedReady) ||
		proof.journal.Digest != nestedDigest ||
		!isCanonicalNativeBrokerJournalSHA256(proof.driverTransactionID) ||
		!isCanonicalNativeBrokerJournalSHA256(proof.driverPendingDigest) ||
		!isCanonicalNativeBrokerJournalSHA256(proof.settlementNonce) {
		return nativeBrokerOuterSettlementBinding{}, "", errors.New(
			"outer driver proof omitted or mismatched the durable two-phase journal binding",
		)
	}
	binding := nativeBrokerOuterSettlementBinding{
		Schema:                   nativeBrokerJournalSchema,
		BrokerTransactionID:      j.snapshot.TransactionID,
		BrokerOuterTransactionID: j.snapshot.OuterTransactionID,
		BrokerCandidateSHA256:    j.snapshot.CandidateSHA256,
		BrokerNestedDigest:       nestedDigest,
		DriverTransactionID:      proof.driverTransactionID,
		DriverPendingDigest:      proof.driverPendingDigest,
		SettlementNonce:          proof.settlementNonce,
	}
	bindingBytes, err := nativeBrokerJournalCanonicalJSON(binding)
	if err != nil {
		return nativeBrokerOuterSettlementBinding{}, "", err
	}
	return binding, nativeBrokerJournalHash(bindingBytes), nil
}

func validateNativeBrokerOuterSettlementRequest(
	j *nativeBrokerJournal,
	request nativeBrokerOuterSettlementRequest,
) error {
	if j == nil || (j.lastPhase() != nativeBrokerPhaseOuterSettlementPending &&
		j.lastPhase() != nativeBrokerPhaseOuterSettled) {
		return errors.New("broker settlement request has no exact pending journal state")
	}
	pendingIndex := len(j.records) - 1
	if j.lastPhase() == nativeBrokerPhaseOuterSettled {
		pendingIndex--
	}
	nestedIndex := pendingIndex - 1
	if nestedIndex < 0 ||
		j.records[pendingIndex].Phase != nativeBrokerPhaseOuterSettlementPending ||
		j.records[nestedIndex].Phase != nativeBrokerPhaseNestedReady {
		return errors.New("broker settlement request has no exact pending journal state")
	}
	binding := request.Binding
	if request.Schema != nativeBrokerJournalSchema || binding.Schema != nativeBrokerJournalSchema ||
		binding.BrokerTransactionID != j.snapshot.TransactionID ||
		binding.BrokerOuterTransactionID != j.snapshot.OuterTransactionID ||
		binding.BrokerCandidateSHA256 != j.snapshot.CandidateSHA256 ||
		binding.BrokerNestedDigest != j.records[nestedIndex].RecordSHA256 ||
		!isCanonicalNativeBrokerJournalSHA256(binding.DriverTransactionID) ||
		!isCanonicalNativeBrokerJournalSHA256(binding.DriverPendingDigest) ||
		!isCanonicalNativeBrokerJournalSHA256(binding.SettlementNonce) ||
		!isCanonicalNativeBrokerJournalSHA256(request.BindingSHA256) ||
		!isCanonicalNativeBrokerJournalSHA256(request.BrokerPendingDigest) ||
		request.BrokerPendingDigest != j.records[pendingIndex].RecordSHA256 ||
		request.BindingSHA256 != j.records[pendingIndex].DetailSHA256 {
		return errors.New("broker settlement request identity does not match its journal chain")
	}
	bindingBytes, err := nativeBrokerJournalCanonicalJSON(binding)
	if err != nil {
		return err
	}
	if request.BindingSHA256 != nativeBrokerJournalHash(bindingBytes) {
		return errors.New("broker settlement request binding digest is invalid")
	}
	return nil
}

func encodeNativeBrokerOuterSettlementEnvelope(
	request nativeBrokerOuterSettlementRequest,
) ([]byte, string, error) {
	payload, err := nativeBrokerJournalCanonicalJSON(request)
	if err != nil {
		return nil, "", err
	}
	envelope := nativeBrokerOuterSettlementEnvelope{
		Schema:        nativeBrokerJournalSchema,
		PayloadSHA256: nativeBrokerJournalHash(payload),
		Payload:       request,
	}
	contents, err := nativeBrokerJournalCanonicalJSON(envelope)
	if err != nil {
		return nil, "", err
	}
	if len(contents) > nativeBrokerJournalMaximumSettlement {
		return nil, "", errors.New("broker settlement envelope exceeds its bound")
	}
	return contents, nativeBrokerJournalHash(contents), nil
}

func decodeNativeBrokerOuterSettlementEnvelope(
	contents []byte,
) (nativeBrokerOuterSettlementEnvelope, error) {
	var envelope nativeBrokerOuterSettlementEnvelope
	if err := decodeCanonicalNativeBrokerJSON(
		contents, &envelope, nativeBrokerJournalMaximumSettlement,
	); err != nil {
		return nativeBrokerOuterSettlementEnvelope{}, err
	}
	payload, err := nativeBrokerJournalCanonicalJSON(envelope.Payload)
	if err != nil {
		return nativeBrokerOuterSettlementEnvelope{}, err
	}
	if envelope.Schema != nativeBrokerJournalSchema ||
		!isCanonicalNativeBrokerJournalSHA256(envelope.PayloadSHA256) ||
		envelope.PayloadSHA256 != nativeBrokerJournalHash(payload) {
		return nativeBrokerOuterSettlementEnvelope{}, errors.New(
			"broker settlement envelope schema or payload digest is invalid",
		)
	}
	return envelope, nil
}

func nativeBrokerOuterSettlementRequestPath(j *nativeBrokerJournal) (string, error) {
	if j == nil {
		return "", errors.New("broker settlement request has no journal")
	}
	_, active, err := nativeBrokerJournalPaths(j.snapshot.TargetUserSID)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(filepath.Clean(j.directory), filepath.Clean(active)) {
		return "", errors.New("broker settlement request escaped the exact active journal")
	}
	path := filepath.Join(active, nativeBrokerJournalSettlementName)
	if !strings.EqualFold(filepath.Dir(path), active) ||
		filepath.Base(path) != nativeBrokerJournalSettlementName {
		return "", errors.New("broker settlement request path escaped its active journal")
	}
	return path, nil
}

func loadNativeBrokerOuterSettlementRequest(
	j *nativeBrokerJournal,
) (nativeBrokerOuterSettlementPrepared, error) {
	path, err := nativeBrokerOuterSettlementRequestPath(j)
	if err != nil {
		return nativeBrokerOuterSettlementPrepared{}, err
	}
	contents, err := readNativeBrokerJournalFile(path, nativeBrokerJournalMaximumSettlement)
	if err != nil {
		return nativeBrokerOuterSettlementPrepared{}, err
	}
	envelope, err := decodeNativeBrokerOuterSettlementEnvelope(contents)
	if err != nil {
		return nativeBrokerOuterSettlementPrepared{}, err
	}
	if err := validateNativeBrokerOuterSettlementRequest(j, envelope.Payload); err != nil {
		return nativeBrokerOuterSettlementPrepared{}, err
	}
	return nativeBrokerOuterSettlementPrepared{
		Request: envelope.Payload, RequestPath: path,
		RequestSHA256: nativeBrokerJournalHash(contents), contents: contents,
	}, nil
}

func publishNativeBrokerOuterSettlementRequest(
	j *nativeBrokerJournal,
	prepared nativeBrokerOuterSettlementPrepared,
) error {
	expectedPath, err := nativeBrokerOuterSettlementRequestPath(j)
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(prepared.RequestPath), filepath.Clean(expectedPath)) ||
		prepared.RequestSHA256 != nativeBrokerJournalHash(prepared.contents) {
		return errors.New("broker settlement publication identity changed")
	}
	if envelope, err := decodeNativeBrokerOuterSettlementEnvelope(prepared.contents); err != nil {
		return err
	} else if envelope.Payload != prepared.Request {
		return errors.New("broker settlement publication payload changed")
	}
	if err := validateNativeBrokerOuterSettlementRequest(j, prepared.Request); err != nil {
		return err
	}
	staging := expectedPath + ".next"
	loadOptional := func(path string) ([]byte, bool, error) {
		if _, err := nativePathAttributes(path); err != nil {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return nil, false, nil
			}
			return nil, false, err
		}
		contents, err := readNativeBrokerJournalFile(path, nativeBrokerJournalMaximumSettlement)
		return contents, true, err
	}
	if err := executeNativeBrokerSettlementPublication(
		prepared.contents,
		nativeBrokerSettlementPublicationOperations{
			loadPublished: func() ([]byte, bool, error) {
				return loadOptional(expectedPath)
			},
			loadStaging: func() ([]byte, bool, error) {
				return loadOptional(staging)
			},
			discardStaging: func() error {
				return discardUnpublishedNativeBrokerJournalFile(staging)
			},
			publishStaging: func() error {
				return moveNativePackageFile(staging, expectedPath, false)
			},
			writeNew: func() error {
				return writeNativeBrokerJournalFile(
					expectedPath, prepared.contents, nativeBrokerJournalMaximumSettlement,
				)
			},
			readback: func() ([]byte, error) {
				return readNativeBrokerJournalFile(
					expectedPath, nativeBrokerJournalMaximumSettlement,
				)
			},
		},
	); err != nil {
		return fmt.Errorf("publish broker settlement request: %w", err)
	}
	loaded, err := loadNativeBrokerOuterSettlementRequest(j)
	if err != nil {
		return fmt.Errorf("read back broker settlement request: %w", err)
	}
	if loaded.Request != prepared.Request || loaded.RequestSHA256 != prepared.RequestSHA256 ||
		!bytes.Equal(loaded.contents, prepared.contents) {
		return errors.New("published broker settlement request differs from its durable receipt")
	}
	return nil
}

func armNativeBrokerOuterSettlement(
	ctx context.Context,
	userSID, outerTransactionID, candidateSHA256 string,
	proof nativePackageInstallProof,
) (*nativeBrokerJournal, nativeBrokerOuterSettlementPrepared, error) {
	_, active, err := nativeBrokerJournalPaths(userSID)
	if err != nil {
		return nil, nativeBrokerOuterSettlementPrepared{}, err
	}
	if _, err := nativePathAttributes(active); err != nil {
		return nil, nativeBrokerOuterSettlementPrepared{}, errors.Join(
			err, errors.New("authoritative outer success expected a durable broker journal"),
		)
	}
	j, err := loadNativeBrokerJournal(active)
	if err != nil {
		return nil, nativeBrokerOuterSettlementPrepared{}, &nativeBrokerJournalManualError{cause: err}
	}
	expectedOuterTransactionID, expectedCandidateSHA256 :=
		nativeBrokerJournalOuterSettlementIdentity(outerTransactionID, candidateSHA256, proof)
	if !strings.EqualFold(j.snapshot.OuterTransactionID, expectedOuterTransactionID) ||
		!strings.EqualFold(j.snapshot.CandidateSHA256, expectedCandidateSHA256) ||
		!strings.EqualFold(j.snapshot.TargetUserSID, userSID) {
		return j, nativeBrokerOuterSettlementPrepared{}, &nativeBrokerJournalManualError{cause: errors.New(
			"outer package proof does not match the durable broker journal binding",
		)}
	}
	if _, _, err := j.loadProtectedArtifacts(); err != nil {
		return j, nativeBrokerOuterSettlementPrepared{}, &nativeBrokerJournalManualError{cause: err}
	}
	binding, bindingDigest, err := validateNativeBrokerOuterSettlementBinding(j, proof)
	if err != nil {
		return j, nativeBrokerOuterSettlementPrepared{}, &nativeBrokerJournalManualError{cause: err}
	}
	if err := verifyNativeBrokerJournalForwardState(ctx, j); err != nil {
		return j, nativeBrokerOuterSettlementPrepared{}, &nativeBrokerJournalManualError{cause: err}
	}
	if j.lastPhase() == nativeBrokerPhaseNestedReady {
		if err := j.appendPhase(nativeBrokerPhaseOuterSettlementPending, bindingDigest); err != nil {
			return j, nativeBrokerOuterSettlementPrepared{}, err
		}
	} else {
		pendingIndex := len(j.records) - 1
		if j.lastPhase() == nativeBrokerPhaseOuterSettled {
			pendingIndex--
		}
		if pendingIndex < 0 ||
			j.records[pendingIndex].Phase != nativeBrokerPhaseOuterSettlementPending ||
			j.records[pendingIndex].DetailSHA256 != bindingDigest {
			return j, nativeBrokerOuterSettlementPrepared{}, &nativeBrokerJournalManualError{cause: errors.New(
				"replayed outer settlement binding differs from the durable pending record",
			)}
		}
	}
	pendingIndex := len(j.records) - 1
	if j.lastPhase() == nativeBrokerPhaseOuterSettled {
		pendingIndex--
	}
	request := nativeBrokerOuterSettlementRequest{
		Schema:              nativeBrokerJournalSchema,
		BindingSHA256:       bindingDigest,
		BrokerPendingDigest: j.records[pendingIndex].RecordSHA256,
		Binding:             binding,
	}
	if err := validateNativeBrokerOuterSettlementRequest(j, request); err != nil {
		return j, nativeBrokerOuterSettlementPrepared{}, err
	}
	contents, requestDigest, err := encodeNativeBrokerOuterSettlementEnvelope(request)
	if err != nil {
		return j, nativeBrokerOuterSettlementPrepared{}, err
	}
	requestPath, err := nativeBrokerOuterSettlementRequestPath(j)
	if err != nil {
		return j, nativeBrokerOuterSettlementPrepared{}, err
	}
	return j, nativeBrokerOuterSettlementPrepared{
		Request: request, RequestPath: requestPath,
		RequestSHA256: requestDigest, contents: contents,
	}, nil
}

func validateNativeBrokerOuterSettlementReceipt(
	prepared nativeBrokerOuterSettlementPrepared,
	receipt nativePackageBrokerSettlementReceipt,
) error {
	binding := prepared.Request.Binding
	if receipt.BrokerTransactionID != binding.BrokerTransactionID ||
		receipt.BrokerPendingDigest != prepared.Request.BrokerPendingDigest ||
		receipt.DriverTransactionID != binding.DriverTransactionID ||
		receipt.DriverPendingDigest != binding.DriverPendingDigest ||
		receipt.SettlementNonce != binding.SettlementNonce ||
		receipt.RequestSHA256 != prepared.RequestSHA256 ||
		receipt.State != string(nativeBrokerPhaseOuterSettled) ||
		!isCanonicalNativeBrokerJournalSHA256(receipt.Digest) ||
		receipt.Digest == receipt.DriverPendingDigest {
		return errors.New("driver settlement receipt does not match both pending journal identities")
	}
	return nil
}

func nativeBrokerOuterSettlementFinalPath(j *nativeBrokerJournal) (string, error) {
	if j == nil {
		return "", errors.New("broker final receipt has no journal")
	}
	_, active, err := nativeBrokerJournalPaths(j.snapshot.TargetUserSID)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(filepath.Clean(j.directory), filepath.Clean(active)) {
		return "", errors.New("broker final receipt escaped the exact active journal")
	}
	path := filepath.Join(active, nativeBrokerJournalSettledReceiptName)
	if !strings.EqualFold(filepath.Dir(path), active) ||
		filepath.Base(path) != nativeBrokerJournalSettledReceiptName {
		return "", errors.New("broker final receipt path escaped its active journal")
	}
	return path, nil
}

func encodeNativeBrokerOuterSettlementFinalEnvelope(
	receipt nativeBrokerOuterSettlementFinal,
) ([]byte, string, error) {
	payload, err := nativeBrokerJournalCanonicalJSON(receipt)
	if err != nil {
		return nil, "", err
	}
	envelope := nativeBrokerOuterSettlementFinalEnvelope{
		Schema:        nativeBrokerJournalSchema,
		PayloadSHA256: nativeBrokerJournalHash(payload),
		Payload:       receipt,
	}
	contents, err := nativeBrokerJournalCanonicalJSON(envelope)
	if err != nil {
		return nil, "", err
	}
	if len(contents) > nativeBrokerJournalMaximumSettlement {
		return nil, "", errors.New("broker final receipt envelope exceeds its bound")
	}
	return contents, nativeBrokerJournalHash(contents), nil
}

func decodeNativeBrokerOuterSettlementFinalEnvelope(
	contents []byte,
) (nativeBrokerOuterSettlementFinalEnvelope, error) {
	var envelope nativeBrokerOuterSettlementFinalEnvelope
	if err := decodeCanonicalNativeBrokerJSON(
		contents, &envelope, nativeBrokerJournalMaximumSettlement,
	); err != nil {
		return nativeBrokerOuterSettlementFinalEnvelope{}, err
	}
	payload, err := nativeBrokerJournalCanonicalJSON(envelope.Payload)
	if err != nil {
		return nativeBrokerOuterSettlementFinalEnvelope{}, err
	}
	if envelope.Schema != nativeBrokerJournalSchema ||
		!isCanonicalNativeBrokerJournalSHA256(envelope.PayloadSHA256) ||
		envelope.PayloadSHA256 != nativeBrokerJournalHash(payload) {
		return nativeBrokerOuterSettlementFinalEnvelope{}, errors.New(
			"broker final receipt envelope schema or payload digest is invalid",
		)
	}
	return envelope, nil
}

func validateNativeBrokerOuterSettlementFinal(
	j *nativeBrokerJournal,
	prepared nativeBrokerOuterSettlementPrepared,
	driverReceipt nativePackageBrokerSettlementReceipt,
	receipt nativeBrokerOuterSettlementFinal,
) error {
	if j == nil || j.lastPhase() != nativeBrokerPhaseOuterSettled ||
		len(j.records) < 3 ||
		j.records[len(j.records)-2].Phase != nativeBrokerPhaseOuterSettlementPending ||
		j.records[len(j.records)-3].Phase != nativeBrokerPhaseNestedReady {
		return errors.New("broker final receipt has no exact terminal journal state")
	}
	driverReceiptBytes, err := nativeBrokerJournalCanonicalJSON(driverReceipt)
	if err != nil {
		return err
	}
	binding := prepared.Request.Binding
	if receipt.Schema != nativeBrokerJournalSchema ||
		receipt.BrokerTransactionID != binding.BrokerTransactionID ||
		receipt.BrokerPendingDigest != prepared.Request.BrokerPendingDigest ||
		receipt.BrokerSettledDigest != j.records[len(j.records)-1].RecordSHA256 ||
		receipt.DriverTransactionID != binding.DriverTransactionID ||
		receipt.DriverPendingDigest != binding.DriverPendingDigest ||
		receipt.DriverSettledDigest != driverReceipt.Digest ||
		receipt.SettlementNonce != binding.SettlementNonce ||
		receipt.RequestSHA256 != prepared.RequestSHA256 ||
		receipt.State != string(nativeBrokerPhaseOuterSettled) ||
		j.records[len(j.records)-1].DetailSHA256 != nativeBrokerJournalHash(driverReceiptBytes) {
		return errors.New("broker final receipt does not bind both terminal journal digests")
	}
	for _, digest := range []string{
		receipt.BrokerPendingDigest, receipt.BrokerSettledDigest,
		receipt.DriverTransactionID, receipt.DriverPendingDigest,
		receipt.DriverSettledDigest, receipt.SettlementNonce,
		receipt.RequestSHA256,
	} {
		if !isCanonicalNativeBrokerJournalSHA256(digest) {
			return errors.New("broker final receipt contains a malformed digest")
		}
	}
	return nil
}

func loadNativeBrokerOuterSettlementFinal(
	j *nativeBrokerJournal,
	prepared nativeBrokerOuterSettlementPrepared,
	driverReceipt nativePackageBrokerSettlementReceipt,
) (nativeBrokerOuterSettlementFinalPrepared, error) {
	path, err := nativeBrokerOuterSettlementFinalPath(j)
	if err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	contents, err := readNativeBrokerJournalFile(path, nativeBrokerJournalMaximumSettlement)
	if err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	envelope, err := decodeNativeBrokerOuterSettlementFinalEnvelope(contents)
	if err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	if err := validateNativeBrokerOuterSettlementFinal(
		j, prepared, driverReceipt, envelope.Payload,
	); err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	return nativeBrokerOuterSettlementFinalPrepared{
		Receipt: envelope.Payload, ReceiptPath: path,
		ReceiptSHA256: nativeBrokerJournalHash(contents), contents: contents,
	}, nil
}

func loadNativeBrokerOuterSettlementFinalForReconciliation(
	j *nativeBrokerJournal,
) (nativeBrokerOuterSettlementFinalPrepared, error) {
	prepared, err := loadNativeBrokerOuterSettlementRequest(j)
	if err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	path, err := nativeBrokerOuterSettlementFinalPath(j)
	if err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	contents, err := readNativeBrokerJournalFile(path, nativeBrokerJournalMaximumSettlement)
	if err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	envelope, err := decodeNativeBrokerOuterSettlementFinalEnvelope(contents)
	if err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	driverReceipt := nativeBrokerDriverReceiptFromFinal(envelope.Payload)
	if err := validateNativeBrokerOuterSettlementReceipt(prepared, driverReceipt); err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	if err := validateNativeBrokerOuterSettlementFinal(
		j, prepared, driverReceipt, envelope.Payload,
	); err != nil {
		return nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	return nativeBrokerOuterSettlementFinalPrepared{
		Receipt: envelope.Payload, ReceiptPath: path,
		ReceiptSHA256: nativeBrokerJournalHash(contents), contents: contents,
	}, nil
}

func publishNativeBrokerOuterSettlementFinal(
	j *nativeBrokerJournal,
	prepared nativeBrokerOuterSettlementFinalPrepared,
	driverRequest nativeBrokerOuterSettlementPrepared,
	driverReceipt nativePackageBrokerSettlementReceipt,
) error {
	expectedPath, err := nativeBrokerOuterSettlementFinalPath(j)
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(prepared.ReceiptPath), filepath.Clean(expectedPath)) ||
		prepared.ReceiptSHA256 != nativeBrokerJournalHash(prepared.contents) {
		return errors.New("broker final receipt publication identity changed")
	}
	if envelope, err := decodeNativeBrokerOuterSettlementFinalEnvelope(prepared.contents); err != nil {
		return err
	} else if envelope.Payload != prepared.Receipt {
		return errors.New("broker final receipt publication payload changed")
	}
	if err := validateNativeBrokerOuterSettlementFinal(
		j, driverRequest, driverReceipt, prepared.Receipt,
	); err != nil {
		return err
	}
	staging := expectedPath + ".next"
	loadOptional := func(path string) ([]byte, bool, error) {
		if _, err := nativePathAttributes(path); err != nil {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return nil, false, nil
			}
			return nil, false, err
		}
		contents, err := readNativeBrokerJournalFile(path, nativeBrokerJournalMaximumSettlement)
		return contents, true, err
	}
	if err := executeNativeBrokerSettlementPublication(
		prepared.contents,
		nativeBrokerSettlementPublicationOperations{
			loadPublished: func() ([]byte, bool, error) { return loadOptional(expectedPath) },
			loadStaging:   func() ([]byte, bool, error) { return loadOptional(staging) },
			discardStaging: func() error {
				return discardUnpublishedNativeBrokerJournalFile(staging)
			},
			publishStaging: func() error {
				return moveNativePackageFile(staging, expectedPath, false)
			},
			writeNew: func() error {
				return writeNativeBrokerJournalFile(
					expectedPath, prepared.contents, nativeBrokerJournalMaximumSettlement,
				)
			},
			readback: func() ([]byte, error) {
				return readNativeBrokerJournalFile(expectedPath, nativeBrokerJournalMaximumSettlement)
			},
		},
	); err != nil {
		return fmt.Errorf("publish broker final receipt: %w", err)
	}
	loaded, err := loadNativeBrokerOuterSettlementFinal(j, driverRequest, driverReceipt)
	if err != nil {
		return fmt.Errorf("read back broker final receipt: %w", err)
	}
	if loaded.Receipt != prepared.Receipt || loaded.ReceiptSHA256 != prepared.ReceiptSHA256 ||
		!bytes.Equal(loaded.contents, prepared.contents) {
		return errors.New("published broker final receipt differs from its durable bytes")
	}
	return nil
}

func recordNativeBrokerOuterSettlement(
	ctx context.Context,
	userSID string,
	receipt nativePackageBrokerSettlementReceipt,
) (*nativeBrokerJournal, nativeBrokerOuterSettlementFinalPrepared, error) {
	_, active, err := nativeBrokerJournalPaths(userSID)
	if err != nil {
		return nil, nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	j, err := loadNativeBrokerJournal(active)
	if err != nil {
		return nil, nativeBrokerOuterSettlementFinalPrepared{}, &nativeBrokerJournalManualError{cause: err}
	}
	if j.lastPhase() != nativeBrokerPhaseOuterSettlementPending &&
		j.lastPhase() != nativeBrokerPhaseOuterSettled {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, &nativeBrokerJournalManualError{cause: fmt.Errorf(
			"broker settlement acknowledgement observed phase %s", j.lastPhase(),
		)}
	}
	prepared, err := loadNativeBrokerOuterSettlementRequest(j)
	if err != nil {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, &nativeBrokerJournalManualError{cause: err}
	}
	if err := validateNativeBrokerOuterSettlementReceipt(prepared, receipt); err != nil {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, &nativeBrokerJournalManualError{cause: err}
	}
	if _, _, err := j.loadProtectedArtifacts(); err != nil {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, &nativeBrokerJournalManualError{cause: err}
	}
	if err := verifyNativeBrokerJournalForwardState(ctx, j); err != nil {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, &nativeBrokerJournalManualError{cause: err}
	}
	receiptBytes, err := nativeBrokerJournalCanonicalJSON(receipt)
	if err != nil {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	receiptDigest := nativeBrokerJournalHash(receiptBytes)
	if j.lastPhase() == nativeBrokerPhaseOuterSettlementPending {
		if err := j.appendPhase(nativeBrokerPhaseOuterSettled, receiptDigest); err != nil {
			return j, nativeBrokerOuterSettlementFinalPrepared{}, err
		}
	} else if j.records[len(j.records)-1].DetailSHA256 != receiptDigest {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, &nativeBrokerJournalManualError{cause: errors.New(
			"replayed driver settlement receipt differs from the terminal broker record",
		)}
	}
	finalReceipt := nativeBrokerOuterSettlementFinal{
		Schema:              nativeBrokerJournalSchema,
		BrokerTransactionID: prepared.Request.Binding.BrokerTransactionID,
		BrokerPendingDigest: prepared.Request.BrokerPendingDigest,
		BrokerSettledDigest: j.records[len(j.records)-1].RecordSHA256,
		DriverTransactionID: receipt.DriverTransactionID,
		DriverPendingDigest: receipt.DriverPendingDigest,
		DriverSettledDigest: receipt.Digest,
		SettlementNonce:     receipt.SettlementNonce,
		RequestSHA256:       receipt.RequestSHA256,
		State:               string(nativeBrokerPhaseOuterSettled),
	}
	if err := validateNativeBrokerOuterSettlementFinal(
		j, prepared, receipt, finalReceipt,
	); err != nil {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	contents, finalDigest, err := encodeNativeBrokerOuterSettlementFinalEnvelope(finalReceipt)
	if err != nil {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	finalPath, err := nativeBrokerOuterSettlementFinalPath(j)
	if err != nil {
		return j, nativeBrokerOuterSettlementFinalPrepared{}, err
	}
	finalPrepared := nativeBrokerOuterSettlementFinalPrepared{
		Receipt: finalReceipt, ReceiptPath: finalPath,
		ReceiptSHA256: finalDigest, contents: contents,
	}
	if err := publishNativeBrokerOuterSettlementFinal(
		j, finalPrepared, prepared, receipt,
	); err != nil {
		return j, finalPrepared, err
	}
	return j, finalPrepared, nil
}

func nativeBrokerJournalAbsenceIsSettled(proof nativePackageInstallProof) bool {
	if proof.success {
		// The driver can change while the child proves an exact healthy no-op.
		// In that case the child correctly owns no journal; any advertised child
		// identity still requires its exact active journal.
		return proof.journal.TransactionID == ""
	}
	return !proof.changed || proof.rollback == "succeeded"
}

func reconcileNativeBrokerJournalAfterOuterFailure(
	ctx context.Context,
	userSID, outerTransactionID, candidateSHA256 string,
	proof nativePackageInstallProof,
) (nativeBrokerJournalProof, error) {
	_, active, err := nativeBrokerJournalPaths(userSID)
	if err != nil {
		return nativeBrokerJournalProof{}, err
	}
	if _, err := nativePathAttributes(active); err != nil {
		if (errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)) &&
			nativeBrokerJournalAbsenceIsSettled(proof) {
			return nativeBrokerJournalProof{}, nil
		}
		return nativeBrokerJournalProof{}, err
	}
	j, err := loadNativeBrokerJournal(active)
	if err != nil {
		return nativeBrokerJournalProof{}, &nativeBrokerJournalManualError{cause: err}
	}
	expectedOuterTransactionID, expectedCandidateSHA256 :=
		nativeBrokerJournalOuterSettlementIdentity(outerTransactionID, candidateSHA256, proof)
	if !strings.EqualFold(j.snapshot.OuterTransactionID, expectedOuterTransactionID) ||
		!strings.EqualFold(j.snapshot.CandidateSHA256, expectedCandidateSHA256) ||
		!strings.EqualFold(j.snapshot.TargetUserSID, userSID) {
		return j.proof(), &nativeBrokerJournalManualError{cause: errors.New(
			"outer failure proof does not match the durable broker journal binding",
		)}
	}
	if _, _, err := j.loadProtectedArtifacts(); err != nil {
		return j.proof(), &nativeBrokerJournalManualError{cause: err}
	}
	if proof.rollback == "succeeded" || (!proof.changed && proof.rollback == "not-needed") {
		if j.lastPhase() != nativeBrokerPhaseRollbackSettled {
			if err := rollbackNativeBrokerJournal(ctx, j); err != nil {
				return j.proof(), err
			}
		}
		return j.proof(), nil
	}
	return j.proof(), &nativeBrokerJournalManualError{cause: errors.New(
		"outer package proof did not authorize broker journal settlement or rollback",
	)}
}

func admitNativeBrokerServiceStartup(executable, credentialPath string) error {
	active, err := nativeBrokerJournalActivePathUnbound()
	if err != nil {
		return err
	}
	if _, err := nativePathAttributes(active); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	handle, _, err := createOrOpenProtectedNativeBrokerJournalDirectory(active, false)
	if err != nil {
		return &nativeBrokerJournalManualError{cause: err}
	}
	windows.CloseHandle(handle) //nolint:errcheck
	j, err := loadNativeBrokerJournal(active)
	if err != nil {
		return &nativeBrokerJournalManualError{cause: err}
	}
	priorCredential, _, err := j.loadProtectedArtifacts()
	if err != nil {
		return &nativeBrokerJournalManualError{cause: err}
	}
	executable = filepath.Clean(executable)
	credentialPath = filepath.Clean(credentialPath)
	expectedCredentialPath, err := nativeServiceKeyFilePath()
	if err != nil {
		return err
	}
	if !strings.EqualFold(executable, filepath.Clean(j.snapshot.CandidatePath)) ||
		!strings.EqualFold(credentialPath, filepath.Clean(expectedCredentialPath)) {
		return &nativeBrokerJournalManualError{cause: errors.New(
			"service startup identity differs from the active broker journal",
		)}
	}
	imageHash, imageExists, err := nativeBrokerJournalPathHash(executable)
	if err != nil {
		return err
	}
	credential, credentialExists, err := snapshotNativeBrokerCredentialReadOnly(j.snapshot.TargetUserSID)
	if err != nil {
		return err
	}
	switch j.lastPhase() {
	case nativeBrokerPhaseServiceStartIntent:
		candidateCredential := currentNativeBrokerJournalCandidateCredentialDigest(j)
		if !imageExists || !strings.EqualFold(imageHash, j.snapshot.CandidateSHA256) ||
			!credentialExists || candidateCredential == "" ||
			!strings.EqualFold(nativeBrokerJournalHash(credential), candidateCredential) {
			return &nativeBrokerJournalManualError{cause: errors.New(
				"candidate broker startup did not match its durable image/key identities",
			)}
		}
		return nil
	case nativeBrokerPhaseRollbackLegacy, nativeBrokerPhaseRollbackSettled:
		if !j.snapshot.Service.Exists || !j.snapshot.PriorImageExists || !imageExists ||
			!strings.EqualFold(imageHash, j.snapshot.PriorImageSHA256) ||
			credentialExists != priorCredential.Exists ||
			(credentialExists && !strings.EqualFold(
				nativeBrokerJournalHash(credential), j.snapshot.PriorCredentialSHA256,
			)) {
			return &nativeBrokerJournalManualError{cause: errors.New(
				"rollback broker startup did not match its durable prior image/key identities",
			)}
		}
		return nil
	case nativeBrokerPhaseNestedReady, nativeBrokerPhaseOuterSettlementPending,
		nativeBrokerPhaseOuterSettled:
		candidateCredential := currentNativeBrokerJournalCandidateCredentialDigest(j)
		if !imageExists || !strings.EqualFold(imageHash, j.snapshot.CandidateSHA256) ||
			!credentialExists || candidateCredential == "" ||
			!strings.EqualFold(nativeBrokerJournalHash(credential), candidateCredential) {
			return &nativeBrokerJournalManualError{cause: errors.New(
				"settled broker startup did not match its durable image/key identities",
			)}
		}
		return nil
	default:
		return &nativeBrokerJournalManualError{cause: fmt.Errorf(
			"service startup is not admitted while broker journal phase is %s", j.lastPhase(),
		)}
	}
}
