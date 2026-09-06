package xboxone

import (
	"crypto/sha256"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAuthorizedRetainedAuthorityID = uint64(0xA701)
	testAuthorizedRetainedDeviceID    = uint64(0xD701)
)

type authorizedRetainedValueExecutorMethods struct{}

func (authorizedRetainedValueExecutorMethods) Execute(
	ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (authorizedRetainedValueExecutorMethods) ResetAndDrain(time.Time) error {
	return nil
}

func (authorizedRetainedValueExecutorMethods) ResetNeutral(
	ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (authorizedRetainedValueExecutorMethods) CancelAndDrain(time.Time) error {
	return nil
}

func (authorizedRetainedValueExecutorMethods) DisconnectNeutral(
	ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

type nonComparableAuthorizedRetainedExecutor struct {
	authorizedRetainedValueExecutorMethods
	storage []byte
}

type runtimeUncomparableAuthorizedRetainedExecutor struct {
	authorizedRetainedValueExecutorMethods
	state any
}

type equalValueAuthorizedRetainedExecutor struct {
	authorizedRetainedValueExecutorMethods
	marker byte
}

type arrayAuthorizedRetainedExecutor [1]byte

func (*arrayAuthorizedRetainedExecutor) Execute(
	ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (*arrayAuthorizedRetainedExecutor) ResetAndDrain(time.Time) error {
	return nil
}

func (*arrayAuthorizedRetainedExecutor) ResetNeutral(
	ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (*arrayAuthorizedRetainedExecutor) CancelAndDrain(time.Time) error {
	return nil
}

func (*arrayAuthorizedRetainedExecutor) DisconnectNeutral(
	ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func TestAuthorizedRetainedUSBCompositionOwnsExactEngineWithoutPublicAlias(
	t *testing.T,
) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	copied := authorization
	executor := newScriptedControllerPersonaLocalExecutor()
	adapter, err := NewAuthorizedDormantRetainedUSBAdapter(
		authorization,
		73,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		executor,
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("NewAuthorizedDormantRetainedUSBAdapter: %v", err)
	}
	if adapter == nil || adapter.coordinator == nil ||
		adapter.coordinator.engine == nil {
		t.Fatal("composition did not retain one exact engine")
	}
	engine := adapter.coordinator.engine
	if authorization.owner.binding.engine != engine ||
		authorization.owner.binding.controlPlane != &engine.usb {
		t.Fatal("adapter does not own the authorization-bound engine/control plane")
	}
	if adapter.expectedAuthority != testAuthorizedRetainedAuthorityID ||
		adapter.expectedDevice != testAuthorizedRetainedDeviceID ||
		adapter.local != executor || adapter.protocolTimeMS != 73 ||
		adapter.clockOriginMS != 73 || adapter.state != dormantRetainedUSBUnbound {
		t.Fatal("adapter did not preserve exact composition inputs")
	}
	if blockers := engine.CapabilityBlockers(); blockers&(ControllerPersonaBlockerExternalUSBStrings|
		ControllerPersonaBlockerIdentityAuthorization) != 0 {
		t.Fatalf("authorized identity blockers remain set: %#x", blockers)
	}
	if _, err := NewAuthorizedControllerPersonaEngine(copied, 73); !errors.Is(
		err, ErrStaleControllerIdentityAuthorization) {
		t.Fatalf("copied authorization minted a second engine: %v", err)
	}

	factoryType := reflect.TypeOf(NewAuthorizedDormantRetainedUSBAdapter)
	wantAdapter := reflect.TypeOf((*DormantRetainedUSBAdapter)(nil))
	wantEngine := reflect.TypeOf((*ControllerPersonaEngine)(nil))
	if factoryType.NumOut() != 2 || factoryType.Out(0) != wantAdapter ||
		factoryType.Out(1) != reflect.TypeOf((*error)(nil)).Elem() {
		t.Fatalf("unexpected public composition signature: %v", factoryType)
	}
	adapterType := reflect.TypeOf(adapter)
	for index := 0; index < adapterType.NumMethod(); index++ {
		method := adapterType.Method(index)
		for output := 0; output < method.Type.NumOut(); output++ {
			if method.Type.Out(output) == wantEngine {
				t.Fatalf("exported adapter method %s leaks the engine alias", method.Name)
			}
		}
	}
}

func TestAuthorizedRetainedUSBCompositionPreflightDoesNotConsumeAuthorization(
	t *testing.T,
) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	executor := newScriptedControllerPersonaLocalExecutor()
	if _, err := NewAuthorizedDormantRetainedUSBAdapter(
		authorization,
		0,
		0,
		testAuthorizedRetainedDeviceID,
		executor,
		100*time.Millisecond,
	); !errors.Is(err, errDormantRetainedUSBUninitialized) {
		t.Fatalf("invalid preflight error = %v", err)
	}
	if _, err := NewAuthorizedDormantRetainedUSBAdapter(
		authorization,
		0,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		executor,
		100*time.Millisecond,
	); err != nil {
		t.Fatalf("preflight consumed reusable authorization: %v", err)
	}
}

func TestAuthorizedRetainedUSBCompositionRejectsUnprovableExecutorIdentityBeforeConsumption(
	t *testing.T,
) {
	t.Run("typed nil", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		var typedNil *scriptedControllerPersonaLocalExecutor
		if _, err := NewAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			typedNil,
			100*time.Millisecond,
		); !errors.Is(err, errDormantRetainedUSBUninitialized) {
			t.Fatalf("typed-nil preflight error = %v", err)
		}
		assertAuthorizationState(t, authorization,
			controllerPersonaIdentityAuthorizationIssued)
	})

	t.Run("statically non-comparable value", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		executor := nonComparableAuthorizedRetainedExecutor{
			storage: []byte{1},
		}
		if _, err := NewAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			executor,
			100*time.Millisecond,
		); !errors.Is(err, errDormantRetainedUSBUninitialized) {
			t.Fatalf("non-comparable preflight error = %v", err)
		}
		assertAuthorizationState(t, authorization,
			controllerPersonaIdentityAuthorizationIssued)
	})

	t.Run("statically comparable runtime-poisoned value", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		executor := runtimeUncomparableAuthorizedRetainedExecutor{
			state: []byte{1},
		}
		if !reflect.TypeOf(executor).Comparable() {
			t.Fatal("repro type is not statically comparable")
		}
		panicked := func() (panicked bool) {
			defer func() {
				panicked = recover() != nil
			}()
			var left ControllerPersonaLocalExecutor = executor
			var right ControllerPersonaLocalExecutor = executor
			_ = left == right
			return false
		}()
		if !panicked {
			t.Fatal("repro did not panic under interface equality")
		}
		if _, err := NewAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			executor,
			100*time.Millisecond,
		); !errors.Is(err, errDormantRetainedUSBUninitialized) {
			t.Fatalf("runtime-poisoned preflight error = %v", err)
		}
		assertAuthorizationState(t, authorization,
			controllerPersonaIdentityAuthorizationIssued)
	})

	t.Run("equal distinct values have no exact object identity", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		first := equalValueAuthorizedRetainedExecutor{marker: 1}
		second := equalValueAuthorizedRetainedExecutor{marker: 1}
		if first != second {
			t.Fatal("repro values are not equal")
		}
		if validAuthorizedRetainedLocalExecutor(first) ||
			validAuthorizedRetainedLocalExecutor(second) {
			t.Fatal("equal value executors acquired object authority")
		}
		if _, err := NewAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			first,
			100*time.Millisecond,
		); !errors.Is(err, errDormantRetainedUSBUninitialized) {
			t.Fatalf("equal-value preflight error = %v", err)
		}
		assertAuthorizationState(t, authorization,
			controllerPersonaIdentityAuthorizationIssued)
	})

	t.Run("zero-sized pointer has no exact object identity", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		first := &authorizedRetainedValueExecutorMethods{}
		second := &authorizedRetainedValueExecutorMethods{}
		firstValue := reflect.ValueOf(first)
		secondValue := reflect.ValueOf(second)
		if firstValue.Type().Elem().Size() != 0 {
			t.Fatal("zero-size executor fixture has nonzero pointee size")
		}
		if firstValue.Type() == secondValue.Type() &&
			firstValue.Pointer() == secondValue.Pointer() {
			t.Log("runtime assigned distinct zero-size objects the same address")
		}
		if validAuthorizedRetainedLocalExecutor(first) ||
			validAuthorizedRetainedLocalExecutor(second) ||
			sameAuthorizedRetainedLocalExecutor(first, second) {
			t.Fatal("zero-size pointer executors acquired object authority")
		}
		if _, err := NewAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			first,
			100*time.Millisecond,
		); !errors.Is(err, errDormantRetainedUSBUninitialized) {
			t.Fatalf("zero-size pointer preflight error = %v", err)
		}
		assertAuthorizationState(t, authorization,
			controllerPersonaIdentityAuthorizationIssued)
		if _, err := NewAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			newScriptedControllerPersonaLocalExecutor(),
			100*time.Millisecond,
		); err != nil {
			t.Fatalf("zero-size preflight consumed authorization: %v", err)
		}
	})

	t.Run("nonzero array pointer is exact and typed nil remains invalid", func(t *testing.T) {
		executor := &arrayAuthorizedRetainedExecutor{1}
		if reflect.TypeOf(executor).Elem().Kind() != reflect.Array ||
			reflect.TypeOf(executor).Elem().Size() == 0 ||
			!validAuthorizedRetainedLocalExecutor(executor) {
			t.Fatal("nonzero array-backed pointer lost exact executor authority")
		}
		var typedNil *arrayAuthorizedRetainedExecutor
		if validAuthorizedRetainedLocalExecutor(typedNil) {
			t.Fatal("typed-nil array pointer acquired executor authority")
		}
	})
}

func TestAuthorizedRetainedUSBCompositionCopiedAuthorizationHasOneWinner(
	t *testing.T,
) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	const contenders = 32
	start := make(chan struct{})
	results := make(chan struct {
		adapter *DormantRetainedUSBAdapter
		err     error
	}, contenders)
	var group sync.WaitGroup
	group.Add(contenders)
	for index := 0; index < contenders; index++ {
		go func() {
			defer group.Done()
			<-start
			adapter, err := NewAuthorizedDormantRetainedUSBAdapter(
				authorization,
				0,
				testAuthorizedRetainedAuthorityID,
				testAuthorizedRetainedDeviceID,
				newScriptedControllerPersonaLocalExecutor(),
				100*time.Millisecond,
			)
			results <- struct {
				adapter *DormantRetainedUSBAdapter
				err     error
			}{adapter: adapter, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
			if result.adapter == nil || result.adapter.coordinator == nil ||
				result.adapter.coordinator.engine == nil {
				t.Fatal("winner did not retain the exact engine")
			}
			continue
		}
		if result.adapter != nil || !errors.Is(
			result.err, ErrStaleControllerIdentityAuthorization) {
			t.Fatalf("loser escaped authority or wrong error: adapter=%p err=%v",
				result.adapter, result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful compositions = %d, want 1", successes)
	}
}

func TestAuthorizedRetainedUSBCompositionDelayedCopyCannotReplayAcrossNewOwner(
	t *testing.T,
) {
	first := testOnlyAuthorizedPersonaConfig(t)
	delayedFirstCopy := first
	firstAdapter, err := NewAuthorizedDormantRetainedUSBAdapter(
		first,
		11,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("first composition: %v", err)
	}

	second := testOnlyAuthorizedPersonaConfig(t)
	secondAdapter, err := NewAuthorizedDormantRetainedUSBAdapter(
		second,
		12,
		testAuthorizedRetainedAuthorityID+1,
		testAuthorizedRetainedDeviceID+1,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("second composition: %v", err)
	}
	if firstAdapter == secondAdapter || firstAdapter.Identity() == secondAdapter.Identity() ||
		firstAdapter.coordinator.engine == secondAdapter.coordinator.engine {
		t.Fatal("fresh owner reused a prior composition identity")
	}

	if replayed, replayErr := NewAuthorizedDormantRetainedUSBAdapter(
		delayedFirstCopy,
		13,
		testAuthorizedRetainedAuthorityID+2,
		testAuthorizedRetainedDeviceID+2,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
	); replayed != nil || !errors.Is(
		replayErr, ErrStaleControllerIdentityAuthorization) {
		t.Fatalf("delayed first-owner copy replayed: adapter=%p err=%v",
			replayed, replayErr)
	}
}

func TestAuthorizedRetainedUSBCompositionFailureRevokesPartialEngineAndCopies(
	t *testing.T,
) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	copied := authorization
	injected := errors.New("injected adapter construction failure")
	var leakedForTest *ControllerPersonaEngine
	adapter, err := newAuthorizedDormantRetainedUSBAdapter(
		authorization,
		0,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
		func(engine *ControllerPersonaEngine) error {
			leakedForTest = engine
			return injected
		},
	)
	if adapter != nil || !errors.Is(err, ErrInvalidAuthorizedRetainedUSBComposition) ||
		!errors.Is(err, injected) {
		t.Fatalf("partial failure shape: adapter=%p err=%v", adapter, err)
	}
	assertAuthorizedRetainedCompositionQuarantined(t, authorization, leakedForTest)
	if _, err := NewAuthorizedDormantRetainedUSBAdapter(
		copied,
		0,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
	); !errors.Is(err, ErrInvalidControllerIdentityAuthorization) {
		t.Fatalf("copied authorization survived partial failure: %v", err)
	}
}

func TestAuthorizedRetainedUSBCompositionContradictionAndPanicQuarantine(
	t *testing.T,
) {
	t.Run("valid descriptor substitution", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		var leakedForTest *ControllerPersonaEngine
		adapter, err := newAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			newScriptedControllerPersonaLocalExecutor(),
			100*time.Millisecond,
			func(engine *ControllerPersonaEngine) error {
				leakedForTest = engine
				substitute, encodeErr := encodeExternalUSBStringDescriptor(
					"product", "Substituted Valid Product")
				if encodeErr != nil || !substitute.validate() ||
					substitute == engine.identityBinding.descriptors.product {
					t.Fatalf("invalid descriptor exploit fixture: %v", encodeErr)
				}
				engine.identityBinding.descriptors.product = substitute
				return nil
			},
		)
		if adapter != nil || !errors.Is(
			err, ErrInvalidAuthorizedRetainedUSBComposition) {
			t.Fatalf("descriptor substitution escaped: adapter=%p err=%v",
				adapter, err)
		}
		assertAuthorizedRetainedCompositionQuarantined(
			t, authorization, leakedForTest)
	})

	t.Run("metadata in-place mutation plus digest substitution", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		var leakedForTest *ControllerPersonaEngine
		adapter, err := newAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			newScriptedControllerPersonaLocalExecutor(),
			100*time.Millisecond,
			func(engine *ControllerPersonaEngine) error {
				leakedForTest = engine
				if len(engine.metadata.data) == 0 {
					t.Fatal("metadata exploit fixture is empty")
				}
				engine.metadata.data[0] ^= 0xff
				engine.identityBinding.metadataDigest =
					sha256.Sum256(engine.metadata.data)
				return nil
			},
		)
		if adapter != nil || !errors.Is(
			err, ErrInvalidAuthorizedRetainedUSBComposition) {
			t.Fatalf("metadata/digest substitution escaped: adapter=%p err=%v",
				adapter, err)
		}
		assertAuthorizedRetainedCompositionQuarantined(
			t, authorization, leakedForTest)
	})

	t.Run("equal metadata storage substitution", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		var leakedForTest *ControllerPersonaEngine
		adapter, err := newAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			newScriptedControllerPersonaLocalExecutor(),
			100*time.Millisecond,
			func(engine *ControllerPersonaEngine) error {
				leakedForTest = engine
				replacement := append([]byte(nil), engine.metadata.data...)
				if len(replacement) == 0 ||
					&replacement[0] == &engine.metadata.data[0] {
					t.Fatal("metadata storage exploit fixture aliases")
				}
				engine.metadata.data = replacement
				return nil
			},
		)
		if adapter != nil || !errors.Is(
			err, ErrInvalidAuthorizedRetainedUSBComposition) {
			t.Fatalf("equal metadata storage substitution escaped: adapter=%p err=%v",
				adapter, err)
		}
		assertAuthorizedRetainedCompositionQuarantined(
			t, authorization, leakedForTest)
	})

	t.Run("engine clock split", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		var leakedForTest *ControllerPersonaEngine
		adapter, err := newAuthorizedDormantRetainedUSBAdapter(
			authorization,
			19,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			newScriptedControllerPersonaLocalExecutor(),
			100*time.Millisecond,
			func(engine *ControllerPersonaEngine) error {
				leakedForTest = engine
				engine.lastNowMS++
				return nil
			},
		)
		if adapter != nil || !errors.Is(
			err, ErrInvalidAuthorizedRetainedUSBComposition) {
			t.Fatalf("engine clock split escaped: adapter=%p err=%v", adapter, err)
		}
		assertAuthorizedRetainedCompositionQuarantined(
			t, authorization, leakedForTest)
	})

	t.Run("engine lifecycle generation split", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		var leakedForTest *ControllerPersonaEngine
		adapter, err := newAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			newScriptedControllerPersonaLocalExecutor(),
			100*time.Millisecond,
			func(engine *ControllerPersonaEngine) error {
				leakedForTest = engine
				engine.generation++
				engine.lifecycle = restartControllerLifecycle(
					engine.lifecycle, engine.generation, engine.lastNowMS)
				return nil
			},
		)
		if adapter != nil || !errors.Is(
			err, ErrInvalidAuthorizedRetainedUSBComposition) {
			t.Fatalf("lifecycle split escaped: adapter=%p err=%v", adapter, err)
		}
		assertAuthorizedRetainedCompositionQuarantined(
			t, authorization, leakedForTest)
	})

	t.Run("binding engine corruption plus error", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		injected := errors.New("binding corruption error")
		var leakedForTest *ControllerPersonaEngine
		adapter, err := newAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			newScriptedControllerPersonaLocalExecutor(),
			100*time.Millisecond,
			func(engine *ControllerPersonaEngine) error {
				leakedForTest = engine
				engine.identityBinding.engine = nil
				return injected
			},
		)
		if adapter != nil ||
			!errors.Is(err, ErrInvalidAuthorizedRetainedUSBComposition) ||
			!errors.Is(err, injected) {
			t.Fatalf("binding corruption error shape: adapter=%p err=%v", adapter, err)
		}
		assertAuthorizedRetainedCompositionQuarantined(
			t, authorization, leakedForTest)
	})

	t.Run("binding owner corruption plus error", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		injected := errors.New("binding owner corruption error")
		var leakedForTest *ControllerPersonaEngine
		adapter, err := newAuthorizedDormantRetainedUSBAdapter(
			authorization,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			newScriptedControllerPersonaLocalExecutor(),
			100*time.Millisecond,
			func(engine *ControllerPersonaEngine) error {
				leakedForTest = engine
				authorization.owner.binding = nil
				return injected
			},
		)
		if adapter != nil ||
			!errors.Is(err, ErrInvalidAuthorizedRetainedUSBComposition) ||
			!errors.Is(err, injected) {
			t.Fatalf("owner corruption error shape: adapter=%p err=%v", adapter, err)
		}
		assertAuthorizedRetainedCompositionQuarantined(
			t, authorization, leakedForTest)
	})

	t.Run("panic", func(t *testing.T) {
		authorization := testOnlyAuthorizedPersonaConfig(t)
		var leakedForTest *ControllerPersonaEngine
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("injected constructor panic did not propagate")
				}
			}()
			_, _ = newAuthorizedDormantRetainedUSBAdapter(
				authorization,
				0,
				testAuthorizedRetainedAuthorityID,
				testAuthorizedRetainedDeviceID,
				newScriptedControllerPersonaLocalExecutor(),
				100*time.Millisecond,
				func(engine *ControllerPersonaEngine) error {
					leakedForTest = engine
					engine.identityBinding.engine = nil
					panic("injected adapter constructor panic")
				},
			)
		}()
		assertAuthorizedRetainedCompositionQuarantined(
			t, authorization, leakedForTest)
	})
}

func TestAuthorizedRetainedUSBCompositionRequiresExactExecutorPointerAndCanonicalProof(
	t *testing.T,
) {
	zeroFirst := &authorizedRetainedValueExecutorMethods{}
	zeroSecond := &authorizedRetainedValueExecutorMethods{}
	if validAuthorizedRetainedLocalExecutor(zeroFirst) ||
		validAuthorizedRetainedLocalExecutor(zeroSecond) ||
		sameAuthorizedRetainedLocalExecutor(zeroFirst, zeroSecond) {
		t.Fatal("zero-size pointer passed executor identity authentication")
	}
	zeroAuthorization := testOnlyAuthorizedPersonaConfig(t)
	zeroEngine, err := NewAuthorizedControllerPersonaEngine(
		zeroAuthorization, 36)
	if err != nil {
		t.Fatalf("zero-size proof engine: %v", err)
	}
	zeroAdapter, zeroProof, err := newDormantRetainedUSBAdapter(
		zeroEngine,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		zeroFirst,
		100*time.Millisecond,
		authorizedRetainedUSBConstructionAuthority,
	)
	if err != nil || zeroAdapter == nil || zeroProof != nil {
		t.Fatalf("zero-size proof request: adapter=%p proof=%p err=%v",
			zeroAdapter, zeroProof, err)
	}

	requested := &equalValueAuthorizedRetainedExecutor{marker: 1}
	equalButForeign := &equalValueAuthorizedRetainedExecutor{marker: 1}
	if !reflect.DeepEqual(*requested, *equalButForeign) {
		t.Fatal("pointer-identity repro values differ")
	}
	if requested == equalButForeign {
		t.Fatal("pointer-identity repro unexpectedly aliases")
	}
	if !validAuthorizedRetainedLocalExecutor(requested) ||
		!validAuthorizedRetainedLocalExecutor(equalButForeign) ||
		sameAuthorizedRetainedLocalExecutor(requested, equalButForeign) {
		t.Fatal("equal but distinct executor pointers collapsed to one owner")
	}

	authorization := testOnlyAuthorizedPersonaConfig(t)
	engine, err := NewAuthorizedControllerPersonaEngine(authorization, 37)
	if err != nil {
		t.Fatalf("authorized engine: %v", err)
	}
	adapter, proof, err := newDormantRetainedUSBAdapter(
		engine,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		requested,
		100*time.Millisecond,
		authorizedRetainedUSBConstructionAuthority,
	)
	if err != nil {
		t.Fatalf("canonical adapter: %v", err)
	}
	if adapter.consumeAuthorizedRetainedUSBConstructionProof(
		proof,
		engine,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		37,
		equalButForeign,
		100*time.Millisecond,
	) {
		t.Fatal("canonical proof accepted an equal but foreign executor pointer")
	}
	if proof == nil || proof.consumed {
		t.Fatal("failed authentication consumed the canonical proof")
	}
	if !adapter.consumeAuthorizedRetainedUSBConstructionProof(
		proof,
		engine,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		37,
		requested,
		100*time.Millisecond,
	) {
		t.Fatal("exact canonical proof was rejected")
	}
	if !proof.consumed || adapter.consumeAuthorizedRetainedUSBConstructionProof(
		proof,
		engine,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		37,
		requested,
		100*time.Millisecond,
	) {
		t.Fatal("canonical construction proof was replayable")
	}

	// Mirror every formerly checked subset field. Without the private canonical
	// issuance this hand-built shape still cannot be adopted.
	fake := &DormantRetainedUSBAdapter{
		identity: 1, expectedAuthority: testAuthorizedRetainedAuthorityID,
		expectedDevice: testAuthorizedRetainedDeviceID,
		generation:     1, protocolTimeMS: 37, clockOriginMS: 37,
		coordinator: adapter.coordinator, local: requested,
		localTimeout: 100 * time.Millisecond,
		state:        dormantRetainedUSBUnbound,
	}
	if fake.consumeAuthorizedRetainedUSBConstructionProof(
		nil,
		engine,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		37,
		requested,
		100*time.Millisecond,
	) {
		t.Fatal("hand-built adapter passed canonical construction authentication")
	}
}

func TestDormantRetainedUSBDirectConstructionRetainsNoCompositionProof(
	t *testing.T,
) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	engine, err := NewAuthorizedControllerPersonaEngine(authorization, 0)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewDormantRetainedUSBAdapter(
		engine,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
	)
	if err != nil || adapter == nil {
		t.Fatalf("direct constructor: adapter=%p err=%v", adapter, err)
	}
	proofType := reflect.TypeOf((*dormantRetainedUSBConstructionProof)(nil))
	adapterType := reflect.TypeOf(adapter).Elem()
	for index := 0; index < adapterType.NumField(); index++ {
		if adapterType.Field(index).Type == proofType {
			t.Fatalf("direct adapter retains ambient proof field %s",
				adapterType.Field(index).Name)
		}
	}

	forgedAuthorization := testOnlyAuthorizedPersonaConfig(t)
	forgedEngine, err := NewAuthorizedControllerPersonaEngine(
		forgedAuthorization, 0)
	if err != nil {
		t.Fatal(err)
	}
	forgedAuthority := &dormantRetainedUSBConstructionAuthority{marker: 1}
	forgedAdapter, forgedProof, err := newDormantRetainedUSBAdapter(
		forgedEngine,
		testAuthorizedRetainedAuthorityID+1,
		testAuthorizedRetainedDeviceID+1,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
		forgedAuthority,
	)
	if err != nil || forgedAdapter == nil || forgedProof != nil {
		t.Fatalf("foreign construction authority shape: adapter=%p proof=%p err=%v",
			forgedAdapter, forgedProof, err)
	}
}

func TestAuthorizedRetainedUSBCompositionFactoryHasOneExactDeviceCallSite(
	t *testing.T,
) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	var publicCallSites []string
	var privateCallSites []string
	var deviceCallSites []string
	var publicReferences []string
	var privateReferences []string
	var deviceReferences []string
	err := filepath.WalkDir(repositoryRoot, func(
		path string,
		entry fs.DirEntry,
		walkErr error,
	) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok {
				switch identifier.Name {
				case "NewAuthorizedDormantRetainedUSBAdapter":
					publicReferences = append(publicReferences, path)
				case "newAuthorizedDormantRetainedUSBAdapter":
					privateReferences = append(privateReferences, path)
				case "NewAuthorizedDormantRetainedUSBDevice":
					deviceReferences = append(deviceReferences, path)
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			called := ""
			switch function := call.Fun.(type) {
			case *ast.Ident:
				called = function.Name
			case *ast.SelectorExpr:
				called = function.Sel.Name
			}
			if called == "NewAuthorizedDormantRetainedUSBAdapter" {
				publicCallSites = append(publicCallSites, path)
			}
			if called == "newAuthorizedDormantRetainedUSBAdapter" {
				privateCallSites = append(privateCallSites, path)
			}
			if called == "NewAuthorizedDormantRetainedUSBDevice" {
				deviceCallSites = append(deviceCallSites, path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPublicCallSuffix :=
		"device/xboxone/authorized_retained_usb_device.go"
	if len(publicCallSites) != 1 ||
		!strings.HasSuffix(
			filepath.ToSlash(publicCallSites[0]), wantPublicCallSuffix) {
		t.Fatalf("authorized composition factory has unexpected production call sites: %v",
			publicCallSites)
	}
	// The two public identifier occurrences are its declaration and the exact
	// retained-only usb.Device wrapper call. Counting every identifier, not only
	// CallExpr nodes, also detects assigning the factory to a function variable
	// and invoking it through an alias.
	if len(publicReferences) != 2 {
		t.Fatalf("authorized composition factory has unexpected references: %v",
			publicReferences)
	}
	wantPrivateSuffix :=
		"device/xboxone/authorized_retained_usb_composition.go"
	if len(privateCallSites) != 1 ||
		!strings.HasSuffix(
			filepath.ToSlash(privateCallSites[0]), wantPrivateSuffix) {
		t.Fatalf("private authorized composition boundary has unexpected call sites: %v",
			privateCallSites)
	}
	if len(privateReferences) != 2 {
		t.Fatalf("private authorized composition boundary has alias references: %v",
			privateReferences)
	}
	wantDeviceCallSuffix := "internal/registry/authorized_xboxone.go"
	if len(deviceCallSites) != 1 ||
		!strings.HasSuffix(
			filepath.ToSlash(deviceCallSites[0]), wantDeviceCallSuffix) {
		t.Fatalf("authorized retained device has unexpected production call sites: %v",
			deviceCallSites)
	}
	// Declaration plus the one explicit internal factory reference. Any alias
	// or second construction surface fails this repository-wide AST check.
	if len(deviceReferences) != 2 {
		t.Fatalf("authorized retained device has unexpected references: %v",
			deviceReferences)
	}
}

func TestAuthorizedRetainedUSBCompositionForgeryCannotQuarantineExactOwner(
	t *testing.T,
) {
	first := testOnlyAuthorizedPersonaConfig(t)
	second := testOnlyAuthorizedPersonaConfig(t)
	forged := first
	forged.credential = second.credential
	forged.profileIssuance = second.profileIssuance
	forged.metadataIssuance = second.metadataIssuance
	if _, err := NewAuthorizedDormantRetainedUSBAdapter(
		forged,
		0,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
	); !errors.Is(err, ErrInvalidControllerIdentityAuthorization) {
		t.Fatalf("forged authorization error = %v", err)
	}
	assertAuthorizationState(t, first,
		controllerPersonaIdentityAuthorizationIssued)
	if _, err := NewAuthorizedDormantRetainedUSBAdapter(
		first,
		0,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
	); err != nil {
		t.Fatalf("forgery damaged exact owner: %v", err)
	}
	if _, err := NewAuthorizedDormantRetainedUSBAdapter(
		second,
		0,
		testAuthorizedRetainedAuthorityID+1,
		testAuthorizedRetainedDeviceID+1,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
	); err != nil {
		t.Fatalf("forgery damaged foreign owner: %v", err)
	}
}

func TestAuthorizedRetainedUSBCompositionOldAndNewConstructorsShareOneShotOwner(
	t *testing.T,
) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	copied := authorization
	if _, err := NewAuthorizedControllerPersonaEngine(authorization, 0); err != nil {
		t.Fatalf("legacy authorized engine constructor: %v", err)
	}
	if _, err := NewAuthorizedDormantRetainedUSBAdapter(
		copied,
		0,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(),
		100*time.Millisecond,
	); !errors.Is(err, ErrStaleControllerIdentityAuthorization) {
		t.Fatalf("composition bypassed prior one-shot consumption: %v", err)
	}
}

func TestAuthorizedRetainedUSBCompositionWarmRejectedAndDiagnosticPathsAllocateNothing(
	t *testing.T,
) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	copied := authorization
	executor := newScriptedControllerPersonaLocalExecutor()
	adapter, err := NewAuthorizedDormantRetainedUSBAdapter(
		authorization,
		0,
		testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID,
		executor,
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}

	rejectedAllocs := testing.AllocsPerRun(1000, func() {
		candidate, err := NewAuthorizedDormantRetainedUSBAdapter(
			copied,
			0,
			testAuthorizedRetainedAuthorityID,
			testAuthorizedRetainedDeviceID,
			executor,
			100*time.Millisecond,
		)
		if candidate != nil || !errors.Is(
			err, ErrStaleControllerIdentityAuthorization) {
			panic("stale authorization changed shape")
		}
	})
	if rejectedAllocs != 0 {
		t.Fatalf("warm stale rejection allocations = %v, want 0", rejectedAllocs)
	}

	diagnosticAllocs := testing.AllocsPerRun(1000, func() {
		if adapter.Identity() == 0 || !adapter.Limits().Valid() {
			panic("invalid returned adapter diagnostics")
		}
	})
	if diagnosticAllocs != 0 {
		t.Fatalf("warm adapter diagnostic allocations = %v, want 0", diagnosticAllocs)
	}
}

func assertAuthorizedRetainedCompositionQuarantined(
	t *testing.T,
	authorization AuthorizedControllerPersonaConfig,
	engine *ControllerPersonaEngine,
) {
	t.Helper()
	if engine == nil {
		t.Fatal("test did not observe the partially constructed engine")
	}
	assertAuthorizationState(t, authorization,
		controllerPersonaIdentityAuthorizationQuarantined)
	if engine.identityBinding != nil && engine.identityBinding.valid {
		t.Fatal("quarantined authorization still authenticates external identity")
	}
	if engine.initialized || engine.usb.initialized ||
		engine.CapabilityBlockers() != controllerPersonaKnownBlockers {
		t.Fatal("partial engine remained operational or identity-authorized")
	}
	if _, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeDeviceIn,
		usbRequestGetDescriptor,
		uint16(usbDescriptorTypeString)<<8|1,
		USBEnglishUnitedStatesLanguageID,
		255,
	)); !errors.Is(err, ErrUninitializedControllerPersona) {
		t.Fatalf("revoked partial engine accepted EP0 identity work: %v", err)
	}
	if _, err := engine.ClaimNextLifecycleAction(^uint64(0)); !errors.Is(
		err, ErrUninitializedControllerPersona) {
		t.Fatalf("revoked partial engine accepted non-string lifecycle work: %v", err)
	}
}

func assertAuthorizationState(
	t *testing.T,
	authorization AuthorizedControllerPersonaConfig,
	want controllerPersonaIdentityAuthorizationState,
) {
	t.Helper()
	if authorization.owner == nil {
		t.Fatal("authorization has no owner")
	}
	authorization.owner.mu.Lock()
	defer authorization.owner.mu.Unlock()
	if got := authorization.owner.state; got != want {
		t.Fatalf("authorization state = %v, want %v", got, want)
	}
}
