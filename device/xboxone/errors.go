package xboxone

import "errors"

var (
	// ErrInvalidLength reports that a fixed-size header or message body was not exact.
	ErrInvalidLength = errors.New("xboxone: invalid wire length")
	// ErrReservedDataClass reports a MessageType whose high three bits select
	// one of the data classes reserved by MS-GIPUSB 1.0.
	ErrReservedDataClass = errors.New("xboxone: reserved GIP data class")
	// ErrInvalidMessageNumber reports a message number wider than five bits.
	ErrInvalidMessageNumber = errors.New("xboxone: invalid GIP message number")
	// ErrFragmentedHeader reports a fragmented header at the deliberately
	// single-packet-only boundary.
	ErrFragmentedHeader = errors.New("xboxone: fragmented GIP header is unsupported")
	// ErrInvalidInitFragment reports InitFrag without Fragment.
	ErrInvalidInitFragment = errors.New("xboxone: InitFrag set on a single-packet GIP header")
	// ErrReservedHeaderFlag reports the mandatory-zero bit 3 in GIP Flags.
	ErrReservedHeaderFlag = errors.New("xboxone: reserved GIP header flag is set")
	// ErrInvalidExpansionIndex reports an expansion index wider than three bits.
	ErrInvalidExpansionIndex = errors.New("xboxone: invalid GIP expansion index")
	// ErrReservedSequence reports the reserved sequence value zero.
	ErrReservedSequence = errors.New("xboxone: reserved GIP sequence ID")
	// ErrExtendedPayloadLength reports a length with its extension bit set at
	// the deliberately four-byte-header boundary.
	ErrExtendedPayloadLength = errors.New("xboxone: extended GIP payload length is unsupported")
	// ErrPayloadTooLarge reports a payload that exceeds its data-class MTU
	// after accounting for the four-byte header.
	ErrPayloadTooLarge = errors.New("xboxone: GIP payload exceeds single-packet MTU")
	// ErrReservedButtonBits reports the mandatory-zero low button bit.
	ErrReservedButtonBits = errors.New("xboxone: reserved base-button bits are set")
	// ErrTriggerOutOfRange reports a gamepad trigger value above the normative
	// 10-bit maximum.
	ErrTriggerOutOfRange = errors.New("xboxone: gamepad trigger exceeds 10-bit range")
	// ErrInvalidSemanticInputContract reports an unsupported version, embedded
	// size, reserved button bit, or non-zero reserved byte at the private
	// DS4Windows-to-VIIPER semantic boundary. It never describes a GIP packet.
	ErrInvalidSemanticInputContract = errors.New("xboxone: invalid broker semantic input contract")
	// ErrGuideRequiresStatusMessage prevents silently dropping Guide from a
	// base input body. Guide travels in a separate Guide Button Status message.
	ErrGuideRequiresStatusMessage = errors.New("xboxone: guide requires a separate status message")
	// ErrGuideButtonEdgeQueueFull applies lossless backpressure when the sole
	// broker ingress cannot retain another ordered Guide transition.
	ErrGuideButtonEdgeQueueFull = errors.New("xboxone: ordered guide-button edge queue is full")
	// ErrShareRequiresExtension prevents silently dropping Share from the base
	// input form. Share travels in the Console Function Map extension.
	ErrShareRequiresExtension = errors.New("xboxone: share requires the Console Function Map extension")
	// ErrInvalidGuideButtonStatusMessage reports a message outside the exact
	// two-byte upstream VK_LWIN status grammar.
	ErrInvalidGuideButtonStatusMessage = errors.New("xboxone: invalid GIP guide-button status message")
	// ErrInvalidConsoleFunctionMap reports a malformed or non-canonical
	// eighteen-byte Console Function Map gamepad extension.
	ErrInvalidConsoleFunctionMap = errors.New("xboxone: invalid GIP Console Function Map extension")
	// ErrInvalidGamepadInputMessage reports a syntactically valid GIP message
	// whose header is not the exact standard Gamepad Input Report envelope.
	ErrInvalidGamepadInputMessage = errors.New("xboxone: invalid GIP gamepad input message")
	// ErrInvalidDirectMotorCommand reports a non-zero Direct Motor Command byte.
	ErrInvalidDirectMotorCommand = errors.New("xboxone: invalid direct-motor command byte")
	// ErrInvalidMotorMask reports bits outside the four Direct Motor actuators.
	ErrInvalidMotorMask = errors.New("xboxone: invalid direct-motor bitmap")
	// ErrMotorLevelOutOfRange reports a Direct Motor level above 100 percent.
	ErrMotorLevelOutOfRange = errors.New("xboxone: direct-motor level exceeds 100 percent")
	// ErrInvalidDirectMotorMessage reports a syntactically valid GIP message
	// whose header is not the exact Direct Motor Command envelope.
	ErrInvalidDirectMotorMessage = errors.New("xboxone: invalid GIP direct-motor message")
	// ErrReservedGuideLEDPattern reports a Guide LED pattern not assigned by
	// MS-GIPUSB 1.0 table 42.
	ErrReservedGuideLEDPattern = errors.New("xboxone: reserved GIP guide LED pattern")
	// ErrGuideLEDIntensityOutOfRange reports an intensity above the normative
	// 47-percent upper bound.
	ErrGuideLEDIntensityOutOfRange = errors.New("xboxone: guide LED intensity exceeds 47 percent")
	// ErrInvalidGuideLEDCommand reports a non-zero Guide LED command selector.
	ErrInvalidGuideLEDCommand = errors.New("xboxone: invalid GIP guide LED command byte")
	// ErrInvalidGuideLEDMessage reports a syntactically valid GIP message whose
	// header is not the exact Guide LED Command envelope.
	ErrInvalidGuideLEDMessage = errors.New("xboxone: invalid GIP guide LED message")
	// ErrAuthenticationUnavailable is the default authentication result. No
	// caller may mistake an absent provider for successful authentication.
	ErrAuthenticationUnavailable = errors.New("xboxone: authentication provider unavailable")
	// ErrInvalidAuthOutputLength protects the session boundary from a provider
	// returning a count outside the supplied destination.
	ErrInvalidAuthOutputLength = errors.New("xboxone: authentication provider returned an invalid output length")
	// ErrInvalidUSBIdentity reports an absent caller-supplied USB VID or PID.
	ErrInvalidUSBIdentity = errors.New("xboxone: invalid caller-supplied USB identity")
	// ErrInvalidExternalUSBIdentityStrings reports absent, malformed UTF-8,
	// embedded NUL, invalid serial grammar, or a value which cannot fit one
	// exact USB UTF-16LE string descriptor.
	ErrInvalidExternalUSBIdentityStrings = errors.New("xboxone: invalid external USB identity strings")
	// ErrUSBSerialIdentityMismatch reports a 32-hex-digit serial which does not
	// contain the exact primary Device ID bound to the numeric profile.
	ErrUSBSerialIdentityMismatch = errors.New("xboxone: USB serial does not contain the bound Device ID")
	// ErrInvalidControllerIdentityAuthorizationDecision rejects an undefined
	// external caller decision. The package does not infer authorization.
	ErrInvalidControllerIdentityAuthorizationDecision = errors.New("xboxone: invalid external identity authorization decision")
	// ErrControllerIdentityAuthorizationDenied reports an explicit caller
	// denial. Denial creates and consumes no authorization credential.
	ErrControllerIdentityAuthorizationDenied = errors.New("xboxone: external identity authorization was denied")
	// ErrInvalidControllerIdentityAuthorization rejects a forged, mismatched,
	// copied-owner, quarantined, or otherwise unauthenticated configuration.
	ErrInvalidControllerIdentityAuthorization = errors.New("xboxone: invalid external identity authorization")
	// ErrStaleControllerIdentityAuthorization rejects reuse of the exact
	// one-shot authorization after an engine construction attempt consumed it.
	ErrStaleControllerIdentityAuthorization = errors.New("xboxone: stale external identity authorization")
	// ErrInvalidDeviceID reports a Device ID that does not have the prefix
	// required by MS-GIPUSB 1.0 section 2.2.1.3.
	ErrInvalidDeviceID = errors.New("xboxone: invalid GIP primary Device ID")
	// ErrInvalidDeviceReleaseBCD reports a USB bcdDevice value containing a
	// non-decimal nibble.
	ErrInvalidDeviceReleaseBCD = errors.New("xboxone: invalid USB device-release BCD")
	// ErrInvalidFirmwareVersion reports the forbidden all-zero Hello firmware
	// version.
	ErrInvalidFirmwareVersion = errors.New("xboxone: invalid all-zero GIP firmware version")
	// ErrInvalidUSBPower reports bMaxPower outside the one-through-250 range
	// established for the controller configuration descriptor.
	ErrInvalidUSBPower = errors.New("xboxone: invalid USB maximum power")
	// ErrInvalidUSBInterval reports a data endpoint interval outside the byte
	// domain or below the four-millisecond GIP minimum.
	ErrInvalidUSBInterval = errors.New("xboxone: invalid GIP interrupt endpoint interval")
	// ErrUninitializedControllerProfile prevents the zero-value profile from
	// producing descriptors or Hello messages.
	ErrUninitializedControllerProfile = errors.New("xboxone: uninitialized unregistered controller profile")
	// ErrUninitializedUSBControlPlane prevents the zero-value EP0 state machine
	// from admitting a request.
	ErrUninitializedUSBControlPlane = errors.New("xboxone: uninitialized USB control plane")
	// ErrInvalidUSBSetupPacket reports an EP0 setup packet that is not exactly
	// eight bytes.
	ErrInvalidUSBSetupPacket = errors.New("xboxone: invalid USB setup packet")
	// ErrUSBControlRequestStalled reports a request which the controller-only
	// MS-GIPUSB table-4 state matrix requires to STALL.
	ErrUSBControlRequestStalled = errors.New("xboxone: USB control request must stall")
	// ErrUSBStringDescriptorUnavailable preserves the explicit Manufacturer,
	// Product, and Serial string gate rather than fabricating an identity.
	ErrUSBStringDescriptorUnavailable = errors.New("xboxone: external USB string descriptor is unavailable")
	// ErrUSBControlPlaneDetached reports a request while the offline device is
	// disconnected.
	ErrUSBControlPlaneDetached = errors.New("xboxone: USB control plane is detached")
	// ErrUSBControlClaimOutstanding reports a second EP0 request before the
	// first has reached terminal resolution.
	ErrUSBControlClaimOutstanding = errors.New("xboxone: USB control claim already outstanding")
	// ErrInvalidUSBControlClaim reports a forged, stale, duplicate, or
	// otherwise mismatched EP0 transaction capability.
	ErrInvalidUSBControlClaim = errors.New("xboxone: invalid USB control claim")
	// ErrUSBControlClaimNotAdmitted reports delivery before the exact response
	// passed final serialized admission.
	ErrUSBControlClaimNotAdmitted = errors.New("xboxone: USB control claim was not admitted")
	// ErrInvalidUSBControlDestination reports a short or oversized response
	// destination. EP0 responses are never partially copied by this API.
	ErrInvalidUSBControlDestination = errors.New("xboxone: invalid USB control response destination")
	// ErrInvalidUSBControlOutcome reports an undefined terminal resolution.
	ErrInvalidUSBControlOutcome = errors.New("xboxone: invalid USB control outcome")
	// ErrUSBControlBoundaryBlocked reports reset/disconnect/reconnect while an
	// EP0 transaction has not been completed or synchronously cancelled.
	ErrUSBControlBoundaryBlocked = errors.New("xboxone: USB control lifecycle boundary is blocked")
	// ErrInvalidUSBControlTransition reports a non-successor generation or an
	// invalid reconnect transition.
	ErrInvalidUSBControlTransition = errors.New("xboxone: invalid USB control lifecycle transition")
	// ErrInvalidHelloMessage reports a syntactically valid GIP message that is
	// not the exact primary-device Hello form.
	ErrInvalidHelloMessage = errors.New("xboxone: invalid GIP Hello message")
	// ErrReservedStatusValue reports a reserved or deprecated value in one of
	// the Extended Status Device Message status sub-fields.
	ErrReservedStatusValue = errors.New("xboxone: reserved GIP status value")
	// ErrReservedExtendedStatusFlag reports a non-zero reserved bit in the
	// Extended Status field.
	ErrReservedExtendedStatusFlag = errors.New("xboxone: reserved GIP extended-status flag")
	// ErrUnsupportedStatusEvents reports an Extended Status body whose Events
	// Present bit requires the separately sized event form that is not exposed
	// by the no-events codec.
	ErrUnsupportedStatusEvents = errors.New("xboxone: extended status events are unsupported")
	// ErrInvalidStatusMessage reports a syntactically valid GIP message that is
	// not the exact primary-device Extended Status no-events form.
	ErrInvalidStatusMessage = errors.New("xboxone: invalid GIP extended status message")
	// ErrUnsupportedProtocolControlCode reports a Protocol Control body whose
	// control code is not the only currently valid value, ACK (zero).
	ErrUnsupportedProtocolControlCode = errors.New("xboxone: unsupported GIP protocol-control code")
	// ErrInvalidProtocolControlReferenceFlags reports RefMessageFlags bits other
	// than System and Expansion Index, which the official table requires clear.
	ErrInvalidProtocolControlReferenceFlags = errors.New("xboxone: invalid GIP protocol-control reference flags")
	// ErrInvalidProtocolControlMessage reports a syntactically valid GIP message
	// that is not the exact primary-device Protocol Control ACK form.
	ErrInvalidProtocolControlMessage = errors.New("xboxone: invalid GIP protocol-control message")
	// ErrUnsupportedHostCommand reports a downstream message outside the
	// controller-startup subset implemented here.
	ErrUnsupportedHostCommand = errors.New("xboxone: unsupported controller host command")
	// ErrUnsupportedSetDeviceStateVariant reports a 15-byte variant other than
	// the exact Windows/SDL initialization frame. MS-GIPUSB 1.0 does not define
	// the extended payload, so no other body is inferred.
	ErrUnsupportedSetDeviceStateVariant = errors.New("xboxone: unsupported Set Device State payload variant")
	// ErrReservedDeviceState reports a reserved Set Device State value.
	ErrReservedDeviceState = errors.New("xboxone: reserved GIP device state")
	// ErrInvalidMetadata reports an empty or overlong externally compiled
	// metadata blob at the typed metadata seam.
	ErrInvalidMetadata = errors.New("xboxone: invalid externally compiled GIP metadata")
	// ErrMetadataIdentityMismatch prevents a compiled metadata binding from
	// being reused with a different descriptor/Hello identity.
	ErrMetadataIdentityMismatch = errors.New("xboxone: GIP metadata identity does not match controller profile")
	// ErrMetadataTransferComplete reports an attempt to emit another packet
	// after the metadata completion message.
	ErrMetadataTransferComplete = errors.New("xboxone: GIP metadata transfer is complete")
	// ErrAcknowledgementRequired reports an attempt to advance a reliable
	// transfer while an ACME-requested acknowledgement remains outstanding.
	ErrAcknowledgementRequired = errors.New("xboxone: reliable-transfer acknowledgement required")
	// ErrInvalidAcknowledgement reports impossible or mismatched ACK progress
	// supplied through the typed ACK seam.
	ErrInvalidAcknowledgement = errors.New("xboxone: invalid reliable-transfer acknowledgement")
	// ErrInvalidTransferGeneration reports a zero or non-successor transaction
	// generation. Generations fence late claims and acknowledgements across a
	// reset without relying on the eight-bit wire sequence alone.
	ErrInvalidTransferGeneration = errors.New("xboxone: invalid transfer generation")
	// ErrInvalidTransferEpoch reports a zero, reused, or non-successor local
	// reliable-message epoch. The epoch is transaction context, not an ACK wire
	// field.
	ErrInvalidTransferEpoch = errors.New("xboxone: invalid reliable-transfer epoch")
	// ErrMetadataClaimOutstanding reports a second packet claim before the
	// first one has reached a terminal resolution.
	ErrMetadataClaimOutstanding = errors.New("xboxone: metadata packet claim already outstanding")
	// ErrMetadataRetryRequired reports that an exact deferred packet must be
	// reclaimed before new metadata progress may be selected.
	ErrMetadataRetryRequired = errors.New("xboxone: metadata packet retry required")
	// ErrInvalidMetadataClaim reports a forged, stale, copied-owner, duplicate,
	// or otherwise mismatched metadata packet claim.
	ErrInvalidMetadataClaim = errors.New("xboxone: invalid metadata packet claim")
	// ErrMetadataClaimNotAdmitted reports an attempted delivery commit before
	// the exact packet passed its final admission check.
	ErrMetadataClaimNotAdmitted = errors.New("xboxone: metadata packet claim was not admitted")
	// ErrInvalidMetadataOutcome reports an undefined packet resolution value.
	ErrInvalidMetadataOutcome = errors.New("xboxone: invalid metadata packet outcome")
	// ErrReliableTransferTimeout reports expiry of the one-second ACK window.
	ErrReliableTransferTimeout = errors.New("xboxone: reliable metadata transfer acknowledgement timeout")
	// ErrMetadataTransferFaulted reports use of a transfer after a terminal
	// timeout and before an explicit successor-generation reset.
	ErrMetadataTransferFaulted = errors.New("xboxone: metadata transfer is faulted")
	// ErrNonMonotonicMetadataClock reports caller time moving backwards across
	// packet selection, admission, resolution, acknowledgement, or polling.
	ErrNonMonotonicMetadataClock = errors.New("xboxone: metadata transfer clock moved backwards")
	// ErrUninitializedLifecycle prevents the zero-value lifecycle from
	// emitting protocol actions.
	ErrUninitializedLifecycle = errors.New("xboxone: uninitialized controller lifecycle")
	// ErrUnexpectedLifecycleCommand reports a valid host command that is not
	// legal in the current controller lifecycle state.
	ErrUnexpectedLifecycleCommand = errors.New("xboxone: unexpected command for controller lifecycle state")
	// ErrNonMonotonicLifecycleClock reports caller time moving backwards.
	ErrNonMonotonicLifecycleClock = errors.New("xboxone: controller lifecycle clock moved backwards")
	// ErrLifecycleClaimOutstanding reports a second lifecycle claim before the
	// first one has reached a terminal resolution.
	ErrLifecycleClaimOutstanding = errors.New("xboxone: controller lifecycle claim already outstanding")
	// ErrLifecycleRetryRequired reports that a deferred lifecycle obligation
	// must be reclaimed before a new host event may be processed.
	ErrLifecycleRetryRequired = errors.New("xboxone: controller lifecycle retry required")
	// ErrLifecycleTransitionPending reports that a new event cannot overtake
	// the explicit action cursor of an unfinished transition.
	ErrLifecycleTransitionPending = errors.New("xboxone: controller lifecycle transition pending")
	// ErrLifecycleNoPendingAction reports a request for a next action when no
	// unfinished transition exists.
	ErrLifecycleNoPendingAction = errors.New("xboxone: no pending controller lifecycle action")
	// ErrInvalidLifecycleClaim reports a forged, stale, copied-owner,
	// duplicate, or otherwise mismatched lifecycle claim.
	ErrInvalidLifecycleClaim = errors.New("xboxone: invalid controller lifecycle claim")
	// ErrLifecycleClaimNotAdmitted reports a delivery commit before final
	// admission of the exact ordered lifecycle decision.
	ErrLifecycleClaimNotAdmitted = errors.New("xboxone: controller lifecycle claim was not admitted")
	// ErrInvalidLifecycleOutcome reports an undefined lifecycle resolution.
	ErrInvalidLifecycleOutcome = errors.New("xboxone: invalid controller lifecycle outcome")
	// ErrSequenceClaimOutstanding reports a second sequence claim before the
	// first one has reached terminal resolution.
	ErrSequenceClaimOutstanding = errors.New("xboxone: sequence claim already outstanding")
	// ErrSequenceRetryRequired reports that the exact deferred sequence value
	// must be reclaimed before allocating a successor.
	ErrSequenceRetryRequired = errors.New("xboxone: sequence retry required")
	// ErrInvalidSequenceClaim reports a forged, stale, copied-owner, duplicate,
	// or otherwise mismatched sequence claim.
	ErrInvalidSequenceClaim = errors.New("xboxone: invalid sequence claim")
	// ErrSequenceClaimNotAdmitted reports a commit before the sequence claim's
	// final admission boundary.
	ErrSequenceClaimNotAdmitted = errors.New("xboxone: sequence claim was not admitted")
	// ErrInvalidSequenceOutcome reports an undefined sequence resolution.
	ErrInvalidSequenceOutcome = errors.New("xboxone: invalid sequence outcome")
	// ErrUninitializedControllerPersona prevents the zero-value offline persona
	// engine from selecting USB, lifecycle, input, or feedback work.
	ErrUninitializedControllerPersona = errors.New("xboxone: uninitialized controller persona engine")
	// ErrInvalidControllerPersonaConfig reports contradictory or incomplete
	// explicit caller facts supplied to the offline persona engine.
	ErrInvalidControllerPersonaConfig = errors.New("xboxone: invalid controller persona configuration")
	// ErrControllerPersonaClaimOutstanding reports a second serialized persona
	// action before the first reaches terminal resolution.
	ErrControllerPersonaClaimOutstanding = errors.New("xboxone: controller persona claim already outstanding")
	// ErrControllerPersonaRetryRequired reports an immutable non-delivered
	// persona action which must be reclaimed before newer work.
	ErrControllerPersonaRetryRequired = errors.New("xboxone: controller persona retry required")
	// ErrControllerPersonaConfigurationLossClearRequired reports that a
	// delivered Configured-to-Addressed transition owns one mandatory typed
	// output-clear action which must be selected before unrelated persona work.
	ErrControllerPersonaConfigurationLossClearRequired = errors.New("xboxone: controller persona configuration-loss output clear required")
	// ErrInvalidControllerPersonaClaim reports a forged, stale, duplicate, or
	// otherwise mismatched persona action capability.
	ErrInvalidControllerPersonaClaim = errors.New("xboxone: invalid controller persona claim")
	// ErrControllerPersonaClaimNotAdmitted reports delivery or execution
	// cancellation before the exact persona action crossed final admission.
	ErrControllerPersonaClaimNotAdmitted = errors.New("xboxone: controller persona claim was not admitted")
	// ErrInvalidControllerPersonaOutcome reports an undefined terminal result.
	ErrInvalidControllerPersonaOutcome = errors.New("xboxone: invalid controller persona outcome")
	// ErrInvalidControllerPersonaDestination reports a short or oversized
	// adapter-owned destination at final admission.
	ErrInvalidControllerPersonaDestination = errors.New("xboxone: invalid controller persona destination")
	// ErrControllerPersonaDetached reports GIP work attempted while the
	// authoritative USB transport generation is detached.
	ErrControllerPersonaDetached = errors.New("xboxone: controller persona transport is detached")
	// ErrControllerPersonaUSBNotConfigured reports GIP endpoint work before the
	// fixed controller configuration is selected.
	ErrControllerPersonaUSBNotConfigured = errors.New("xboxone: controller persona USB data interface is not configured")
	// ErrControllerPersonaEndpointHalted reports GIP endpoint work held while
	// the corresponding interrupt endpoint Halt feature is set.
	ErrControllerPersonaEndpointHalted = errors.New("xboxone: controller persona GIP endpoint is halted")
	// ErrControllerPersonaUpstreamGated reports ordinary input publication
	// before the ordered START permit action or after a gate boundary.
	ErrControllerPersonaUpstreamGated = errors.New("xboxone: controller persona normal upstream publication is gated")
	// ErrUnsupportedControllerPersonaHostMessage reports a well-framed but
	// deliberately absent downstream message family.
	ErrUnsupportedControllerPersonaHostMessage = errors.New("xboxone: unsupported controller persona host message")
	// ErrControllerPersonaMetadataNotActive reports reliable metadata work with
	// no exact lifecycle metadata-transfer fence.
	ErrControllerPersonaMetadataNotActive = errors.New("xboxone: controller persona metadata transfer is not active")
	// ErrControllerPersonaBoundaryBlocked reports a reset, disconnect, or
	// reconnect which would overtake an action, retry, or unfinished transition.
	ErrControllerPersonaBoundaryBlocked = errors.New("xboxone: controller persona transport boundary is blocked")
	// ErrNonMonotonicControllerPersonaClock reports caller time moving backward
	// across the composed lifecycle and metadata clock domain.
	ErrNonMonotonicControllerPersonaClock = errors.New("xboxone: controller persona clock moved backwards")
	// ErrControllerPersonaInvariantViolation reports hidden inner state which
	// cannot be produced through the composed engine's serialized public API.
	ErrControllerPersonaInvariantViolation = errors.New("xboxone: controller persona invariant violation")
	// ErrControllerPersonaHostPacketContextLimit rejects a coalesced packet
	// containing more than one lifecycle/ACK-governed message, or a STOP/OFF/
	// RESET lifecycle member with any sibling while its mandatory output clear
	// is a successor cursor. The canonical persona currently owns one such inner
	// claim; guessing a second preview, delaying the clear, or committing a
	// prefix is forbidden.
	ErrControllerPersonaHostPacketContextLimit = errors.New("xboxone: downstream packet exceeds canonical context-claim capacity")
	// ErrInvalidControllerPersonaHostPacketClaim rejects forged, stale, or
	// mismatched whole-packet capabilities.
	ErrInvalidControllerPersonaHostPacketClaim = errors.New("xboxone: invalid controller persona downstream-packet claim")
	// ErrControllerPersonaHostPacketRetryRequired prevents standalone persona
	// work from overtaking the exact immutable coalesced-packet retry.
	ErrControllerPersonaHostPacketRetryRequired = errors.New("xboxone: controller persona downstream-packet retry required")
	// ErrControllerPersonaHostPacketOwnerUninitialized rejects an absent engine
	// or whole-vector participant at the dormant combined-owner boundary.
	ErrControllerPersonaHostPacketOwnerUninitialized = errors.New("xboxone: controller persona downstream-packet owner is uninitialized")
	// ErrControllerPersonaHostPacketOwnerBusy preserves one combined packet at
	// a time across both persona and external execution owners.
	ErrControllerPersonaHostPacketOwnerBusy = errors.New("xboxone: controller persona downstream-packet owner is busy")
	// ErrControllerPersonaHostPacketQuarantined is terminal when a selected
	// persona vector violates an invariant or the two canonical owners cannot
	// be proven to have resolved the same exact fact.
	ErrControllerPersonaHostPacketQuarantined = errors.New("xboxone: controller persona downstream-packet owner is quarantined")
)
