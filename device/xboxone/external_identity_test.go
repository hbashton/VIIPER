package xboxone

import (
	"encoding/binary"
	"errors"
	stdstrings "strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf16"
)

const testOnlySyntheticSerial = "0000fffb01020304A1B2C3D4E5F60708"

func testOnlyControllerPersonaConfig(
	t *testing.T,
) ControllerPersonaConfig {
	t.Helper()
	profile := testOnlySyntheticControllerProfile(t)
	metadata, err := profile.BindExternallyCompiledMetadata([]byte{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	return ControllerPersonaConfig{
		Profile: profile, Metadata: metadata,
		CurrentInput:      GamepadInputReportV1{},
		CurrentStatus:     NewWiredNoBatteryStatus(false),
		PoweringOffStatus: NewWiredNoBatteryStatus(true),
	}
}

func testOnlyIdentityStrings() ControllerUSBIdentityStrings {
	return ControllerUSBIdentityStrings{
		Manufacturer: "Acme",
		Product:      "Pro Pad",
		Serial:       testOnlySyntheticSerial,
	}
}

func testOnlyAuthorizedPersonaConfig(
	t *testing.T,
) AuthorizedControllerPersonaConfig {
	t.Helper()
	authorization, err := NewAuthorizedControllerPersonaConfig(
		testOnlyControllerPersonaConfig(t), testOnlyIdentityStrings(),
		ControllerIdentityAuthorizationGranted)
	if err != nil {
		t.Fatalf("NewAuthorizedControllerPersonaConfig: %v", err)
	}
	return authorization
}

func testOnlyEncodeUSBStringDescriptor(value string) []byte {
	units := utf16.Encode([]rune(value))
	wire := make([]byte, 2+2*len(units))
	wire[0] = byte(len(wire))
	wire[1] = usbDescriptorTypeString
	for index, unit := range units {
		binary.LittleEndian.PutUint16(wire[2+index*2:], unit)
	}
	return wire
}

func deliverAuthorizedUSBString(
	t *testing.T,
	engine *ControllerPersonaEngine,
	index byte,
	language uint16,
	wLength uint16,
) (ControllerPersonaClaim, []byte) {
	t.Helper()
	claim, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeDeviceIn, usbRequestGetDescriptor,
		uint16(usbDescriptorTypeString)<<8|uint16(index), language, wLength))
	if err != nil {
		t.Fatalf("ClaimUSBControl index %d: %v", index, err)
	}
	wire := make([]byte, claim.Size())
	if err := engine.AdmitAndCopy(claim, wire, 0); err != nil {
		t.Fatalf("AdmitAndCopy index %d: %v", index, err)
	}
	if err := engine.Resolve(claim, ControllerPersonaDelivered, 0); err != nil {
		t.Fatalf("Resolve index %d: %v", index, err)
	}
	return claim, wire
}

func TestAuthorizedControllerPersonaServesOnlyBoundIdentityStrings(t *testing.T) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	engine, err := NewAuthorizedControllerPersonaEngine(authorization, 0)
	if err != nil {
		t.Fatalf("NewAuthorizedControllerPersonaEngine: %v", err)
	}
	wantBlockers := controllerPersonaKnownBlockers &^
		(ControllerPersonaBlockerExternalUSBStrings |
			ControllerPersonaBlockerIdentityAuthorization)
	if got := engine.CapabilityBlockers(); got != wantBlockers {
		t.Fatalf("authorized blockers = 0x%x, want 0x%x", got, wantBlockers)
	}

	tests := []struct {
		index byte
		kind  USBControlResponseKind
		value string
	}{
		{index: 1, kind: USBControlResponseManufacturerStringDescriptor, value: "Acme"},
		{index: 2, kind: USBControlResponseProductStringDescriptor, value: "Pro Pad"},
		{index: 3, kind: USBControlResponseSerialStringDescriptor, value: testOnlySyntheticSerial},
	}
	for _, test := range tests {
		claim, got := deliverAuthorizedUSBString(t, engine, test.index,
			USBEnglishUnitedStatesLanguageID, 0xffff)
		if claim.USBResponseKind() != test.kind {
			t.Fatalf("index %d kind = %d, want %d",
				test.index, claim.USBResponseKind(), test.kind)
		}
		want := testOnlyEncodeUSBStringDescriptor(test.value)
		if string(got) != string(want) {
			t.Fatalf("index %d descriptor = % x, want % x", test.index, got, want)
		}
	}

	_, truncated := deliverAuthorizedUSBString(t, engine, 2,
		USBEnglishUnitedStatesLanguageID, 5)
	wantProduct := testOnlyEncodeUSBStringDescriptor("Pro Pad")
	if string(truncated) != string(wantProduct[:5]) {
		t.Fatalf("truncated product = % x, want % x", truncated, wantProduct[:5])
	}
}

func TestAuthorizedControllerPersonaStringRequestMatrixFailsClosed(t *testing.T) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	engine, err := NewAuthorizedControllerPersonaEngine(authorization, 0)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		index    byte
		language uint16
	}{
		{name: "wrong language", index: 1, language: 0},
		{name: "unsupported language", index: 2, language: 0x0411},
		{name: "unsupported index", index: 4, language: USBEnglishUnitedStatesLanguageID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setup := testUSBSetup(usbRequestTypeDeviceIn, usbRequestGetDescriptor,
				uint16(usbDescriptorTypeString)<<8|uint16(test.index),
				test.language, 0xffff)
			if _, err := engine.ClaimUSBControl(setup); !errors.Is(
				err, ErrUSBControlRequestStalled) {
				t.Fatalf("error = %v, want stall", err)
			}
			if engine.Snapshot().ClaimOutstanding {
				t.Fatal("rejected string request retained a claim")
			}
		})
	}
}

func TestAuthorizedControllerPersonaSplitBrainCannotServeStrings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *ControllerPersonaEngine)
	}{
		{
			name: "engine binding removed",
			mutate: func(_ *testing.T, engine *ControllerPersonaEngine) {
				engine.identityBinding = nil
			},
		},
		{
			name: "foreign metadata issuance with same profile",
			mutate: func(t *testing.T, engine *ControllerPersonaEngine) {
				metadata, err := engine.profile.BindExternallyCompiledMetadata(
					engine.metadata.data)
				if err != nil {
					t.Fatal(err)
				}
				engine.metadata = metadata
			},
		},
		{
			name: "metadata digest changed",
			mutate: func(_ *testing.T, engine *ControllerPersonaEngine) {
				changed := append([]byte(nil), engine.metadata.data...)
				changed[0] ^= 0xff
				engine.metadata.data = changed
			},
		},
		{
			name: "engine profile equal-value new issuance",
			mutate: func(t *testing.T, engine *ControllerPersonaEngine) {
				profile, err := NewUnregisteredControllerProfile(
					engine.profile.identity, engine.profile.usb)
				if err != nil {
					t.Fatal(err)
				}
				engine.profile = profile
			},
		},
		{
			name: "control-plane profile equal-value new issuance",
			mutate: func(t *testing.T, engine *ControllerPersonaEngine) {
				profile, err := NewUnregisteredControllerProfile(
					engine.usb.profile.identity, engine.usb.profile.usb)
				if err != nil {
					t.Fatal(err)
				}
				engine.usb.profile = profile
			},
		},
		{
			name: "control-plane binding from foreign engine",
			mutate: func(t *testing.T, engine *ControllerPersonaEngine) {
				foreign, err := NewAuthorizedControllerPersonaEngine(
					testOnlyAuthorizedPersonaConfig(t), 0)
				if err != nil {
					t.Fatal(err)
				}
				engine.usb.identityBinding = foreign.identityBinding
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, err := NewAuthorizedControllerPersonaEngine(
				testOnlyAuthorizedPersonaConfig(t), 0)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, engine)
			if got := engine.CapabilityBlockers(); got != controllerPersonaKnownBlockers {
				t.Fatalf("split-brain blockers = 0x%x", got)
			}
			setup := testUSBSetup(usbRequestTypeDeviceIn, usbRequestGetDescriptor,
				uint16(usbDescriptorTypeString)<<8|1,
				USBEnglishUnitedStatesLanguageID, 0xffff)
			if _, err := engine.ClaimUSBControl(setup); !errors.Is(
				err, ErrInvalidControllerIdentityAuthorization) ||
				!errors.Is(err, ErrUSBControlRequestStalled) {
				t.Fatalf("split-brain string error = %v", err)
			}
			if engine.Snapshot().ClaimOutstanding {
				t.Fatal("split-brain string request published a claim")
			}
		})
	}
}

func TestExternalUSBIdentityStringValidation(t *testing.T) {
	valid := testOnlyIdentityStrings()
	tests := []struct {
		name    string
		strings ControllerUSBIdentityStrings
		want    error
	}{
		{name: "empty manufacturer", strings: func() ControllerUSBIdentityStrings {
			value := valid
			value.Manufacturer = ""
			return value
		}(), want: ErrInvalidExternalUSBIdentityStrings},
		{name: "malformed manufacturer UTF-8", strings: func() ControllerUSBIdentityStrings {
			value := valid
			value.Manufacturer = string([]byte{0xff})
			return value
		}(), want: ErrInvalidExternalUSBIdentityStrings},
		{name: "embedded NUL", strings: func() ControllerUSBIdentityStrings {
			value := valid
			value.Product = "Pad\x00Name"
			return value
		}(), want: ErrInvalidExternalUSBIdentityStrings},
		{name: "UTF-16 overflow", strings: func() ControllerUSBIdentityStrings {
			value := valid
			value.Product = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			return value
		}(), want: ErrInvalidExternalUSBIdentityStrings},
		{name: "short serial", strings: func() ControllerUSBIdentityStrings {
			value := valid
			value.Serial = value.Serial[:31]
			return value
		}(), want: ErrInvalidExternalUSBIdentityStrings},
		{name: "non-hex serial", strings: func() ControllerUSBIdentityStrings {
			value := valid
			value.Serial = value.Serial[:31] + "Z"
			return value
		}(), want: ErrInvalidExternalUSBIdentityStrings},
		{name: "serial missing Device ID", strings: func() ControllerUSBIdentityStrings {
			value := valid
			value.Serial = "11111111111111112222222222222222"
			return value
		}(), want: ErrUSBSerialIdentityMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewAuthorizedControllerPersonaConfig(
				testOnlyControllerPersonaConfig(t), test.strings,
				ControllerIdentityAuthorizationGranted)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	strings := valid
	strings.Product = "Pad \U0001f3ae"
	authorization, err := NewAuthorizedControllerPersonaConfig(
		testOnlyControllerPersonaConfig(t), strings,
		ControllerIdentityAuthorizationGranted)
	if err != nil {
		t.Fatalf("valid supplementary Unicode: %v", err)
	}
	engine, err := NewAuthorizedControllerPersonaEngine(authorization, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, got := deliverAuthorizedUSBString(t, engine, 2,
		USBEnglishUnitedStatesLanguageID, 0xffff)
	if want := testOnlyEncodeUSBStringDescriptor(strings.Product); string(got) != string(want) {
		t.Fatalf("supplementary descriptor = % x, want % x", got, want)
	}

	strings = valid
	strings.Product = stdstrings.Repeat("a", 126)
	authorization, err = NewAuthorizedControllerPersonaConfig(
		testOnlyControllerPersonaConfig(t), strings,
		ControllerIdentityAuthorizationGranted)
	if err != nil {
		t.Fatalf("maximum UTF-16 length: %v", err)
	}
	engine, err = NewAuthorizedControllerPersonaEngine(authorization, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, got = deliverAuthorizedUSBString(t, engine, 2,
		USBEnglishUnitedStatesLanguageID, 0xffff)
	if len(got) != usbMaximumStringDescriptorSize {
		t.Fatalf("maximum descriptor length = %d", len(got))
	}

	strings = valid
	strings.Product = stdstrings.Repeat("\u0800", 126)
	if len(strings.Product) != usbMaximumStringSourceUTF8Bytes {
		t.Fatalf("boundary source length = %d", len(strings.Product))
	}
	if _, err := NewAuthorizedControllerPersonaConfig(
		testOnlyControllerPersonaConfig(t), strings,
		ControllerIdentityAuthorizationGranted); err != nil {
		t.Fatalf("378-byte UTF-8 boundary: %v", err)
	}
	strings.Product += "a"
	if len(strings.Product) != usbMaximumStringSourceUTF8Bytes+1 {
		t.Fatalf("over-boundary source length = %d", len(strings.Product))
	}
	if _, err := NewAuthorizedControllerPersonaConfig(
		testOnlyControllerPersonaConfig(t), strings,
		ControllerIdentityAuthorizationGranted); !errors.Is(
		err, ErrInvalidExternalUSBIdentityStrings) {
		t.Fatalf("379-byte UTF-8 boundary error = %v", err)
	}
	strings.Product = stdstrings.Repeat("a", 1<<20)
	if _, err := NewAuthorizedControllerPersonaConfig(
		testOnlyControllerPersonaConfig(t), strings,
		ControllerIdentityAuthorizationGranted); !errors.Is(
		err, ErrInvalidExternalUSBIdentityStrings) {
		t.Fatalf("huge UTF-8 source error = %v", err)
	}
}

func TestControllerIdentityAuthorizationDecisionAndExactIssuance(t *testing.T) {
	if _, err := NewAuthorizedControllerPersonaEngine(
		AuthorizedControllerPersonaConfig{}, 0); !errors.Is(
		err, ErrInvalidControllerIdentityAuthorization) {
		t.Fatalf("zero authorization error = %v", err)
	}
	config := testOnlyControllerPersonaConfig(t)
	if _, err := NewAuthorizedControllerPersonaConfig(config,
		testOnlyIdentityStrings(), ControllerIdentityAuthorizationDenied); !errors.Is(
		err, ErrControllerIdentityAuthorizationDenied) {
		t.Fatalf("denied error = %v", err)
	}
	if _, err := NewAuthorizedControllerPersonaConfig(config,
		testOnlyIdentityStrings(), 0); !errors.Is(
		err, ErrInvalidControllerIdentityAuthorizationDecision) {
		t.Fatalf("zero decision error = %v", err)
	}
	// A denial created no credential and did not revoke the same reusable
	// caller inputs. A subsequent explicit grant is independently valid.
	authorization, err := NewAuthorizedControllerPersonaConfig(config,
		testOnlyIdentityStrings(), ControllerIdentityAuthorizationGranted)
	if err != nil {
		t.Fatalf("grant after denial: %v", err)
	}
	if _, err := NewAuthorizedControllerPersonaEngine(authorization, 0); err != nil {
		t.Fatalf("engine after grant: %v", err)
	}

	otherProfile, err := NewUnregisteredControllerProfile(
		config.Profile.identity, config.Profile.usb)
	if err != nil {
		t.Fatal(err)
	}
	otherMetadata, err := otherProfile.BindExternallyCompiledMetadata(config.Metadata.data)
	if err != nil {
		t.Fatal(err)
	}
	mismatched := config
	mismatched.Metadata = otherMetadata
	if _, err := NewControllerPersonaEngine(mismatched, 0); !errors.Is(
		err, ErrMetadataIdentityMismatch) {
		t.Fatalf("legacy same-value foreign metadata error = %v", err)
	}
	if _, err := config.Profile.NewMetadataTransfer(
		otherMetadata, 1, 1, 1, 0); !errors.Is(err, ErrMetadataIdentityMismatch) {
		t.Fatalf("transfer same-value foreign metadata error = %v", err)
	}
	if _, err := NewAuthorizedControllerPersonaConfig(mismatched,
		testOnlyIdentityStrings(), ControllerIdentityAuthorizationGranted); !errors.Is(
		err, ErrMetadataIdentityMismatch) {
		t.Fatalf("same-value foreign metadata error = %v", err)
	}
}

func TestControllerIdentityAuthorizationCopyForgeABAAndFailure(t *testing.T) {
	t.Run("copy is one shot", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		copied := authorization
		if _, err := NewAuthorizedControllerPersonaEngine(authorization, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := NewAuthorizedControllerPersonaEngine(copied, 0); !errors.Is(
			err, ErrStaleControllerIdentityAuthorization) {
			t.Fatalf("copied reuse error = %v", err)
		}
	})

	t.Run("forged credential does not poison canonical", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		forged := authorization
		forged.credential = &controllerPersonaIdentityCredential{marker: 1}
		if _, err := NewAuthorizedControllerPersonaEngine(forged, 0); !errors.Is(
			err, ErrInvalidControllerIdentityAuthorization) {
			t.Fatalf("forged error = %v", err)
		}
		if _, err := NewAuthorizedControllerPersonaEngine(authorization, 0); err != nil {
			t.Fatalf("canonical after forgery: %v", err)
		}
	})

	t.Run("cross-owner forgery does not poison either owner", func(t *testing.T) {
		first := testOnlyAuthorizedPersonaConfig(t)
		second := testOnlyAuthorizedPersonaConfig(t)
		forged := first
		forged.owner = second.owner
		if _, err := NewAuthorizedControllerPersonaEngine(forged, 0); !errors.Is(
			err, ErrInvalidControllerIdentityAuthorization) {
			t.Fatalf("cross-owner error = %v", err)
		}
		if _, err := NewAuthorizedControllerPersonaEngine(first, 0); err != nil {
			t.Fatalf("first canonical after forgery: %v", err)
		}
		if _, err := NewAuthorizedControllerPersonaEngine(second, 0); err != nil {
			t.Fatalf("second canonical after forgery: %v", err)
		}
	})

	t.Run("same-value ABA is quarantined", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		original := authorization.owner.config
		replacement, err := NewUnregisteredControllerProfile(
			original.Profile.identity, original.Profile.usb)
		if err != nil {
			t.Fatal(err)
		}
		replacementMetadata, err := replacement.BindExternallyCompiledMetadata(
			original.Metadata.data)
		if err != nil {
			t.Fatal(err)
		}
		authorization.owner.config.Profile = replacement
		authorization.owner.config.Metadata = replacementMetadata
		if _, err := NewAuthorizedControllerPersonaEngine(authorization, 0); !errors.Is(
			err, ErrInvalidControllerIdentityAuthorization) {
			t.Fatalf("ABA error = %v", err)
		}
		if authorization.owner.state != controllerPersonaIdentityAuthorizationQuarantined {
			t.Fatalf("ABA state = %d", authorization.owner.state)
		}
	})

	t.Run("post-consumption constructor failure quarantines", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		authorization.owner.config.CurrentStatus = NewWiredNoBatteryStatus(true)
		if _, err := NewAuthorizedControllerPersonaEngine(authorization, 0); !errors.Is(
			err, ErrInvalidControllerPersonaConfig) {
			t.Fatalf("construction error = %v", err)
		}
		if authorization.owner.state != controllerPersonaIdentityAuthorizationQuarantined {
			t.Fatalf("failure state = %d", authorization.owner.state)
		}
		if _, err := NewAuthorizedControllerPersonaEngine(authorization, 0); !errors.Is(
			err, ErrInvalidControllerIdentityAuthorization) {
			t.Fatalf("quarantined retry error = %v", err)
		}
	})

	t.Run("malformed private UTF-16 length is quarantined", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		authorization.owner.binding.descriptors.product.size--
		if _, err := NewAuthorizedControllerPersonaEngine(authorization, 0); !errors.Is(
			err, ErrInvalidControllerIdentityAuthorization) {
			t.Fatalf("malformed descriptor error = %v", err)
		}
		if authorization.owner.state != controllerPersonaIdentityAuthorizationQuarantined {
			t.Fatalf("malformed descriptor state = %d", authorization.owner.state)
		}
	})

	t.Run("malformed private UTF-16 surrogate is quarantined", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		authorization.owner.binding.descriptors.product.wire[2] = 0x00
		authorization.owner.binding.descriptors.product.wire[3] = 0xd8
		if _, err := NewAuthorizedControllerPersonaEngine(authorization, 0); !errors.Is(
			err, ErrInvalidControllerIdentityAuthorization) {
			t.Fatalf("malformed surrogate error = %v", err)
		}
		if authorization.owner.state != controllerPersonaIdentityAuthorizationQuarantined {
			t.Fatalf("malformed surrogate state = %d", authorization.owner.state)
		}
	})

	t.Run("engine and control-plane copies lose authority", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		engine, err := NewAuthorizedControllerPersonaEngine(authorization, 0)
		if err != nil {
			t.Fatal(err)
		}
		copiedEngine := *engine
		if got := copiedEngine.CapabilityBlockers(); got != controllerPersonaKnownBlockers {
			t.Fatalf("copied engine blockers = 0x%x", got)
		}
		setup := testUSBSetup(usbRequestTypeDeviceIn, usbRequestGetDescriptor,
			uint16(usbDescriptorTypeString)<<8|1,
			USBEnglishUnitedStatesLanguageID, 0xffff)
		if _, err := copiedEngine.ClaimUSBControl(setup); !errors.Is(
			err, ErrInvalidControllerIdentityAuthorization) ||
			!errors.Is(err, ErrUSBControlRequestStalled) {
			t.Fatalf("copied engine string error = %v", err)
		}
		copiedPlane := engine.usb
		if _, err := copiedPlane.Claim(setup); !errors.Is(
			err, ErrInvalidControllerIdentityAuthorization) ||
			!errors.Is(err, ErrUSBControlRequestStalled) {
			t.Fatalf("copied plane string error = %v", err)
		}
		if _, got := deliverAuthorizedUSBString(t, engine, 1,
			USBEnglishUnitedStatesLanguageID, 0xffff); len(got) == 0 {
			t.Fatal("canonical engine lost string authority after copy attempts")
		}
	})
}

func TestControllerIdentityAuthorizationConcurrentCopiesAdmitOne(t *testing.T) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	const contenders = 32
	start := make(chan struct{})
	var group sync.WaitGroup
	var successes atomic.Int32
	var stale atomic.Int32
	var unexpected atomic.Int32
	group.Add(contenders)
	for range contenders {
		copied := authorization
		go func() {
			defer group.Done()
			<-start
			_, err := NewAuthorizedControllerPersonaEngine(copied, 0)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrStaleControllerIdentityAuthorization):
				stale.Add(1)
			default:
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	group.Wait()
	if successes.Load() != 1 || stale.Load() != contenders-1 || unexpected.Load() != 0 {
		t.Fatalf("success/stale/unexpected = %d/%d/%d",
			successes.Load(), stale.Load(), unexpected.Load())
	}
}

func TestAuthorizedUSBIdentityWarmPathAllocatesZero(t *testing.T) {
	deniedConfig := testOnlyControllerPersonaConfig(t)
	deniedStrings := testOnlyIdentityStrings()
	if allocations := testing.AllocsPerRun(1000, func() {
		if _, err := NewAuthorizedControllerPersonaConfig(
			deniedConfig, deniedStrings,
			ControllerIdentityAuthorizationDenied); !errors.Is(
			err, ErrControllerIdentityAuthorizationDenied) {
			panic(err)
		}
	}); allocations != 0 {
		t.Fatalf("denied authorization allocations = %v, want 0", allocations)
	}

	authorization := testOnlyAuthorizedPersonaConfig(t)
	engine, err := NewAuthorizedControllerPersonaEngine(authorization, 0)
	if err != nil {
		t.Fatal(err)
	}
	setup := [usbSetupPacketSize]byte{
		usbRequestTypeDeviceIn, usbRequestGetDescriptor,
		1, usbDescriptorTypeString,
		0x09, 0x04,
		0xff, 0,
	}
	var response [usbMaximumStringDescriptorSize]byte
	if allocations := testing.AllocsPerRun(1000, func() {
		claim, err := engine.ClaimUSBControl(setup[:])
		if err != nil {
			panic(err)
		}
		if err := engine.AdmitAndCopy(claim, response[:claim.Size()], 0); err != nil {
			panic(err)
		}
		if err := engine.Resolve(claim, ControllerPersonaDelivered, 0); err != nil {
			panic(err)
		}
		if engine.CapabilityBlockers()&ControllerPersonaBlockerExternalUSBStrings != 0 {
			panic("authorized string blocker returned")
		}
	}); allocations != 0 {
		t.Fatalf("authorized USB identity warm allocations = %v, want 0", allocations)
	}
}
