//go:build !cgo

package sdl

import "errors"

// This stub keeps the end-to-end benchmark type-checked in ordinary
// CGO-disabled builds. The real benchmark still requires the vendored SDL3
// development files and CGO; it fails explicitly instead of disappearing from
// compilation and allowing benchmark-only regressions to go unnoticed.

type InitFlags uint32

const InitFlagGamepad InitFlags = 0x00002000

type GamepadID uint32

type GamepadButton int32

const GamepadButtonSouth GamepadButton = 0

type Gamepad struct{}

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

func (*Gamepad) GetButton(GamepadButton) bool { return false }
