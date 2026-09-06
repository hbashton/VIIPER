package xboxone

import (
	"errors"
	"fmt"
)

// ControllerPersonaMaximumWireSize is the fixed private Command/Low/Standard
// message capacity used by one composed persona claim.
const ControllerPersonaMaximumWireSize = 64

// ControllerPersonaBlocker is an explicit capability gate which this offline
// engine does not satisfy. The set is intentionally value-only so a future
// adapter can refuse registration without parsing documentation strings.
type ControllerPersonaBlocker uint32

const (
	// ControllerPersonaBlockerExternalUSBStrings records the missing validated
	// manufacturer, product, and serial descriptor seam.
	ControllerPersonaBlockerExternalUSBStrings ControllerPersonaBlocker = 1 << iota
	// ControllerPersonaBlockerIdentityAuthorization records the absence of an
	// explicit external caller attestation for this exact identity lifetime. It
	// is not package-verified VID/PID ownership or legal authorization.
	ControllerPersonaBlockerIdentityAuthorization
	ControllerPersonaBlockerMetadataSemanticValidation
	ControllerPersonaBlockerAuthentication
	ControllerPersonaBlockerTransferReassembly
	ControllerPersonaBlockerReceiveReplayPolicy
	ControllerPersonaBlockerGuideInput
	ControllerPersonaBlockerStatusEvents
	ControllerPersonaBlockerUSBPowerLifecycle
	ControllerPersonaBlockerBackendIntegration
	ControllerPersonaBlockerWindowsBinding
	ControllerPersonaBlockerHardwareConformance
	// ControllerPersonaBlockerMultiContextPacketAuthority records that a
	// packet may own only one lifecycle/ACK-governed member.
	ControllerPersonaBlockerMultiContextPacketAuthority
)

// ControllerPersonaBlockerGuideInputAndShare is retained as a source-
// compatibility alias. Share is no longer part of this blocker: the strict
// official Console Function Map metadata variant owns that input form.
const ControllerPersonaBlockerGuideInputAndShare = ControllerPersonaBlockerGuideInput

const controllerPersonaKnownBlockers = ControllerPersonaBlockerExternalUSBStrings |
	ControllerPersonaBlockerIdentityAuthorization |
	ControllerPersonaBlockerMetadataSemanticValidation |
	ControllerPersonaBlockerAuthentication |
	ControllerPersonaBlockerTransferReassembly |
	ControllerPersonaBlockerReceiveReplayPolicy |
	ControllerPersonaBlockerStatusEvents |
	ControllerPersonaBlockerUSBPowerLifecycle |
	ControllerPersonaBlockerBackendIntegration |
	ControllerPersonaBlockerWindowsBinding |
	ControllerPersonaBlockerHardwareConformance |
	ControllerPersonaBlockerMultiContextPacketAuthority

// ControllerPersonaAction identifies one immutable adapter obligation.
type ControllerPersonaAction uint8

const (
	ControllerPersonaUSBControl ControllerPersonaAction = iota + 1
	ControllerPersonaSendHello
	ControllerPersonaBeginMetadata
	ControllerPersonaSendMetadata
	ControllerPersonaSendCurrentStatus
	ControllerPersonaSendPoweringOffStatus
	ControllerPersonaSendInitialInput
	ControllerPersonaSendInput
	ControllerPersonaPermitNormalUpstream
	ControllerPersonaGateNormalUpstream
	ControllerPersonaClearOutputs
	ControllerPersonaApplyMetadataAcknowledgement
	ControllerPersonaApplyDirectMotor
	ControllerPersonaApplyGuideLED
	ControllerPersonaCompletePowerOff
	ControllerPersonaPerformReset
	// ControllerPersonaIgnoreHostMessage represents the source-defined FULL
	// POWER no-op only inside an admitted downstream packet vector. It has no
	// wire or local effect but preserves the host message's exact packet order.
	ControllerPersonaIgnoreHostMessage
	// ControllerPersonaSendGuideButtonStatus emits one ordered upstream system
	// Command 7 transition from the Global sequence pool.
	ControllerPersonaSendGuideButtonStatus
)

// ControllerPersonaOutcome is the terminal result for one adapter obligation.
// Deferred and DeliveryFailed assert that no effect can become visible later.
// ExecutionCancelled requires synchronous cancellation and drain. Every
// non-delivered non-EP0 result retains the same immutable mandatory retry.
type ControllerPersonaOutcome uint8

const (
	ControllerPersonaDelivered ControllerPersonaOutcome = iota + 1
	ControllerPersonaDeferred
	ControllerPersonaDeliveryFailed
	ControllerPersonaExecutionCancelled
)

// ControllerPersonaHostDisposition reports the action selected for an exact
// downstream message. The acknowledgement values describe a previewed
// reliable ACK effect carried by the returned persona claim; they do not mean
// progress has already committed. Ignored is used only for the source-defined
// FULL POWER no-op.
type ControllerPersonaHostDisposition uint8

const (
	ControllerPersonaHostActionClaimed ControllerPersonaHostDisposition = iota + 1
	ControllerPersonaHostAcknowledgementProgress
	ControllerPersonaHostAcknowledgementRewind
	ControllerPersonaHostAcknowledgementDuplicate
	ControllerPersonaHostIgnored
)

// ControllerPersonaConfig contains only explicit caller-owned facts. It does
// not supply a default identity, external strings, metadata validation, or a
// backend. PoweringOffStatus must use the powering-off power value.
type ControllerPersonaConfig struct {
	Profile           UnregisteredControllerProfile
	Metadata          BoundCompiledMetadata
	CurrentInput      GamepadInputReportV1
	CurrentStatus     ExtendedStatusNoEventsBodyV1
	PoweringOffStatus ExtendedStatusNoEventsBodyV1
}

// ControllerPersonaClaim is an opaque capability for one exact action.
type ControllerPersonaClaim struct {
	owner                   *ControllerPersonaEngine
	token                   uint64
	generation              uint64
	action                  ControllerPersonaAction
	size                    uint8
	sequence                uint8
	clearEpoch              uint64
	selectedAtMS            uint64
	usbResponseKind         USBControlResponseKind
	metadataKind            MetadataPacketKind
	metadataAcknowledgement bool
	reliableAcknowledgement ReliableAcknowledgement
	reliableDisposition     ReliableAcknowledgementDisposition
	directMotor             RumbleBodyV1
	guideLED                GuideLEDCommandV1
	configurationLossClear  bool
}

func (claim ControllerPersonaClaim) Valid() bool {
	return claim.owner != nil && claim.token != 0 && claim.generation != 0
}
func (claim ControllerPersonaClaim) Generation() uint64 { return claim.generation }
func (claim ControllerPersonaClaim) Action() ControllerPersonaAction {
	return claim.action
}
func (claim ControllerPersonaClaim) Size() int          { return int(claim.size) }
func (claim ControllerPersonaClaim) Sequence() uint8    { return claim.sequence }
func (claim ControllerPersonaClaim) ClearEpoch() uint64 { return claim.clearEpoch }
func (claim ControllerPersonaClaim) SelectedAtMilliseconds() uint64 {
	return claim.selectedAtMS
}
func (claim ControllerPersonaClaim) USBResponseKind() USBControlResponseKind {
	return claim.usbResponseKind
}
func (claim ControllerPersonaClaim) MetadataKind() MetadataPacketKind {
	return claim.metadataKind
}
func (claim ControllerPersonaClaim) MetadataAcknowledgementRequested() bool {
	return claim.metadataAcknowledgement
}
func (claim ControllerPersonaClaim) ReliableAcknowledgementDisposition() (
	ReliableAcknowledgementDisposition, bool,
) {
	return claim.reliableDisposition,
		claim.action == ControllerPersonaApplyMetadataAcknowledgement
}
func (claim ControllerPersonaClaim) DirectMotor() (RumbleBodyV1, bool) {
	return claim.directMotor, claim.action == ControllerPersonaApplyDirectMotor
}
func (claim ControllerPersonaClaim) GuideLED() (GuideLEDCommandV1, bool) {
	return claim.guideLED, claim.action == ControllerPersonaApplyGuideLED
}

// RequiresConfigurationLossClear reports that successful delivery of this
// exact EP0 claim will derive one mandatory typed output-clear obligation.
func (claim ControllerPersonaClaim) RequiresConfigurationLossClear() bool {
	return claim.action == ControllerPersonaUSBControl && claim.configurationLossClear
}

type controllerPersonaSequencePool uint8

const (
	controllerPersonaNoSequence controllerPersonaSequencePool = iota
	controllerPersonaGlobalSequence
	controllerPersonaInputSequence
)

type controllerPersonaRecord struct {
	action       ControllerPersonaAction
	wire         [ControllerPersonaMaximumWireSize]byte
	size         uint8
	sequence     uint8
	clearEpoch   uint64
	selectedAtMS uint64

	usbClaim       USBControlClaim
	lifecycleClaim ControllerLifecycleClaim
	metadataClaim  MetadataPacketClaim
	sequenceClaim  SequenceClaim
	sequencePool   controllerPersonaSequencePool

	usbOwned       bool
	lifecycleOwned bool
	metadataOwned  bool
	// metadataFailureHello is set only on the lifecycle Hello selected after
	// this exact transfer faults. It prevents an unrelated Arrival Hello from
	// retiring metadata state and survives the wrapper's immutable retry path.
	metadataFailureHello bool

	directMotor             RumbleBodyV1
	guideLED                GuideLEDCommandV1
	usbResponseKind         USBControlResponseKind
	metadataKind            MetadataPacketKind
	metadataAcknowledgement bool
	reliableAcknowledgement ReliableAcknowledgement
	reliableDisposition     ReliableAcknowledgementDisposition
	reliableAdmittedAtMS    uint64
	reliableAdmitted        bool
	configurationLossClear  bool
}

// ControllerPersonaFeedbackSnapshot is the last successfully applied typed
// feedback state. ClearEpoch identifies the most recent logical all-output
// release; retries preserve one epoch instead of manufacturing extra stops.
type ControllerPersonaFeedbackSnapshot struct {
	DirectMotor RumbleBodyV1
	GuideLED    GuideLEDCommandV1
	ClearEpoch  uint64
}

// ControllerPersonaSnapshot is a value-only diagnostic view of the composed
// state, its serialized protocol claim lane, and its bounded ordinary-feedback
// side owner. RetryPending and ClaimOutstanding describe only the protocol lane.
type ControllerPersonaSnapshot struct {
	Generation                    uint64
	Attached                      bool
	USBState                      USBControlDeviceState
	LifecycleState                ControllerLifecycleState
	NormalUpstream                bool
	MetadataActive                bool
	ClaimOutstanding              bool
	ClaimAdmitted                 bool
	RetryPending                  bool
	OrdinaryFeedbackOutstanding   bool
	OrdinaryFeedbackAdmitted      bool
	OrdinaryFeedbackRetryPending  bool
	OrdinaryFeedbackAction        ControllerPersonaAction
	ConfigurationLossClearPending bool
	GlobalSequence                uint8
	InputSequence                 uint8
	Feedback                      ControllerPersonaFeedbackSnapshot
	CapabilityBlockers            ControllerPersonaBlocker
}

// ControllerPersonaEngine composes the pure USB control, GIP lifecycle,
// reliable metadata, sequence, input, and typed feedback contracts. It never
// registers a USB device, performs I/O, sleeps, or applies hardware output.
// Calls must be serialized and the value must not be copied after first use.
// The authorized constructor returns one exact pointer; copying that engine
// even before use deliberately loses its external-identity authority.
type ControllerPersonaEngine struct {
	initialized        bool
	profile            UnregisteredControllerProfile
	metadata           BoundCompiledMetadata
	identityBinding    *controllerPersonaExternalIdentityBinding
	capabilityBlockers ControllerPersonaBlocker

	usb       USBControlPlane
	lifecycle ControllerLifecycle
	global    SequenceCounter
	input     SequenceCounter

	generation     uint64
	attached       bool
	normalUpstream bool
	lastNowMS      uint64

	currentInput      GamepadInputReportV1
	currentStatus     ExtendedStatusNoEventsBodyV1
	poweringOffStatus ExtendedStatusNoEventsBodyV1
	statusTimer       controllerPersonaStatusTimer

	metadataTransfer MetadataTransfer
	metadataFence    ControllerMetadataTransferFence
	metadataActive   bool

	directMotor RumbleBodyV1
	guideLED    GuideLEDCommandV1

	nextClearEpoch   uint64
	lastClearEpoch   uint64
	nextToken        uint64
	hasClaim         bool
	claimAdmitted    bool
	claimToken       uint64
	claimRecord      controllerPersonaRecord
	retryPending     bool
	retryRecord      controllerPersonaRecord
	ordinaryFeedback controllerPersonaOrdinaryFeedbackLane

	configurationLossClearPending bool
	configurationLossClearRecord  controllerPersonaRecord

	// hostPacket is the dormant whole-packet composition lane. It reuses the
	// canonical persona claim and retry owners; no second lifecycle, ACK, or
	// feedback state machine exists here.
	hostPacketActive       bool
	hostPacketRetryPending bool
	hostPacketQuarantined  bool
	hostPacketQuarantine   error
	nextHostPacketEpoch    uint64
	hostPacketRecord       controllerPersonaHostPacketRecord
	hostPacketRetryRecord  controllerPersonaHostPacketRecord
}

// NewControllerPersonaEngine constructs generation one in USB Default and GIP
// Arrival. Metadata remains opaque but must already be bound to this profile.
func NewControllerPersonaEngine(
	config ControllerPersonaConfig,
	nowMS uint64,
) (ControllerPersonaEngine, error) {
	return newControllerPersonaEngine(config, nil, nowMS)
}

func validateControllerPersonaConfig(config ControllerPersonaConfig) error {
	if err := config.Profile.validate(); err != nil {
		return err
	}
	if !config.Metadata.valid || config.Metadata.issuance == nil ||
		config.Metadata.issuance.marker != 1 ||
		len(config.Metadata.data) == 0 ||
		len(config.Metadata.data) > MetadataMaximumBoundLength {
		return ErrInvalidMetadata
	}
	if config.Metadata.identity != config.Profile.identity ||
		config.Metadata.profileIssuance != config.Profile.issuance {
		return ErrMetadataIdentityMismatch
	}
	if err := config.Metadata.validateInputReport(config.CurrentInput); err != nil {
		return err
	}
	if err := config.CurrentStatus.Validate(); err != nil {
		return err
	}
	if err := config.PoweringOffStatus.Validate(); err != nil {
		return err
	}
	if config.CurrentStatus.PowerLevel != StatusFullPower ||
		config.PoweringOffStatus.PowerLevel != StatusPoweringOff {
		return ErrInvalidControllerPersonaConfig
	}
	return nil
}

func newControllerPersonaEngine(
	config ControllerPersonaConfig,
	binding *controllerPersonaExternalIdentityBinding,
	nowMS uint64,
) (ControllerPersonaEngine, error) {
	if err := validateControllerPersonaConfig(config); err != nil {
		return ControllerPersonaEngine{}, err
	}
	if binding != nil && !binding.authenticatesConfig(config) {
		return ControllerPersonaEngine{}, ErrInvalidControllerIdentityAuthorization
	}
	usb, err := newUSBControlPlane(config.Profile, binding)
	if err != nil {
		return ControllerPersonaEngine{}, err
	}
	blockers := controllerPersonaKnownBlockers
	if config.Metadata.officialGamepadVariant != 0 {
		blockers &^= ControllerPersonaBlockerMetadataSemanticValidation
	}
	if binding != nil {
		blockers &^= ControllerPersonaBlockerExternalUSBStrings |
			ControllerPersonaBlockerIdentityAuthorization
	}
	return ControllerPersonaEngine{
		initialized: true, profile: config.Profile, metadata: config.Metadata,
		identityBinding: binding, capabilityBlockers: blockers,
		usb: usb, lifecycle: NewControllerLifecycle(nowMS),
		generation: 1, attached: true, lastNowMS: nowMS,
		currentInput: config.CurrentInput, currentStatus: config.CurrentStatus,
		poweringOffStatus: config.PoweringOffStatus,
		guideLED:          GuideLEDCommandV1{Pattern: GuideLEDPatternOff},
	}, nil
}

// CapabilityBlockers returns every unsupported gate which prevents treating
// this offline engine as a registrable or Windows-compatible persona.
func (engine *ControllerPersonaEngine) CapabilityBlockers() ControllerPersonaBlocker {
	if engine == nil || !engine.initialized {
		return controllerPersonaKnownBlockers
	}
	if engine.capabilityBlockers&^controllerPersonaKnownBlockers != 0 ||
		engine.capabilityBlockers == 0 {
		return controllerPersonaKnownBlockers
	}
	want := controllerPersonaKnownBlockers
	if engine.metadata.officialGamepadVariant != 0 {
		want &^= ControllerPersonaBlockerMetadataSemanticValidation
	}
	if engine.identityBinding == nil {
		if engine.capabilityBlockers != want {
			return controllerPersonaKnownBlockers
		}
		return engine.capabilityBlockers
	}
	want &^= ControllerPersonaBlockerExternalUSBStrings |
		ControllerPersonaBlockerIdentityAuthorization
	if engine.capabilityBlockers != want || !engine.identityBinding.authenticatesEngine(engine) {
		return controllerPersonaKnownBlockers
	}
	return engine.capabilityBlockers
}

func (engine *ControllerPersonaEngine) Snapshot() ControllerPersonaSnapshot {
	if engine == nil {
		return ControllerPersonaSnapshot{CapabilityBlockers: controllerPersonaKnownBlockers}
	}
	return ControllerPersonaSnapshot{
		Generation: engine.generation, Attached: engine.attached,
		USBState: engine.usb.Snapshot().State, LifecycleState: engine.lifecycle.State(),
		NormalUpstream: engine.normalUpstream, MetadataActive: engine.metadataActive,
		ClaimOutstanding: engine.hasClaim, ClaimAdmitted: engine.claimAdmitted,
		RetryPending:                  engine.retryPending,
		OrdinaryFeedbackOutstanding:   engine.ordinaryFeedback.state == controllerPersonaOrdinaryFeedbackAdmitted,
		OrdinaryFeedbackAdmitted:      engine.ordinaryFeedback.state == controllerPersonaOrdinaryFeedbackAdmitted,
		OrdinaryFeedbackRetryPending:  engine.ordinaryFeedbackRetryPending(),
		OrdinaryFeedbackAction:        engine.ordinaryFeedback.record.action,
		ConfigurationLossClearPending: engine.configurationLossClearPending,
		GlobalSequence:                engine.global.LastCommitted(),
		InputSequence:                 engine.input.LastCommitted(),
		Feedback: ControllerPersonaFeedbackSnapshot{
			DirectMotor: engine.directMotor, GuideLED: engine.guideLED,
			ClearEpoch: engine.lastClearEpoch,
		},
		CapabilityBlockers: engine.CapabilityBlockers(),
	}
}

func (engine *ControllerPersonaEngine) validateInitialized() error {
	if engine == nil || !engine.initialized || engine.generation == 0 {
		return ErrUninitializedControllerPersona
	}
	return nil
}

func (engine *ControllerPersonaEngine) observeClock(nowMS uint64) error {
	if nowMS < engine.lastNowMS {
		return fmt.Errorf("%w: now=%d previous=%d",
			ErrNonMonotonicControllerPersonaClock, nowMS, engine.lastNowMS)
	}
	engine.lastNowMS = nowMS
	return nil
}

func (engine *ControllerPersonaEngine) ensureNewClaimAllowed() error {
	if err := engine.ensurePrimaryClaimAllowed(); err != nil {
		return err
	}
	return engine.ensureNoOrdinaryFeedback()
}

// ensurePrimaryClaimAllowed preserves the protocol lane's original ownership
// gates. Only ordinary IN selection may use it while feedback is detached.
func (engine *ControllerPersonaEngine) ensurePrimaryClaimAllowed() error {
	if err := engine.validateInitialized(); err != nil {
		return err
	}
	if engine.hostPacketQuarantined {
		return errors.Join(ErrControllerPersonaHostPacketQuarantined,
			engine.hostPacketQuarantine)
	}
	if engine.hasClaim {
		return ErrControllerPersonaClaimOutstanding
	}
	if engine.retryPending {
		return ErrControllerPersonaRetryRequired
	}
	if engine.configurationLossClearPending {
		return ErrControllerPersonaConfigurationLossClearRequired
	}
	if err := engine.validateInnerIdle(); err != nil {
		return err
	}
	return nil
}

func (engine *ControllerPersonaEngine) validateInnerIdle() error {
	if engine.hostPacketActive || engine.hostPacketRetryPending {
		return ErrControllerPersonaInvariantViolation
	}
	usb := engine.usb.Snapshot()
	lifecycle := engine.lifecycle.Snapshot()
	if usb.ClaimOutstanding || lifecycle.ClaimOutstanding || lifecycle.RetryPending ||
		engine.global.hasClaim || engine.global.retryPending ||
		engine.input.hasClaim || engine.input.retryPending {
		return ErrControllerPersonaInvariantViolation
	}
	metadata := engine.metadataTransfer.Snapshot()
	if metadata.ClaimOutstanding || metadata.RetryPending {
		return ErrControllerPersonaInvariantViolation
	}
	return nil
}

func (engine *ControllerPersonaEngine) validateLifecycleEmissionSources() error {
	if err := engine.profile.validate(); err != nil {
		return err
	}
	if err := engine.metadata.validateInputReport(engine.currentInput); err != nil {
		return err
	}
	if err := engine.currentStatus.Validate(); err != nil {
		return err
	}
	if err := engine.poweringOffStatus.Validate(); err != nil {
		return err
	}
	if engine.currentStatus.PowerLevel != StatusFullPower ||
		engine.poweringOffStatus.PowerLevel != StatusPoweringOff {
		return ErrInvalidControllerPersonaConfig
	}
	if err := engine.validateClearEpochAvailable(); err != nil {
		return err
	}
	return nil
}

func previewControllerPersonaSequence(counter *SequenceCounter) (uint8, error) {
	if counter.hasClaim {
		return 0, ErrSequenceClaimOutstanding
	}
	if counter.retryPending {
		return 0, ErrSequenceRetryRequired
	}
	value := counter.last + 1
	if value == 0 {
		value = 1
	}
	return value, nil
}

func (engine *ControllerPersonaEngine) ensureGIPConfigured() error {
	if !engine.attached {
		return ErrControllerPersonaDetached
	}
	if engine.usb.Snapshot().State != USBControlDeviceConfigured {
		return ErrControllerPersonaUSBNotConfigured
	}
	return nil
}

func (engine *ControllerPersonaEngine) ensureGIPUpstreamAvailable() error {
	if err := engine.ensureGIPConfigured(); err != nil {
		return err
	}
	if engine.usb.Snapshot().EndpointHalt[2] {
		return ErrControllerPersonaEndpointHalted
	}
	return nil
}

func (engine *ControllerPersonaEngine) ensureGIPDownstreamAvailable() error {
	if err := engine.ensureGIPConfigured(); err != nil {
		return err
	}
	if engine.usb.Snapshot().EndpointHalt[1] {
		return ErrControllerPersonaEndpointHalted
	}
	return nil
}

func (engine *ControllerPersonaEngine) nextClaimToken() uint64 {
	engine.nextToken++
	if engine.nextToken == 0 {
		engine.nextToken++
	}
	return engine.nextToken
}

func (engine *ControllerPersonaEngine) makeClaim(
	record controllerPersonaRecord,
) ControllerPersonaClaim {
	token := engine.nextClaimToken()
	engine.hasClaim = true
	engine.claimAdmitted = false
	engine.claimToken = token
	engine.claimRecord = record
	return ControllerPersonaClaim{
		owner: engine, token: token, generation: engine.generation,
		action: record.action, size: record.size, sequence: record.sequence,
		clearEpoch: record.clearEpoch, selectedAtMS: record.selectedAtMS,
		usbResponseKind:         record.usbResponseKind,
		metadataKind:            record.metadataKind,
		metadataAcknowledgement: record.metadataAcknowledgement,
		reliableAcknowledgement: record.reliableAcknowledgement,
		reliableDisposition:     record.reliableDisposition,
		directMotor:             record.directMotor, guideLED: record.guideLED,
		configurationLossClear: record.configurationLossClear,
	}
}

func (engine *ControllerPersonaEngine) validateClaim(claim ControllerPersonaClaim) error {
	record := engine.claimRecord
	if !engine.hasClaim || claim.owner != engine || claim.token == 0 ||
		claim.token != engine.claimToken || claim.generation != engine.generation ||
		claim.action != record.action || claim.size != record.size ||
		claim.sequence != record.sequence || claim.clearEpoch != record.clearEpoch ||
		claim.selectedAtMS != record.selectedAtMS ||
		claim.usbResponseKind != record.usbResponseKind ||
		claim.metadataKind != record.metadataKind ||
		claim.metadataAcknowledgement != record.metadataAcknowledgement ||
		claim.reliableAcknowledgement != record.reliableAcknowledgement ||
		claim.reliableDisposition != record.reliableDisposition ||
		claim.directMotor != record.directMotor || claim.guideLED != record.guideLED ||
		claim.configurationLossClear != record.configurationLossClear {
		return ErrInvalidControllerPersonaClaim
	}
	return nil
}

// SetCurrentInput updates the explicit source snapshot used by the mandatory
// START report. An already selected adapter claim remains immutable.
func (engine *ControllerPersonaEngine) SetCurrentInput(report GamepadInputReportV1) error {
	if err := engine.validateInitialized(); err != nil {
		return err
	}
	if err := engine.metadata.validateInputReport(report); err != nil {
		return err
	}
	engine.currentInput = report
	return nil
}

// SetCurrentStatus updates the explicit non-powering-off status source.
func (engine *ControllerPersonaEngine) SetCurrentStatus(
	status ExtendedStatusNoEventsBodyV1,
) error {
	if err := engine.validateInitialized(); err != nil {
		return err
	}
	if err := status.Validate(); err != nil {
		return err
	}
	if status.PowerLevel != StatusFullPower {
		return ErrInvalidControllerPersonaConfig
	}
	engine.currentStatus = status
	return nil
}

// ClaimUSBControl selects one exact EP0 response/effect from the composed USB
// control plane. The response is materialized only at final admission.
func (engine *ControllerPersonaEngine) ClaimUSBControl(
	setup []byte,
) (ControllerPersonaClaim, error) {
	if err := engine.ensureNewClaimAllowed(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	claim, err := engine.usb.Claim(setup)
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	record := controllerPersonaRecord{
		action: ControllerPersonaUSBControl, size: uint8(claim.ResponseSize()),
		usbClaim: claim, usbOwned: true, selectedAtMS: engine.lastNowMS,
		usbResponseKind:        claim.ResponseKind(),
		configurationLossClear: engine.usb.claimLosesConfiguration(claim),
	}
	return engine.makeClaim(record), nil
}

// ClaimPoll selects a due lifecycle action. Arrival Hello is held until the
// USB data interface is Configured. Reliable ACK timeout is bridged into the
// lifecycle's exact metadata-failure Hello transition.
func (engine *ControllerPersonaEngine) ClaimPoll(
	nowMS uint64,
) (ControllerPersonaClaim, bool, error) {
	if err := engine.validateInitialized(); err != nil {
		return ControllerPersonaClaim{}, false, err
	}
	if engine.configurationLossClearPending {
		claim, err := engine.claimConfigurationLossClear(nowMS)
		return claim, err == nil, err
	}
	if engine.ordinaryFeedbackPending() {
		// Ordinary status remains schedulable, but lifecycle Poll must not
		// mutate a cursor or begin a boundary while local feedback is owned.
		if err := engine.ensureNewOrdinaryUpstreamClaimAllowed(); err != nil {
			return ControllerPersonaClaim{}, false, err
		}
		if err := engine.observeClock(nowMS); err != nil {
			return ControllerPersonaClaim{}, false, err
		}
		if err := engine.validateLifecycleEmissionSources(); err != nil {
			return ControllerPersonaClaim{}, false, err
		}
		return engine.claimPeriodicStatus(nowMS)
	}
	if err := engine.ensureNewClaimAllowed(); err != nil {
		return ControllerPersonaClaim{}, false, err
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaClaim{}, false, err
	}
	if !engine.attached {
		return ControllerPersonaClaim{}, false, ErrControllerPersonaDetached
	}
	if engine.metadataActive {
		if err := engine.metadataTransfer.Poll(nowMS); err != nil {
			if err != ErrReliableTransferTimeout && err != ErrMetadataTransferFaulted {
				return ControllerPersonaClaim{}, false, err
			}
			if err := engine.ensureGIPUpstreamAvailable(); err != nil {
				return ControllerPersonaClaim{}, false, err
			}
			if claimErr := engine.validateLifecycleEmissionSources(); claimErr != nil {
				return ControllerPersonaClaim{}, false, claimErr
			}
			claim, claimErr := engine.lifecycle.ClaimMetadataTransferFailed(
				nowMS, engine.metadataFence)
			if claimErr != nil {
				return ControllerPersonaClaim{}, false, claimErr
			}
			record, claimErr := engine.recordLifecycleAction(claim, nowMS)
			if claimErr != nil {
				return ControllerPersonaClaim{}, false, claimErr
			}
			record.metadataFailureHello = true
			return engine.makeClaim(record), true, nil
		}
	}
	if engine.lifecycle.State() == ControllerLifecycleArrival &&
		engine.usb.Snapshot().State != USBControlDeviceConfigured {
		return ControllerPersonaClaim{}, false, nil
	}
	if engine.lifecycle.State() == ControllerLifecycleArrival {
		if err := engine.ensureGIPUpstreamAvailable(); err != nil {
			return ControllerPersonaClaim{}, false, err
		}
	}
	if err := engine.validateLifecycleEmissionSources(); err != nil {
		return ControllerPersonaClaim{}, false, err
	}
	if claim, present, err := engine.claimPeriodicStatus(nowMS); err != nil || present {
		return claim, present, err
	}
	claim, present, err := engine.lifecycle.ClaimPoll(nowMS)
	if err != nil || !present {
		return ControllerPersonaClaim{}, present, err
	}
	record, err := engine.recordLifecycleAction(claim, nowMS)
	if err != nil {
		return ControllerPersonaClaim{}, false, err
	}
	return engine.makeClaim(record), true, nil
}

// claimConfigurationLossClear transfers the one derived safety obligation
// created by delivered SET_CONFIGURATION(0) into the ordinary immutable local
// claim lane. It is private so the coordinator can require this exact cause;
// direct engine consumers obtain the same action through ClaimPoll.
func (engine *ControllerPersonaEngine) claimConfigurationLossClear(
	nowMS uint64,
) (ControllerPersonaClaim, error) {
	if err := engine.validateInitialized(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.ensureNoOrdinaryFeedback(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if engine.hasClaim {
		return ControllerPersonaClaim{}, ErrControllerPersonaClaimOutstanding
	}
	if engine.retryPending {
		return ControllerPersonaClaim{}, ErrControllerPersonaRetryRequired
	}
	if !engine.configurationLossClearPending {
		return ControllerPersonaClaim{}, ErrControllerPersonaInvariantViolation
	}
	if err := engine.validateInnerIdle(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	record := engine.configurationLossClearRecord
	if record.action != ControllerPersonaClearOutputs || record.size != 0 ||
		record.clearEpoch == 0 || record.clearEpoch != engine.nextClearEpoch ||
		record.usbOwned || record.lifecycleOwned || record.metadataOwned ||
		record.sequencePool != controllerPersonaNoSequence ||
		record.configurationLossClear {
		return ControllerPersonaClaim{}, ErrControllerPersonaInvariantViolation
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaClaim{}, err
	}
	record.selectedAtMS = nowMS
	engine.configurationLossClearPending = false
	engine.configurationLossClearRecord = controllerPersonaRecord{}
	return engine.makeClaim(record), nil
}

// ReceiveHostMessage classifies one exact downstream message and selects every
// resulting effect as an adapter claim. In particular, a Metadata ACK is only
// previewed here: reliable-transfer state cannot advance until the transport
// finally admits and delivers the host's interrupt-OUT acknowledgement.
func (engine *ControllerPersonaEngine) ReceiveHostMessage(
	nowMS uint64,
	wire []byte,
) (ControllerPersonaClaim, ControllerPersonaHostDisposition, error) {
	if err := engine.ensureNewClaimAllowed(); err != nil {
		return ControllerPersonaClaim{}, 0, err
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaClaim{}, 0, err
	}
	if err := engine.ensureGIPDownstreamAvailable(); err != nil {
		return ControllerPersonaClaim{}, 0, err
	}
	message, err := DecodeControllerDownstreamMessage(wire)
	if err != nil {
		return ControllerPersonaClaim{}, 0, err
	}
	return engine.claimDecodedHostMessage(nowMS, message)
}

// claimDecodedHostMessage assumes the caller has serialized the persona,
// observed nowMS, and proved interrupt-OUT availability. Keeping the canonical
// selector behind one helper lets the whole-packet lane reuse it without
// re-encoding typed wire values or creating a second mapping stack.
func (engine *ControllerPersonaEngine) claimDecodedHostMessage(
	nowMS uint64,
	message ControllerDownstreamMessage,
) (ControllerPersonaClaim, ControllerPersonaHostDisposition, error) {
	switch message.Kind {
	case ControllerDownstreamLifecycle:
		// START's first lifecycle obligation is an upstream Current Status
		// message. Prove interrupt IN availability before the lifecycle owns a
		// cursor or Global sequence; an IN Halt must not strand a hidden retry.
		// Other admitted host commands begin with local actions (or are ignored)
		// and remain available under a directional IN Halt.
		if controllerHostCommandStartsWithUpstreamWire(
			engine.lifecycle.State(), message.Lifecycle) {
			if err := engine.ensureGIPUpstreamAvailable(); err != nil {
				return ControllerPersonaClaim{}, 0, err
			}
		}
		if err := engine.validateLifecycleEmissionSources(); err != nil {
			return ControllerPersonaClaim{}, 0, err
		}
		claim, present, err := engine.lifecycle.ClaimHostCommand(nowMS, message.Lifecycle)
		if err != nil {
			return ControllerPersonaClaim{}, 0, err
		}
		if !present {
			return ControllerPersonaClaim{}, ControllerPersonaHostIgnored, nil
		}
		record, err := engine.recordLifecycleAction(claim, nowMS)
		if err != nil {
			return ControllerPersonaClaim{}, 0, err
		}
		return engine.makeClaim(record), ControllerPersonaHostActionClaimed, nil

	case ControllerDownstreamProtocolControlACK:
		if !engine.metadataActive {
			return ControllerPersonaClaim{}, 0, ErrControllerPersonaMetadataNotActive
		}
		body := message.ACK
		if err := body.Validate(); err != nil ||
			body.ReferencedDataClass != DataClassCommand ||
			body.ReferencedMessageNumber != messageNumberMetadataRequest ||
			!body.ReferencedSystem || body.ReferencedExpansionIndex != 0 ||
			body.FragmentOffset > MetadataMaximumBoundLength {
			return ControllerPersonaClaim{}, 0, ErrInvalidAcknowledgement
		}
		ack := ReliableAcknowledgement{
			TransferGeneration:           engine.generation,
			TransferEpoch:                engine.metadataFence.TransferGeneration,
			MessageNumber:                messageNumberMetadataRequest,
			Sequence:                     message.Sequence,
			ContiguousPayloadBytes:       uint16(body.FragmentOffset),
			ReceiverRemainingBufferBytes: body.RemainingBuffer,
		}
		disposition, err := engine.metadataTransfer.PreviewAcknowledgement(ack, nowMS)
		if err != nil {
			return ControllerPersonaClaim{}, 0, err
		}
		record := controllerPersonaRecord{
			action:   ControllerPersonaApplyMetadataAcknowledgement,
			sequence: message.Sequence, selectedAtMS: nowMS,
			reliableAcknowledgement: ack, reliableDisposition: disposition,
		}
		claim := engine.makeClaim(record)
		switch disposition {
		case ReliableAcknowledgementProgress:
			return claim, ControllerPersonaHostAcknowledgementProgress, nil
		case ReliableAcknowledgementRewind:
			return claim, ControllerPersonaHostAcknowledgementRewind, nil
		case ReliableAcknowledgementDuplicate:
			return claim, ControllerPersonaHostAcknowledgementDuplicate, nil
		default:
			return ControllerPersonaClaim{}, 0, ErrInvalidAcknowledgement
		}

	case ControllerDownstreamDirectMotor:
		record := controllerPersonaRecord{
			action:   ControllerPersonaApplyDirectMotor,
			sequence: message.Sequence, directMotor: message.DirectMotor,
			selectedAtMS: nowMS,
		}
		return engine.makeClaim(record), ControllerPersonaHostActionClaimed, nil

	case ControllerDownstreamGuideLED:
		record := controllerPersonaRecord{
			action:   ControllerPersonaApplyGuideLED,
			sequence: message.Sequence, guideLED: message.GuideLED,
			selectedAtMS: nowMS,
		}
		return engine.makeClaim(record), ControllerPersonaHostActionClaimed, nil
	}
	return ControllerPersonaClaim{}, 0, ErrUnsupportedControllerPersonaHostMessage
}

func controllerHostCommandStartsWithUpstreamWire(
	state ControllerLifecycleState,
	command ControllerHostCommand,
) bool {
	return command.Kind == ControllerHostCommandSetDeviceState &&
		command.State == SetDeviceStateStart &&
		(state == ControllerLifecycleArrival || state == ControllerLifecycleIdle)
}

// ClaimNextLifecycleAction selects the exact next cursor in a pending
// lifecycle transition.
func (engine *ControllerPersonaEngine) ClaimNextLifecycleAction(
	nowMS uint64,
) (ControllerPersonaClaim, error) {
	if err := engine.ensureNewClaimAllowed(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.validateLifecycleEmissionSources(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	current := engine.lifecycle.Snapshot().CurrentAction
	if controllerLifecycleActionRequiresGIPConfiguration(current) {
		if err := engine.ensureGIPUpstreamAvailable(); err != nil {
			return ControllerPersonaClaim{}, err
		}
	}
	claim, err := engine.lifecycle.ClaimNextAction(nowMS)
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	record, err := engine.recordLifecycleAction(claim, nowMS)
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	return engine.makeClaim(record), nil
}

func controllerLifecycleActionRequiresGIPConfiguration(
	action ControllerLifecycleAction,
) bool {
	switch action {
	case ControllerLifecycleSendHello,
		ControllerLifecycleSendCurrentStatus,
		ControllerLifecycleSendInitialInput,
		ControllerLifecycleSendPoweringOffStatus:
		return true
	default:
		return false
	}
}

// ClaimMetadataPacket selects one reliable metadata packet. The first packet
// reserves a Global sequence value; only its admitted delivered resolution
// commits that sequence. All fragments and Metadata Complete reuse it.
func (engine *ControllerPersonaEngine) ClaimMetadataPacket(
	nowMS uint64,
) (ControllerPersonaClaim, error) {
	if err := engine.ensureNewClaimAllowed(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.ensureGIPUpstreamAvailable(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	fence, ok := engine.lifecycle.MetadataTransferFence()
	if !ok {
		return ControllerPersonaClaim{}, ErrControllerPersonaMetadataNotActive
	}
	var sequenceClaim SequenceClaim
	first := false
	if !engine.metadataActive || engine.metadataFence != fence {
		sequence, err := previewControllerPersonaSequence(&engine.global)
		if err != nil {
			return ControllerPersonaClaim{}, err
		}
		transfer, err := engine.profile.NewMetadataTransfer(
			engine.metadata, sequence, engine.generation,
			fence.TransferGeneration, nowMS)
		if err != nil {
			return ControllerPersonaClaim{}, err
		}
		// Prove the first packet is selectable before reserving the Global
		// sequence. Claim below sees the same serialized clock/state.
		if _, err := transfer.selectPacket(nowMS); err != nil {
			return ControllerPersonaClaim{}, err
		}
		sequenceClaim, err = engine.global.Claim()
		if err != nil {
			return ControllerPersonaClaim{}, err
		}
		engine.metadataTransfer = transfer
		engine.metadataFence = fence
		engine.metadataActive = true
		first = true
	}
	claim, err := engine.metadataTransfer.Claim(nowMS)
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	record := controllerPersonaRecord{
		action: ControllerPersonaSendMetadata, size: uint8(claim.Size()),
		sequence: claim.Sequence(), selectedAtMS: nowMS,
		metadataClaim: claim, metadataOwned: true,
		metadataKind:            claim.Kind(),
		metadataAcknowledgement: claim.AcknowledgementRequested(),
	}
	if first {
		record.sequencePool = controllerPersonaGlobalSequence
		record.sequenceClaim = sequenceClaim
	}
	return engine.makeClaim(record), nil
}

// ClaimInput selects one exact ordinary Gamepad Input message. The caller may
// update CurrentInput independently; the selected bytes remain immutable.
func (engine *ControllerPersonaEngine) ClaimInput(
	nowMS uint64,
	report GamepadInputReportV1,
) (ControllerPersonaClaim, error) {
	if err := engine.ensureNewOrdinaryUpstreamClaimAllowed(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.ensureOrdinaryInputAvailable(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.metadata.validateInputReport(report); err != nil {
		return ControllerPersonaClaim{}, err
	}
	sequence, err := previewControllerPersonaSequence(&engine.input)
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	messageSize, err := engine.metadata.inputMessageSize()
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	record := controllerPersonaRecord{
		action: ControllerPersonaSendInput, size: uint8(messageSize),
		sequence: sequence, selectedAtMS: nowMS,
	}
	if err := engine.metadata.encodeInputMessageInto(
		record.wire[:messageSize], record.sequence, report); err != nil {
		return ControllerPersonaClaim{}, err
	}
	sequenceClaim, err := engine.input.Claim()
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	record.sequencePool = controllerPersonaInputSequence
	record.sequenceClaim = sequenceClaim
	return engine.makeClaim(record), nil
}

// ensureOrdinaryInputAvailable is shared with the retained semantic selector:
// a gated host poll must not consume or activate an ordinary input journal.
func (engine *ControllerPersonaEngine) ensureOrdinaryInputAvailable() error {
	if err := engine.ensureGIPUpstreamAvailable(); err != nil {
		return err
	}
	if engine.lifecycle.State() != ControllerLifecycleActive || !engine.normalUpstream ||
		engine.lifecycle.Snapshot().PendingTransition {
		return ErrControllerPersonaUpstreamGated
	}
	return nil
}

// hasLiveInputBaseline also covers the interval after START's initial image
// delivered but before its local Permit action completed. A control boundary
// in that interval must retain following transitions, though IN remains gated.
func (engine *ControllerPersonaEngine) hasLiveInputBaseline() bool {
	if engine.ensureGIPUpstreamAvailable() != nil {
		return false
	}
	lifecycle := engine.lifecycle.Snapshot()
	return (lifecycle.PendingTransition && lifecycle.CurrentAction == ControllerLifecyclePermitNormalUpstream) ||
		engine.ensureOrdinaryInputAvailable() == nil
}

// ClaimGuideButtonStatus selects one exact ordered Guide transition. Guide is
// intentionally not sampled from CurrentInput: a latest-only cell cannot
// preserve a press and release which both arrive between host polls.
func (engine *ControllerPersonaEngine) ClaimGuideButtonStatus(
	nowMS uint64,
	status GuideButtonStatusV1,
) (ControllerPersonaClaim, error) {
	if err := engine.ensureNewOrdinaryUpstreamClaimAllowed(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.ensureGIPUpstreamAvailable(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if engine.lifecycle.State() != ControllerLifecycleActive || !engine.normalUpstream ||
		engine.lifecycle.Snapshot().PendingTransition {
		return ControllerPersonaClaim{}, ErrControllerPersonaUpstreamGated
	}
	record := controllerPersonaRecord{
		action: ControllerPersonaSendGuideButtonStatus, selectedAtMS: nowMS,
	}
	record, err := engine.recordGlobalWire(
		record, GuideButtonStatusMessageSize,
		func(dst []byte, sequence uint8) error {
			return EncodeGuideButtonStatusMessageInto(dst, sequence, status)
		})
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	return engine.makeClaim(record), nil
}

func (engine *ControllerPersonaEngine) recordLifecycleAction(
	claim ControllerLifecycleClaim,
	nowMS uint64,
) (controllerPersonaRecord, error) {
	record := controllerPersonaRecord{
		lifecycleClaim: claim, lifecycleOwned: true, selectedAtMS: nowMS,
	}
	switch claim.Action() {
	case ControllerLifecycleSendHello:
		record.action = ControllerPersonaSendHello
		return engine.recordGlobalWire(record, HelloMessageSize,
			func(dst []byte, sequence uint8) error {
				return engine.profile.EncodeHelloMessageInto(dst, sequence)
			})
	case ControllerLifecycleBeginMetadata:
		record.action = ControllerPersonaBeginMetadata
	case ControllerLifecycleSendCurrentStatus:
		record.action = ControllerPersonaSendCurrentStatus
		return engine.recordStatusWire(record, engine.currentStatus)
	case ControllerLifecycleSendInitialInput:
		record.action = ControllerPersonaSendInitialInput
		sequence, err := previewControllerPersonaSequence(&engine.input)
		if err != nil {
			return controllerPersonaRecord{}, err
		}
		messageSize, err := engine.metadata.inputMessageSize()
		if err != nil {
			return controllerPersonaRecord{}, err
		}
		record.size = uint8(messageSize)
		record.sequence = sequence
		if err := engine.metadata.encodeInputMessageInto(
			record.wire[:messageSize], record.sequence,
			engine.currentInput); err != nil {
			return controllerPersonaRecord{}, err
		}
		sequenceClaim, err := engine.input.Claim()
		if err != nil {
			return controllerPersonaRecord{}, err
		}
		record.sequencePool = controllerPersonaInputSequence
		record.sequenceClaim = sequenceClaim
	case ControllerLifecyclePermitNormalUpstream:
		record.action = ControllerPersonaPermitNormalUpstream
	case ControllerLifecycleGateNormalUpstream:
		record.action = ControllerPersonaGateNormalUpstream
	case ControllerLifecycleClearOutputs:
		record.action = ControllerPersonaClearOutputs
		clearEpoch, err := engine.allocateClearEpoch()
		if err != nil {
			return controllerPersonaRecord{}, err
		}
		record.clearEpoch = clearEpoch
	case ControllerLifecycleSendPoweringOffStatus:
		record.action = ControllerPersonaSendPoweringOffStatus
		return engine.recordStatusWire(record, engine.poweringOffStatus)
	case ControllerLifecycleCompletePowerOff:
		record.action = ControllerPersonaCompletePowerOff
	case ControllerLifecyclePerformReset:
		record.action = ControllerPersonaPerformReset
	default:
		return controllerPersonaRecord{}, ErrInvalidControllerPersonaConfig
	}
	return record, nil
}

func (engine *ControllerPersonaEngine) recordGlobalWire(
	record controllerPersonaRecord,
	size int,
	encode func([]byte, uint8) error,
) (controllerPersonaRecord, error) {
	sequence, err := previewControllerPersonaSequence(&engine.global)
	if err != nil {
		return controllerPersonaRecord{}, err
	}
	record.size = uint8(size)
	record.sequence = sequence
	if err := encode(record.wire[:size], record.sequence); err != nil {
		return controllerPersonaRecord{}, err
	}
	claim, err := engine.global.Claim()
	if err != nil {
		return controllerPersonaRecord{}, err
	}
	record.sequencePool = controllerPersonaGlobalSequence
	record.sequenceClaim = claim
	return record, nil
}

func (engine *ControllerPersonaEngine) validateClearEpochAvailable() error {
	if engine.nextClearEpoch == ^uint64(0) {
		return ErrInvalidTransferEpoch
	}
	return nil
}

func (engine *ControllerPersonaEngine) allocateClearEpoch() (uint64, error) {
	if err := engine.validateClearEpochAvailable(); err != nil {
		return 0, err
	}
	engine.nextClearEpoch++
	return engine.nextClearEpoch, nil
}

// ClaimRetry reclaims only the exact immutable non-delivered action. Wire
// bytes, sequence, metadata packet identity, and clear epoch cannot change.
func (engine *ControllerPersonaEngine) ClaimRetry(
	nowMS uint64,
) (ControllerPersonaClaim, error) {
	if err := engine.validateInitialized(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if engine.hasClaim {
		return ControllerPersonaClaim{}, ErrControllerPersonaClaimOutstanding
	}
	if engine.hostPacketRetryPending {
		return ControllerPersonaClaim{}, ErrControllerPersonaHostPacketRetryRequired
	}
	if !engine.retryPending {
		return ControllerPersonaClaim{}, ErrControllerPersonaRetryRequired
	}
	if engine.ordinaryFeedbackPending() && !isOrdinaryUpstreamRecord(engine.retryRecord) {
		return ControllerPersonaClaim{}, ErrControllerPersonaBoundaryBlocked
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaClaim{}, err
	}
	record := engine.retryRecord
	if record.action == ControllerPersonaApplyMetadataAcknowledgement {
		_, previewErr := engine.metadataTransfer.PreviewAcknowledgement(
			record.reliableAcknowledgement, nowMS)
		switch previewErr {
		case ErrReliableTransferTimeout:
			// Poll owns the actual timeout transition. Retire the adapter retry
			// only after the reliable transfer has faulted, so ClaimPoll can
			// select the ordinary metadata-failure Hello instead of wedging on
			// an ACK which can never pass final admission again.
			pollErr := engine.metadataTransfer.Poll(nowMS)
			if pollErr != ErrReliableTransferTimeout {
				return ControllerPersonaClaim{}, pollErr
			}
			engine.retryPending = false
			engine.retryRecord = controllerPersonaRecord{}
			return ControllerPersonaClaim{}, ErrReliableTransferTimeout
		case ErrMetadataTransferFaulted:
			engine.retryPending = false
			engine.retryRecord = controllerPersonaRecord{}
			return ControllerPersonaClaim{}, ErrMetadataTransferFaulted
		case nil:
			// Continue through the ordinary immutable retry path.
		default:
			return ControllerPersonaClaim{}, previewErr
		}
	}
	if err := engine.validateRetryPreconditions(record, nowMS); err != nil {
		return ControllerPersonaClaim{}, err
	}
	record.selectedAtMS = nowMS
	record.reliableAdmittedAtMS = 0
	record.reliableAdmitted = false
	if record.lifecycleOwned {
		claim, err := engine.lifecycle.ClaimRetry(nowMS)
		if err != nil {
			return ControllerPersonaClaim{}, err
		}
		record.lifecycleClaim = claim
	}
	if record.metadataOwned {
		claim, err := engine.metadataTransfer.ClaimRetry(nowMS)
		if err != nil {
			return ControllerPersonaClaim{}, err
		}
		if claim.Size() != int(record.size) || claim.Sequence() != record.sequence {
			return ControllerPersonaClaim{}, ErrInvalidControllerPersonaClaim
		}
		record.metadataClaim = claim
	}
	if record.sequencePool != controllerPersonaNoSequence {
		counter := engine.sequenceCounter(record.sequencePool)
		claim, err := counter.ClaimRetry()
		if err != nil {
			return ControllerPersonaClaim{}, err
		}
		if claim.Value() != record.sequence {
			return ControllerPersonaClaim{}, ErrInvalidControllerPersonaClaim
		}
		record.sequenceClaim = claim
	}
	engine.retryPending = false
	engine.retryRecord = controllerPersonaRecord{}
	return engine.makeClaim(record), nil
}

func (engine *ControllerPersonaEngine) validateRetryPreconditions(
	record controllerPersonaRecord,
	nowMS uint64,
) error {
	if record.lifecycleOwned {
		snapshot := engine.lifecycle.Snapshot()
		if !snapshot.PendingTransition || snapshot.ClaimOutstanding ||
			!snapshot.RetryPending ||
			snapshot.CurrentAction != record.lifecycleClaim.Action() {
			return ErrLifecycleRetryRequired
		}
		if err := engine.lifecycle.validateClock(nowMS); err != nil {
			return err
		}
	}
	if record.metadataOwned {
		if err := engine.metadataTransfer.Poll(nowMS); err != nil {
			return err
		}
		if engine.metadataTransfer.hasClaim ||
			!engine.metadataTransfer.retryPending ||
			engine.metadataTransfer.retryRecord.size != record.size ||
			engine.metadataTransfer.sequence != record.sequence {
			return ErrMetadataRetryRequired
		}
	}
	if record.sequencePool != controllerPersonaNoSequence {
		counter := engine.sequenceCounter(record.sequencePool)
		if counter.hasClaim || !counter.retryPending ||
			counter.retryValue != record.sequence {
			return ErrSequenceRetryRequired
		}
	}
	return nil
}

func (engine *ControllerPersonaEngine) sequenceCounter(
	pool controllerPersonaSequencePool,
) *SequenceCounter {
	if pool == controllerPersonaInputSequence {
		return &engine.input
	}
	return &engine.global
}

func (engine *ControllerPersonaEngine) validateResolutionPreconditions(
	record controllerPersonaRecord,
	outcome ControllerPersonaOutcome,
) error {
	requiresAdmission := outcome == ControllerPersonaDelivered ||
		outcome == ControllerPersonaExecutionCancelled
	if record.usbOwned {
		if err := engine.usb.validateClaim(record.usbClaim); err != nil {
			return err
		}
		if requiresAdmission && !engine.usb.claimAdmitted {
			return ErrUSBControlClaimNotAdmitted
		}
	}
	if record.metadataOwned {
		if err := engine.metadataTransfer.validateClaim(record.metadataClaim); err != nil {
			return err
		}
		if requiresAdmission && !engine.metadataTransfer.claimAdmitted {
			return ErrMetadataClaimNotAdmitted
		}
	}
	if record.lifecycleOwned {
		if err := engine.lifecycle.validateClaim(record.lifecycleClaim); err != nil {
			return err
		}
		if requiresAdmission && !engine.lifecycle.claimAdmitted {
			return ErrLifecycleClaimNotAdmitted
		}
	}
	if record.sequencePool != controllerPersonaNoSequence {
		counter := engine.sequenceCounter(record.sequencePool)
		if err := counter.validateClaim(record.sequenceClaim); err != nil {
			return err
		}
		if requiresAdmission && !counter.claimAdmitted {
			return ErrSequenceClaimNotAdmitted
		}
	}
	if outcome == ControllerPersonaDelivered &&
		record.action == ControllerPersonaSendMetadata &&
		(record.metadataKind == MetadataPacketSingle ||
			record.metadataKind == MetadataPacketComplete) {
		fence, ok := engine.lifecycle.MetadataTransferFence()
		if !ok || fence != engine.metadataFence {
			return ErrInvalidTransferGeneration
		}
	}
	if outcome == ControllerPersonaDelivered &&
		(record.action == ControllerPersonaPerformReset ||
			record.action == ControllerPersonaCompletePowerOff) {
		if err := engine.validateTransportAdvance(false); err != nil {
			return err
		}
	}
	if outcome == ControllerPersonaDelivered &&
		record.action == ControllerPersonaApplyMetadataAcknowledgement {
		if !record.reliableAdmitted {
			return ErrControllerPersonaClaimNotAdmitted
		}
		disposition, err := engine.metadataTransfer.PreviewAcknowledgement(
			record.reliableAcknowledgement, record.reliableAdmittedAtMS)
		if err != nil {
			return err
		}
		if disposition != record.reliableDisposition {
			return ErrInvalidAcknowledgement
		}
	}
	if outcome == ControllerPersonaDelivered && record.configurationLossClear {
		if record.action != ControllerPersonaUSBControl || !record.usbOwned ||
			engine.configurationLossClearPending {
			return ErrControllerPersonaInvariantViolation
		}
		if err := engine.validateClearEpochAvailable(); err != nil {
			return err
		}
	}
	return nil
}

// AdmitAndCopy performs every inner final fence and copies exactly one private
// wire image. Local actions require an empty destination. Short and oversized
// destinations fail before any inner admission.
func (engine *ControllerPersonaEngine) AdmitAndCopy(
	claim ControllerPersonaClaim,
	dst []byte,
	nowMS uint64,
) error {
	if err := engine.validateClaim(claim); err != nil {
		return err
	}
	if engine.claimAdmitted {
		return ErrInvalidControllerPersonaClaim
	}
	if len(dst) != int(engine.claimRecord.size) {
		return fmt.Errorf("%w: got=%d want=%d",
			ErrInvalidControllerPersonaDestination, len(dst), engine.claimRecord.size)
	}
	if err := engine.observeClock(nowMS); err != nil {
		return err
	}
	record := &engine.claimRecord
	if record.action == ControllerPersonaApplyMetadataAcknowledgement {
		disposition, err := engine.metadataTransfer.PreviewAcknowledgement(
			record.reliableAcknowledgement, nowMS)
		if err != nil {
			return err
		}
		if disposition != record.reliableDisposition {
			return ErrInvalidAcknowledgement
		}
		record.reliableAdmittedAtMS = nowMS
		record.reliableAdmitted = true
	}
	if record.metadataOwned {
		if err := engine.metadataTransfer.Poll(nowMS); err != nil {
			return err
		}
		if err := engine.metadataTransfer.validateClaim(record.metadataClaim); err != nil ||
			engine.metadataTransfer.claimAdmitted {
			if err != nil {
				return err
			}
			return ErrInvalidMetadataClaim
		}
	}
	if record.lifecycleOwned {
		if err := engine.lifecycle.validateClaim(record.lifecycleClaim); err != nil {
			return err
		}
		if err := engine.lifecycle.validateClock(nowMS); err != nil {
			return err
		}
	}
	if record.sequencePool != controllerPersonaNoSequence {
		counter := engine.sequenceCounter(record.sequencePool)
		if err := counter.validateClaim(record.sequenceClaim); err != nil ||
			counter.claimAdmitted {
			if err != nil {
				return err
			}
			return ErrInvalidSequenceClaim
		}
	}
	if record.action == ControllerPersonaSendMetadata &&
		(record.metadataKind == MetadataPacketSingle ||
			record.metadataKind == MetadataPacketComplete) {
		fence, ok := engine.lifecycle.MetadataTransferFence()
		if !ok || fence != engine.metadataFence {
			return ErrInvalidTransferGeneration
		}
	}
	if record.action == ControllerPersonaPerformReset ||
		record.action == ControllerPersonaCompletePowerOff {
		// These local actions hand transport teardown/reset to the adapter.
		// Prove every composed generation fence before that effect can start;
		// Resolve must not discover an overflow or split-brain generation after
		// the external boundary has already become visible.
		if err := engine.validateTransportAdvance(false); err != nil {
			return err
		}
	}
	if record.configurationLossClear {
		if record.action != ControllerPersonaUSBControl || !record.usbOwned {
			return ErrControllerPersonaInvariantViolation
		}
		// Fence epoch exhaustion before the EP0 response/status is allowed to
		// become externally visible. Serialized ownership guarantees the value
		// cannot change before terminal completion.
		if err := engine.validateClearEpochAvailable(); err != nil {
			return err
		}
	}

	if record.usbOwned {
		if err := engine.usb.AdmitAndCopy(record.usbClaim, dst); err != nil {
			return err
		}
	} else {
		if record.lifecycleOwned {
			if ok, err := engine.lifecycle.Admit(record.lifecycleClaim, nowMS); err != nil || !ok {
				if err != nil {
					return err
				}
				return ErrInvalidLifecycleClaim
			}
		}
		if record.sequencePool != controllerPersonaNoSequence {
			if ok, err := engine.sequenceCounter(record.sequencePool).CanAdmit(
				record.sequenceClaim); err != nil || !ok {
				if err != nil {
					return err
				}
				return ErrInvalidSequenceClaim
			}
		}
		if record.metadataOwned {
			if _, err := engine.metadataTransfer.AdmitAndCopy(
				record.metadataClaim, dst, nowMS); err != nil {
				return err
			}
		} else {
			copy(dst, record.wire[:record.size])
		}
	}
	engine.claimAdmitted = true
	return nil
}

// Resolve terminally advances one composed action. Non-delivery retains an
// exact mandatory retry except for EP0, whose host owns request retry. A valid
// admitted delivery is designed to be non-failing after the external effect.
func (engine *ControllerPersonaEngine) Resolve(
	claim ControllerPersonaClaim,
	outcome ControllerPersonaOutcome,
	completedMS uint64,
) error {
	if err := engine.validateClaim(claim); err != nil {
		return err
	}
	if outcome < ControllerPersonaDelivered ||
		outcome > ControllerPersonaExecutionCancelled {
		return ErrInvalidControllerPersonaOutcome
	}
	if (outcome == ControllerPersonaDelivered ||
		outcome == ControllerPersonaExecutionCancelled) && !engine.claimAdmitted {
		return ErrControllerPersonaClaimNotAdmitted
	}
	if completedMS < engine.lastNowMS {
		completedMS = engine.lastNowMS
	}
	record := engine.claimRecord
	delivered := outcome == ControllerPersonaDelivered
	if err := engine.validateResolutionPreconditions(record, outcome); err != nil {
		return err
	}
	engine.lastNowMS = completedMS

	if record.usbOwned {
		usbOutcome := USBControlDeliveryFailed
		if delivered {
			usbOutcome = USBControlDelivered
		} else if outcome == ControllerPersonaExecutionCancelled {
			usbOutcome = USBControlExecutionCancelled
		}
		if err := engine.usb.Resolve(record.usbClaim, usbOutcome); err != nil {
			return err
		}
	}
	if record.metadataOwned {
		metadataOutcome := MetadataPacketDeliveryFailed
		if delivered {
			metadataOutcome = MetadataPacketDelivered
		} else if outcome == ControllerPersonaDeferred {
			metadataOutcome = MetadataPacketDeferred
		}
		if err := engine.metadataTransfer.Resolve(
			record.metadataClaim, metadataOutcome, completedMS); err != nil {
			return err
		}
	}
	if record.lifecycleOwned {
		lifecycleOutcome := ControllerLifecycleDeliveryFailed
		if delivered {
			lifecycleOutcome = ControllerLifecycleDelivered
		} else if outcome == ControllerPersonaDeferred {
			lifecycleOutcome = ControllerLifecycleDeferred
		} else if outcome == ControllerPersonaExecutionCancelled {
			lifecycleOutcome = ControllerLifecycleExecutionCancelled
		}
		if err := engine.lifecycle.Resolve(
			record.lifecycleClaim, lifecycleOutcome, completedMS); err != nil {
			return err
		}
	}
	if record.sequencePool != controllerPersonaNoSequence {
		sequenceOutcome := SequenceDeliveryFailed
		if delivered {
			sequenceOutcome = SequenceDelivered
		} else if outcome == ControllerPersonaDeferred {
			sequenceOutcome = SequenceDeferred
		}
		if err := engine.sequenceCounter(record.sequencePool).Resolve(
			record.sequenceClaim, sequenceOutcome); err != nil {
			return err
		}
	}

	if !delivered {
		engine.retryPending = !record.usbOwned
		if engine.retryPending {
			engine.retryRecord = record
		}
		engine.clearClaim()
		return nil
	}

	if err := engine.applyDelivered(record, completedMS); err != nil {
		return err
	}
	engine.clearClaim()
	if record.configurationLossClear {
		// Epoch availability was fenced before final admission and again before
		// USB state commit. No fallible work remains after the host-visible EP0
		// completion: reserve exactly one logical output release now.
		engine.nextClearEpoch++
		engine.configurationLossClearPending = true
		engine.configurationLossClearRecord = controllerPersonaRecord{
			action: ControllerPersonaClearOutputs, clearEpoch: engine.nextClearEpoch,
			selectedAtMS: completedMS,
		}
	}
	return nil
}

func (engine *ControllerPersonaEngine) applyDelivered(
	record controllerPersonaRecord,
	completedMS uint64,
) error {
	switch record.action {
	case ControllerPersonaSendHello:
		if record.metadataFailureHello {
			engine.metadataActive = false
			engine.metadataTransfer = MetadataTransfer{}
			engine.metadataFence = ControllerMetadataTransferFence{}
		}
	case ControllerPersonaBeginMetadata:
		if fence, ok := engine.lifecycle.MetadataTransferFence(); ok &&
			fence != engine.metadataFence {
			engine.metadataActive = false
			engine.metadataTransfer = MetadataTransfer{}
		}
	case ControllerPersonaPermitNormalUpstream:
		engine.normalUpstream = true
	case ControllerPersonaGateNormalUpstream:
		engine.normalUpstream = false
		engine.statusTimer = controllerPersonaStatusTimer{}
	case ControllerPersonaSendCurrentStatus:
		engine.statusTimer.delivered(completedMS, record.lifecycleOwned)
	case ControllerPersonaClearOutputs:
		engine.directMotor = NewStopRumbleBody()
		engine.guideLED = GuideLEDCommandV1{Pattern: GuideLEDPatternOff}
		engine.lastClearEpoch = record.clearEpoch
	case ControllerPersonaApplyMetadataAcknowledgement:
		disposition, err := engine.metadataTransfer.Acknowledge(
			record.reliableAcknowledgement, record.reliableAdmittedAtMS)
		if err != nil {
			return err
		}
		if disposition != record.reliableDisposition {
			return ErrInvalidAcknowledgement
		}
	case ControllerPersonaApplyDirectMotor:
		engine.directMotor = record.directMotor
	case ControllerPersonaApplyGuideLED:
		engine.guideLED = record.guideLED
	case ControllerPersonaCompletePowerOff:
		return engine.advanceTransportBoundary(false, true, completedMS)
	case ControllerPersonaPerformReset:
		return engine.advanceTransportBoundary(true, true, completedMS)
	case ControllerPersonaSendMetadata:
		if engine.metadataTransfer.Done() {
			if err := engine.lifecycle.MetadataTransferSucceeded(
				completedMS, engine.metadataFence); err != nil {
				return err
			}
			engine.metadataActive = false
			engine.metadataTransfer = MetadataTransfer{}
		}
	}
	return nil
}

func (engine *ControllerPersonaEngine) clearClaim() {
	engine.hasClaim = false
	engine.claimAdmitted = false
	engine.claimToken = 0
	engine.claimRecord = controllerPersonaRecord{}
}

// BeginUSBReset advances the authoritative transport generation immediately,
// retires all predecessor protocol work, and returns exactly one logical
// clear-output action. Arrival Hello becomes eligible only after EP0 selects
// the configuration in the successor generation.
func (engine *ControllerPersonaEngine) BeginUSBReset(
	nowMS uint64,
) (ControllerPersonaClaim, error) {
	if err := engine.validateClearEpochAvailable(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.validateTransportGenerationAlignment(false); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.ensureBoundaryAllowed(nowMS, true, true); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.advanceTransportBoundary(true, false, nowMS); err != nil {
		return ControllerPersonaClaim{}, err
	}
	record := controllerPersonaRecord{
		action: ControllerPersonaClearOutputs, selectedAtMS: nowMS,
	}
	clearEpoch, err := engine.allocateClearEpoch()
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	record.clearEpoch = clearEpoch
	return engine.makeClaim(record), nil
}

// BeginDisconnect advances to a detached generation and returns exactly one
// logical clear-output action. Reconnect is blocked until that release is
// delivered or its exact retry is resolved.
func (engine *ControllerPersonaEngine) BeginDisconnect(
	nowMS uint64,
) (ControllerPersonaClaim, error) {
	if err := engine.validateClearEpochAvailable(); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.validateTransportGenerationAlignment(false); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.ensureBoundaryAllowed(nowMS, true, true); err != nil {
		return ControllerPersonaClaim{}, err
	}
	if err := engine.advanceTransportBoundary(false, false, nowMS); err != nil {
		return ControllerPersonaClaim{}, err
	}
	record := controllerPersonaRecord{
		action: ControllerPersonaClearOutputs, selectedAtMS: nowMS,
	}
	clearEpoch, err := engine.allocateClearEpoch()
	if err != nil {
		return ControllerPersonaClaim{}, err
	}
	record.clearEpoch = clearEpoch
	return engine.makeClaim(record), nil
}

// Reconnect advances from Detached to USB Default under a fresh generation.
// No second clear is manufactured; the disconnect release must already have
// reached terminal delivered resolution.
func (engine *ControllerPersonaEngine) Reconnect(nowMS uint64) error {
	if err := engine.ensureBoundaryAllowed(nowMS, false, false); err != nil {
		return err
	}
	if engine.attached {
		return ErrInvalidUSBControlTransition
	}
	return engine.advanceReconnect(nowMS)
}

// adoptRetainedUSBIPAddress is the canonical persona-side half of the USB/IP
// import boundary. The vhci host assigns a real USB address without emitting a
// SET_ADDRESS URB, so only an otherwise-idle attached Default-state persona may
// adopt the transport-owned addressed state.
func (engine *ControllerPersonaEngine) adoptRetainedUSBIPAddress() error {
	if err := engine.validateInitialized(); err != nil {
		return err
	}
	if err := engine.validateTransportGenerationAlignment(false); err != nil {
		return err
	}
	if !engine.attached || engine.hasClaim || engine.retryPending || engine.ordinaryFeedbackPending() ||
		engine.hostPacketActive || engine.hostPacketRetryPending ||
		engine.hostPacketQuarantined || engine.configurationLossClearPending {
		return ErrControllerPersonaBoundaryBlocked
	}
	return engine.usb.adoptRetainedUSBIPAddress()
}

func (engine *ControllerPersonaEngine) ensureBoundaryAllowed(
	nowMS uint64,
	requireAttached bool,
	retirePredecessor bool,
) error {
	if err := engine.validateInitialized(); err != nil {
		return err
	}
	if engine.hostPacketActive || engine.hostPacketRetryPending ||
		engine.hostPacketQuarantined {
		return ErrControllerPersonaBoundaryBlocked
	}
	if engine.hasClaim && engine.claimAdmitted {
		return ErrControllerPersonaBoundaryBlocked
	}
	if engine.ordinaryFeedback.state == controllerPersonaOrdinaryFeedbackAdmitted ||
		(engine.ordinaryFeedbackRetryPending() && !retirePredecessor) {
		return ErrControllerPersonaBoundaryBlocked
	}
	if engine.configurationLossClearPending {
		return ErrControllerPersonaBoundaryBlocked
	}
	if (engine.hasClaim &&
		engine.claimRecord.action == ControllerPersonaClearOutputs) ||
		(engine.retryPending &&
			engine.retryRecord.action == ControllerPersonaClearOutputs) {
		// An output release is a safety obligation, not disposable predecessor
		// presentation. Authoritative reset/disconnect must not replace its exact
		// epoch with a successor-generation clear before delivery.
		return ErrControllerPersonaBoundaryBlocked
	}
	if (engine.hasClaim || engine.retryPending) && !retirePredecessor {
		return ErrControllerPersonaBoundaryBlocked
	}
	if requireAttached && !engine.attached {
		return ErrControllerPersonaDetached
	}
	if err := engine.observeClock(nowMS); err != nil {
		return err
	}
	if engine.hasClaim {
		if err := engine.retireUnadmittedClaim(nowMS); err != nil {
			return err
		}
	}
	if retirePredecessor {
		engine.retryPending = false
		engine.retryRecord = controllerPersonaRecord{}
	}
	return nil
}

func (engine *ControllerPersonaEngine) retireUnadmittedClaim(nowMS uint64) error {
	record := engine.claimRecord
	if err := engine.validateResolutionPreconditions(
		record, ControllerPersonaDeliveryFailed); err != nil {
		return err
	}
	if record.usbOwned {
		if err := engine.usb.Resolve(record.usbClaim, USBControlDeliveryFailed); err != nil {
			return err
		}
	}
	if record.metadataOwned {
		if err := engine.metadataTransfer.Resolve(
			record.metadataClaim, MetadataPacketDeliveryFailed, nowMS); err != nil {
			return err
		}
	}
	if record.lifecycleOwned {
		if err := engine.lifecycle.Resolve(
			record.lifecycleClaim, ControllerLifecycleDeliveryFailed, nowMS); err != nil {
			return err
		}
	}
	if record.sequencePool != controllerPersonaNoSequence {
		if err := engine.sequenceCounter(record.sequencePool).Resolve(
			record.sequenceClaim, SequenceDeliveryFailed); err != nil {
			return err
		}
	}
	engine.clearClaim()
	return nil
}

func (engine *ControllerPersonaEngine) advanceTransportBoundary(
	reset bool,
	clearAlreadyDelivered bool,
	nowMS uint64,
) error {
	if err := engine.validateTransportAdvance(false); err != nil {
		return err
	}
	successor := engine.generation + 1
	var err error
	if reset {
		err = engine.usb.Reset(successor)
	} else {
		err = engine.usb.Disconnect(successor)
	}
	if err != nil {
		return err
	}
	if err := engine.global.Reset(successor); err != nil {
		return err
	}
	if err := engine.input.Reset(successor); err != nil {
		return err
	}
	engine.generation = successor
	engine.attached = reset
	// Only the authoritative drained reset/disconnect path can reach this
	// point with a non-delivered ordinary feedback retry. An admitted side
	// owner is rejected before any transport generation changes.
	engine.ordinaryFeedback = controllerPersonaOrdinaryFeedbackLane{}
	engine.normalUpstream = false
	engine.statusTimer = controllerPersonaStatusTimer{}
	engine.metadataActive = false
	engine.metadataTransfer = MetadataTransfer{}
	engine.metadataFence = ControllerMetadataTransferFence{}
	engine.retryPending = false
	engine.retryRecord = controllerPersonaRecord{}
	engine.hostPacketActive = false
	engine.hostPacketRetryPending = false
	engine.hostPacketRecord = controllerPersonaHostPacketRecord{}
	engine.hostPacketRetryRecord = controllerPersonaHostPacketRecord{}
	engine.lifecycle = restartControllerLifecycle(
		engine.lifecycle, successor, nowMS)
	if clearAlreadyDelivered {
		engine.directMotor = NewStopRumbleBody()
		engine.guideLED = GuideLEDCommandV1{Pattern: GuideLEDPatternOff}
	}
	return nil
}

func (engine *ControllerPersonaEngine) advanceReconnect(nowMS uint64) error {
	if err := engine.validateTransportAdvance(true); err != nil {
		return err
	}
	successor := engine.generation + 1
	if err := engine.usb.Reconnect(successor); err != nil {
		return err
	}
	if err := engine.global.Reset(successor); err != nil {
		return err
	}
	if err := engine.input.Reset(successor); err != nil {
		return err
	}
	engine.generation = successor
	engine.attached = true
	engine.normalUpstream = false
	engine.statusTimer = controllerPersonaStatusTimer{}
	engine.metadataActive = false
	engine.metadataTransfer = MetadataTransfer{}
	engine.metadataFence = ControllerMetadataTransferFence{}
	engine.retryPending = false
	engine.retryRecord = controllerPersonaRecord{}
	engine.hostPacketActive = false
	engine.hostPacketRetryPending = false
	engine.hostPacketRecord = controllerPersonaHostPacketRecord{}
	engine.hostPacketRetryRecord = controllerPersonaHostPacketRecord{}
	engine.lifecycle = restartControllerLifecycle(
		engine.lifecycle, successor, nowMS)
	return nil
}

func (engine *ControllerPersonaEngine) validateTransportAdvance(
	reconnect bool,
) error {
	if err := engine.validateTransportGenerationAlignment(reconnect); err != nil {
		return err
	}
	usb := engine.usb.Snapshot()
	if usb.ClaimOutstanding || engine.configurationLossClearPending ||
		engine.ordinaryFeedback.state == controllerPersonaOrdinaryFeedbackAdmitted ||
		(reconnect && engine.ordinaryFeedbackPending()) {
		return ErrControllerPersonaBoundaryBlocked
	}
	return nil
}

func (engine *ControllerPersonaEngine) validateTransportGenerationAlignment(
	reconnect bool,
) error {
	if err := engine.validateInitialized(); err != nil {
		return err
	}
	if engine.generation == ^uint64(0) ||
		engine.global.Generation() != engine.generation ||
		engine.input.Generation() != engine.generation ||
		engine.lifecycle.Generation() != engine.generation {
		return ErrInvalidTransferGeneration
	}
	usb := engine.usb.Snapshot()
	if usb.Generation != engine.generation {
		return ErrInvalidTransferGeneration
	}
	if reconnect {
		if engine.attached || usb.State != USBControlDeviceDetached {
			return ErrInvalidUSBControlTransition
		}
		return nil
	}
	if !engine.attached || usb.State == USBControlDeviceDetached {
		return ErrInvalidUSBControlTransition
	}
	return nil
}

func restartControllerLifecycle(
	previous ControllerLifecycle,
	generation uint64,
	nowMS uint64,
) ControllerLifecycle {
	return ControllerLifecycle{
		core: controllerLifecycleCore{
			state:       ControllerLifecycleArrival,
			generation:  generation,
			nextHelloMS: nowMS,
		},
		lastNowMS:                      nowMS,
		nextTransitionEpoch:            previous.nextTransitionEpoch,
		nextMetadataTransferGeneration: previous.nextMetadataTransferGeneration,
		nextToken:                      previous.nextToken,
	}
}
