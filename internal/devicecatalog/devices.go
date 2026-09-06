// Package devicecatalog links VIIPER's generically creatable device types.
// It is deliberately separate from the typed retained-persona registry so an
// authenticated factory can depend on registry ownership without importing
// every concrete device package back through its API caller.
package devicecatalog

import (
	_ "github.com/Alia5/VIIPER/device/dualsense"
	_ "github.com/Alia5/VIIPER/device/dualshock4"
	_ "github.com/Alia5/VIIPER/device/keyboard"
	_ "github.com/Alia5/VIIPER/device/mouse"
	_ "github.com/Alia5/VIIPER/device/ns2pro"
	_ "github.com/Alia5/VIIPER/device/xbox360"
)
