package ns2pro

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// RuntimeStatusVersionV1 is the first out-of-band NS2 Pro status
	// contract. It is deliberately separate from the fixed 24-byte input
	// stream.
	RuntimeStatusVersionV1 uint16 = 1

	minimumRuntimeBatteryMillivolts uint16 = 2500
	maximumRuntimeBatteryMillivolts uint16 = 5000
)

var (
	ErrInvalidRuntimeStatusVersion = errors.New("invalid ns2pro runtime status version")
	ErrInvalidRuntimeBatteryLevel  = errors.New("invalid ns2pro runtime battery level")
	ErrInvalidRuntimeBatteryVolts  = errors.New("invalid ns2pro runtime battery voltage")
	ErrIncompleteRuntimeStatus     = errors.New("incomplete ns2pro runtime status")
)

// RuntimeStatusV1 is the complete mutable power-status snapshot exposed by
// the versioned control plane. Serial identity and the 24-byte input-state
// wire contract are intentionally outside this type.
type RuntimeStatusV1 struct {
	Version       uint16 `json:"version"`
	BatteryLevel  uint8  `json:"batteryLevel"`
	Charging      bool   `json:"charging"`
	ExternalPower bool   `json:"externalPower"`
	BatteryVolts  uint16 `json:"batteryVolts"`
}

func (status RuntimeStatusV1) Validate() error {
	if status.Version != RuntimeStatusVersionV1 {
		return fmt.Errorf("%w: %d", ErrInvalidRuntimeStatusVersion,
			status.Version)
	}
	if status.BatteryLevel > BatteryMax {
		return fmt.Errorf("%w: %d", ErrInvalidRuntimeBatteryLevel,
			status.BatteryLevel)
	}
	if status.BatteryVolts < minimumRuntimeBatteryMillivolts ||
		status.BatteryVolts > maximumRuntimeBatteryMillivolts {
		return fmt.Errorf("%w: %d", ErrInvalidRuntimeBatteryVolts,
			status.BatteryVolts)
	}
	return nil
}

type runtimeStatusV1Wire struct {
	Version       *uint16 `json:"version"`
	BatteryLevel  *uint8  `json:"batteryLevel"`
	Charging      *bool   `json:"charging"`
	ExternalPower *bool   `json:"externalPower"`
	BatteryVolts  *uint16 `json:"batteryVolts"`
}

// DecodeRuntimeStatusV1 accepts exactly one strict JSON object. Required
// false and zero values remain distinguishable from omitted fields.
func DecodeRuntimeStatusV1(payload string) (RuntimeStatusV1, error) {
	decoder := json.NewDecoder(strings.NewReader(payload))
	var wire runtimeStatusV1Wire
	opening, err := decoder.Token()
	if err != nil {
		return RuntimeStatusV1{}, fmt.Errorf(
			"decode ns2pro runtime status opening token: %w", err)
	}
	if opening != json.Delim('{') {
		return RuntimeStatusV1{}, errors.New(
			"decode ns2pro runtime status: expected object")
	}
	seen := make(map[string]struct{}, 5)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return RuntimeStatusV1{}, fmt.Errorf(
				"decode ns2pro runtime status field: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return RuntimeStatusV1{}, errors.New(
				"decode ns2pro runtime status: non-string field name")
		}
		if _, duplicate := seen[key]; duplicate {
			return RuntimeStatusV1{}, fmt.Errorf(
				"decode ns2pro runtime status: duplicate field %q", key)
		}
		seen[key] = struct{}{}
		switch key {
		case "version":
			err = decoder.Decode(&wire.Version)
		case "batteryLevel":
			err = decoder.Decode(&wire.BatteryLevel)
		case "charging":
			err = decoder.Decode(&wire.Charging)
		case "externalPower":
			err = decoder.Decode(&wire.ExternalPower)
		case "batteryVolts":
			err = decoder.Decode(&wire.BatteryVolts)
		default:
			return RuntimeStatusV1{}, fmt.Errorf(
				"decode ns2pro runtime status: unknown field %q", key)
		}
		if err != nil {
			return RuntimeStatusV1{}, fmt.Errorf(
				"decode ns2pro runtime status field %q: %w", key, err)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return RuntimeStatusV1{}, fmt.Errorf(
			"decode ns2pro runtime status closing token: %w", err)
	}
	if closing != json.Delim('}') {
		return RuntimeStatusV1{}, errors.New(
			"decode ns2pro runtime status: expected object end")
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return RuntimeStatusV1{}, errors.New(
				"decode ns2pro runtime status: trailing JSON value")
		}
		return RuntimeStatusV1{}, fmt.Errorf(
			"decode ns2pro runtime status trailing data: %w", err)
	}
	if wire.Version == nil || wire.BatteryLevel == nil ||
		wire.Charging == nil || wire.ExternalPower == nil ||
		wire.BatteryVolts == nil {
		return RuntimeStatusV1{}, ErrIncompleteRuntimeStatus
	}
	status := RuntimeStatusV1{
		Version:       *wire.Version,
		BatteryLevel:  *wire.BatteryLevel,
		Charging:      *wire.Charging,
		ExternalPower: *wire.ExternalPower,
		BatteryVolts:  *wire.BatteryVolts,
	}
	if err := status.Validate(); err != nil {
		return RuntimeStatusV1{}, err
	}
	return status, nil
}
