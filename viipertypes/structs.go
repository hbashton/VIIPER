package viipertypes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	Server    string         `json:"server"`
	Version   string         `json:"version"`
	Transport string         `json:"transport,omitempty"`
	Ready     *bool          `json:"ready,omitempty"`
	NativeUDE *NativeUDEInfo `json:"nativeUde,omitempty"`
}

// NativeUDEInfo is the negotiated kernel contract for the active native
// transport. It is additive to the historical ping response so older clients
// continue to work while safety-conscious clients can fail closed unless the
// exact ABI and capabilities they require are live.
type NativeUDEInfo struct {
	ABIMajor                     uint16 `json:"abiMajor"`
	ABIMinor                     uint16 `json:"abiMinor"`
	Capabilities                 uint32 `json:"capabilities"`
	ExpectedDriverPackageVersion string `json:"expectedDriverPackageVersion"`
	// LoadedDriverBuildIdentity is the lowercase SHA-256 identity returned by
	// the currently loaded kernel image during ABI negotiation. It is not an
	// on-disk hash or a broker-computed status echo.
	LoadedDriverBuildIdentity string `json:"loadedDriverBuildIdentity"`
	ControllerSessionID       string `json:"controllerSessionId"`
	ControllerInstanceID      string `json:"controllerInstanceId"`
	MaxDevices                uint32 `json:"maxDevices"`
	MaxDescriptorBytes        uint32 `json:"maxDescriptorBytes"`
	MaxTransferBytes          uint32 `json:"maxTransferBytes"`
	MaxIsoPackets             uint32 `json:"maxIsoPackets"`
	MaxPendingOperations      uint32 `json:"maxPendingOperations"`
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
	BusID            uint32               `json:"busId"`
	DevID            string               `json:"devId"`
	Vid              string               `json:"vid"`
	Pid              string               `json:"pid"`
	Type             string               `json:"type"`
	DeviceSpecific   map[string]any       `json:"deviceSpecific"`
	Transport        string               `json:"transport"`
	NativeUDE        *NativeUDEDeviceInfo `json:"nativeUde,omitempty"`
	USBIPPort        int32                `json:"usbipPort,omitempty"`
	USBIPOwnerSerial string               `json:"usbipOwnerSerial,omitempty"`
}

// NativeUDEDeviceInfo is the exact kernel/controller receipt used to
// correlate one API device with its Windows HID and UAC descendants. DeviceID
// is decimal text so every JSON consumer preserves the full uint64 value.
type NativeUDEDeviceInfo struct {
	DeviceID             string `json:"deviceId"`
	DeviceGeneration     uint32 `json:"deviceGeneration"`
	ControllerSessionID  string `json:"controllerSessionId"`
	ControllerInstanceID string `json:"controllerInstanceId"`
	USB20PortNumber      uint32 `json:"usb20PortNumber"`
	USB30PortNumber      uint32 `json:"usb30PortNumber"`
}

type DevicesListResponse struct {
	Devices []Device `json:"devices"`
}

type DeviceRemoveResponse struct {
	BusID uint32 `json:"busId"`
	DevID string `json:"devId"`
}

// NativeUDEDeviceRemoveRequest is a compare-and-remove request. Native clients
// must echo the exact correlation receipt returned by add/list so a delayed
// cleanup cannot remove a successor that reused the same bus and device IDs.
type NativeUDEDeviceRemoveRequest struct {
	DevID     string               `json:"devId"`
	Transport string               `json:"transport"`
	NativeUDE *NativeUDEDeviceInfo `json:"nativeUde"`
}

// UnmarshalJSON rejects unknown, duplicate, and trailing fields. The echoed
// correlation receipt is mutation authority, so ambiguous JSON is not
// accepted even when encoding/json could otherwise choose a last value.
func (r *NativeUDEDeviceRemoveRequest) UnmarshalJSON(data []byte) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	if err := validateNativeRemoveJSONFieldNames(data); err != nil {
		return err
	}
	type requestAlias NativeUDEDeviceRemoveRequest
	var decoded requestAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("native remove request contains trailing JSON")
		}
		return fmt.Errorf("native remove request contains trailing JSON: %w", err)
	}
	*r = NativeUDEDeviceRemoveRequest(decoded)
	return nil
}

func validateNativeRemoveJSONFieldNames(data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return err
	}
	topFields := []string{"devId", "transport", "nativeUde"}
	if len(top) != len(topFields) {
		return fmt.Errorf("native remove request must contain exactly devId, transport, and nativeUde")
	}
	for _, field := range topFields {
		if _, ok := top[field]; !ok {
			return fmt.Errorf("native remove request is missing canonical JSON field %q", field)
		}
	}

	var native map[string]json.RawMessage
	if err := json.Unmarshal(top["nativeUde"], &native); err != nil {
		return fmt.Errorf("nativeUde must be an object: %w", err)
	}
	nativeFields := []string{
		"deviceId", "deviceGeneration", "controllerSessionId",
		"controllerInstanceId", "usb20PortNumber", "usb30PortNumber",
	}
	if len(native) != len(nativeFields) {
		return fmt.Errorf("nativeUde must contain the exact correlation receipt fields")
	}
	for _, field := range nativeFields {
		if _, ok := native[field]; !ok {
			return fmt.Errorf("nativeUde is missing canonical JSON field %q", field)
		}
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkUniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("native remove request contains trailing JSON")
		}
		return fmt.Errorf("native remove request contains trailing JSON: %w", err)
	}
	return nil
}

func walkUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("native remove request contains a non-string JSON object key")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("native remove request contains duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := walkUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("native remove request has malformed JSON object")
		}
	case '[':
		for decoder.More() {
			if err := walkUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("native remove request has malformed JSON array")
		}
	default:
		return fmt.Errorf("native remove request has unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

type DeviceCreateRequest struct {
	Type           *string        `json:"type"`
	IDVendor       *uint16        `json:"idVendor,omitempty"`
	IDProduct      *uint16        `json:"idProduct,omitempty"`
	DeviceSpecific map[string]any `json:"deviceSpecific,omitempty"`
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
