package viipertypes

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"golang.org/x/exp/constraints"
)

// APIError represents an RFC 7807 (problem+json) error response.
type APIError struct {
	// Status is the HTTP-style status code (e.g., 400, 404, 500)
	Status int `json:"status"`
	// Title is a short, human-readable summary of the problem type
	Title string `json:"title"`
	// Detail is a human-readable explanation specific to this occurrence
	Detail string `json:"detail"`
}

func (e APIError) Error() string {
	if e.Status == 0 && e.Title == "" {
		return "unknown error"
	}
	if e.Status == 0 {
		return fmt.Sprintf("%s: %s", e.Title, e.Detail)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Title, e.Detail)
}

// --

type PingResponse struct {
	Server  string `json:"server"`
	Version string `json:"version"`
}

type BusListResponse struct {
	Buses []uint32 `json:"buses"`
}

type BusCreateResponse struct {
	BusID uint32 `json:"busId"`
}

type BusRemoveResponse struct {
	BusID uint32 `json:"busId"`
}

type Device struct {
	BusID            uint32         `json:"busId"`
	DevID            string         `json:"devId"`
	Vid              string         `json:"vid"`
	Pid              string         `json:"pid"`
	Type             string         `json:"type"`
	DeviceSpecific   map[string]any `json:"deviceSpecific"`
	USBIPPort        int32          `json:"usbipPort,omitempty"`
	USBIPOwnerSerial string         `json:"usbipOwnerSerial,omitempty"`
}

type DevicesListResponse struct {
	Devices []Device `json:"devices"`
}

type DeviceRemoveResponse struct {
	BusID uint32 `json:"busId"`
	DevID string `json:"devId"`
}

// NS2ProRuntimeStatusV1 is the complete mutable Switch 2 Pro power snapshot.
// It travels over the versioned management endpoint, never the fixed input
// stream.
type NS2ProRuntimeStatusV1 struct {
	Version       uint16 `json:"version"`
	BatteryLevel  uint8  `json:"batteryLevel"`
	Charging      bool   `json:"charging"`
	ExternalPower bool   `json:"externalPower"`
	BatteryVolts  uint16 `json:"batteryVolts"`
}

type NS2ProRuntimeStatusUpdateResponseV1 struct {
	Version uint16 `json:"version"`
	Updated bool   `json:"updated"`
}

type DeviceCreateRequest struct {
	Type           *string        `json:"type"`
	IDVendor       *uint16        `json:"idVendor,omitempty"`
	IDProduct      *uint16        `json:"idProduct,omitempty"`
	DeviceSpecific map[string]any `json:"deviceSpecific,omitempty"`
}

// XboxOneAuthorizedIdentityV1 preserves the JSON identity contract while making
// its complete structure available to the generated client libraries.
type XboxOneAuthorizedIdentityV1 struct {
	VendorID         uint16 `json:"vendorId"`
	ProductID        uint16 `json:"productId"`
	DeviceReleaseBCD uint16 `json:"deviceReleaseBcd"`
	DeviceID         uint64 `json:"deviceId"`
	FirmwareMajor    uint16 `json:"firmwareMajor"`
	FirmwareMinor    uint16 `json:"firmwareMinor"`
	FirmwareBuild    uint16 `json:"firmwareBuild"`
	FirmwareRevision uint16 `json:"firmwareRevision"`
	HardwareMajor    uint8  `json:"hardwareMajor"`
	HardwareMinor    uint8  `json:"hardwareMinor"`
}

type XboxOneAuthorizedUSBV1 struct {
	MaxPower2mA   uint16 `json:"maxPower2mA"`
	OUTIntervalMS uint16 `json:"outIntervalMs"`
	INIntervalMS  uint16 `json:"inIntervalMs"`
}

type XboxOneAuthorizedStringsV1 struct {
	Manufacturer string `json:"manufacturer"`
	Product      string `json:"product"`
	Serial       string `json:"serial"`
}

type XboxOneAuthorizedFeedbackV1 struct {
	Source                 uint8  `json:"source"`
	PersonaGeneration      uint64 `json:"personaGeneration"`
	DeviceGeneration       uint64 `json:"deviceGeneration"`
	TransportGeneration    uint64 `json:"transportGeneration"`
	OwnershipEpoch         uint64 `json:"ownershipEpoch"`
	TimeToLiveMicroseconds uint64 `json:"timeToLiveMicroseconds"`
}

// XboxOneAuthorizedCreateRequestV1 is the closed authenticated factory
// contract for one retained Xbox One/Series-class USB persona. It is separate
// from DeviceCreateRequest so generic deviceSpecific JSON cannot weaken the
// identity, lifecycle-generation, or feedback-ownership boundaries.
type XboxOneAuthorizedCreateRequestV1 struct {
	Version                      uint16                      `json:"version"`
	IdentityAuthorizationGranted bool                        `json:"identityAuthorizationGranted"`
	Identity                     XboxOneAuthorizedIdentityV1 `json:"identity"`
	USB                          XboxOneAuthorizedUSBV1      `json:"usb"`
	Strings                      XboxOneAuthorizedStringsV1  `json:"strings"`
	Feedback                     XboxOneAuthorizedFeedbackV1 `json:"feedback"`
	ImportDeviceID               uint64                      `json:"importDeviceId"`
	LocalTimeoutMilliseconds     uint32                      `json:"localTimeoutMilliseconds"`
}

// UnmarshalJSON implements custom unmarshaling to accept both uint16 and hex string formats
// for idVendor and idProduct (e.g., "0x12ac" or 4780).
func (d *DeviceCreateRequest) UnmarshalJSON(data []byte) error {
	// Parse into a temporary structure with flexible types
	var raw struct {
		Type           *string        `json:"type"`
		IDVendor       any            `json:"idVendor,omitempty"`
		IDProduct      any            `json:"idProduct,omitempty"`
		DeviceSpecific map[string]any `json:"deviceSpecific,omitempty"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	d.Type = raw.Type

	if raw.IDVendor != nil {
		val, err := parseNumberOrHex[uint16](raw.IDVendor)
		if err != nil {
			return fmt.Errorf("idVendor: %w", err)
		}
		d.IDVendor = &val
	}

	if raw.IDProduct != nil {
		val, err := parseNumberOrHex[uint16](raw.IDProduct)
		if err != nil {
			return fmt.Errorf("idProduct: %w", err)
		}
		d.IDProduct = &val
	}

	d.DeviceSpecific = raw.DeviceSpecific

	return nil
}

// parseUint16OrHex accepts either a JSON number or a hex string like "0x12ac"
func parseNumberOrHex[N constraints.Integer](v any) (N, error) {
	var zero N
	switch val := v.(type) {
	case float64:
		var minVal, maxVal float64
		switch any(zero).(type) {
		case int8:
			minVal, maxVal = math.MinInt8, math.MaxInt8
		case int16:
			minVal, maxVal = math.MinInt16, math.MaxInt16
		case int32:
			minVal, maxVal = math.MinInt32, math.MaxInt32
		case int64, int:
			minVal, maxVal = math.MinInt64, math.MaxInt64
		case uint8:
			minVal, maxVal = 0, math.MaxUint8
		case uint16:
			minVal, maxVal = 0, math.MaxUint16
		case uint32:
			minVal, maxVal = 0, math.MaxUint32
		case uint64, uint:
			minVal, maxVal = 0, math.MaxUint64
		default:
			return zero, fmt.Errorf("unsupported integer type %T", zero)
		}
		if val < minVal || val > maxVal {
			return zero, fmt.Errorf("value %v out of range for type %T", val, zero)
		}
		return N(val), nil
	case string:
		s := strings.TrimSpace(val)
		base := 10
		if strings.HasPrefix(strings.ToLower(s), "0x") {
			s = s[2:]
			base = 16
		} else if len(s) > 0 {
			if strings.ContainsAny(s, "abcdefABCDEF") {
				base = 16
			}
		}
		var bitSize int
		switch any(zero).(type) {
		case int8, uint8:
			bitSize = 8
		case int16, uint16:
			bitSize = 16
		case int32, uint32:
			bitSize = 32
		case int64, uint64, int, uint:
			bitSize = 64
		default:
			return zero, fmt.Errorf("unsupported integer type %T", zero)
		}
		switch any(zero).(type) {
		case int, int8, int16, int32, int64:
			parsed, err := strconv.ParseInt(s, base, bitSize)
			if err != nil {
				return zero, fmt.Errorf("invalid hex/numeric string %q: %w", val, err)
			}
			return N(parsed), nil
		default:
			parsed, err := strconv.ParseUint(s, base, bitSize)
			if err != nil {
				return zero, fmt.Errorf("invalid hex/numeric string %q: %w", val, err)
			}
			return N(parsed), nil
		}
	default:
		return zero, fmt.Errorf("expected number or hex string, got %T", v)
	}
}
