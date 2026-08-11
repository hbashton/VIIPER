//go:build !cgo

package sdl

import "errors"

// This stub keeps the end-to-end benchmark type-checked in ordinary
// CGO-disabled builds. The real benchmark still requires the vendored SDL3
// development files and CGO; it fails explicitly instead of disappearing from
// compilation and allowing benchmark-only regressions to go unnoticed.

type InitFlags uint32

const (
	InitFlagGamepad InitFlags = 0x00002000
	InitFlagEvents  InitFlags = 0x00004000
)

type GamepadID int32

type GamepadType int32

const (
	GamepadTypeUnknown GamepadType = iota
	GamepadTypeStandard
	GamepadTypeXbox360
	GamepadTypeXboxOne
	GamepadTypePS3
	GamepadTypePS4
	GamepadTypePS5
)

type GamepadButton int32

const GamepadButtonSouth GamepadButton = 0

type Gamepad struct{}

type GUID [16]byte

func (GUID) String() string { return "" }

type GamepadButtonEvent struct {
	Down        bool
	TimestampNS uint64
}

func Init(InitFlags) error {
	return errors.New("SDL3 end-to-end benchmarks require CGO and the vendored SDL3 development files")
}

func Quit() {}

func UpdateGamepads() {}

func GetGamepads() ([]GamepadID, error) { return nil, nil }

func OpenGamepad(GamepadID) (*Gamepad, error) {
	return nil, errors.New("SDL3 end-to-end benchmarks require CGO")
}

func (*Gamepad) Close() {}

func (*Gamepad) ID() GamepadID { return 0 }

func (*Gamepad) Path() string { return "" }

func (*Gamepad) Name() string { return "" }

func (*Gamepad) Type() GamepadType { return GamepadTypeUnknown }

func (*Gamepad) RealType() GamepadType { return GamepadTypeUnknown }

func (*Gamepad) Vendor() uint16 { return 0 }

func (*Gamepad) Product() uint16 { return 0 }

func GetGamepadGUIDForID(GamepadID) GUID { return GUID{} }

func (*Gamepad) GetButton(GamepadButton) bool { return false }

func (*Gamepad) WaitButtonEvent(GamepadButton, bool, int32) bool { return false }

func (*Gamepad) WaitButtonTransition(GamepadButton, int32) (GamepadButtonEvent, bool, error) {
	return GamepadButtonEvent{}, false, errors.New("SDL3 end-to-end benchmarks require CGO")
}
