package usb

import (
	"errors"
	"time"
)

var errRetainedInputServicePolicy = errors.New("experimental retained IN service interval must be 0 (descriptor), 1, 2, or 4 milliseconds")

// ServerConfig represents the server subcommand configuration.
type ServerConfig struct {
	Addr                    string        `help:"USB-IP server listen address; non-loopback exposure requires a trusted tunnel or firewall boundary" default:"127.0.0.1:3241" env:"VIIPER_USB_ADDR"`
	ConnectionTimeout       time.Duration `kong:"-"`
	BusCleanupTimeout       time.Duration `help:"-"`
	WriteBatchFlushInterval time.Duration `default:"0" help:"Interval to flush write batches to clients; default: disabled / immediate updates" env:"VIIPER_USB_WRITE_BATCH_FLUSH_INTERVAL"`
	EndpointDiagnostics     bool          `default:"false" help:"Emit aggregate USB endpoint scheduling diagnostics every five seconds" env:"VIIPER_USB_ENDPOINT_DIAGNOSTICS"`
	// RetainedImportAuthorityID is an explicit, nonzero opt-in for devices which
	// implement retainedusb.ImportDevice. Zero preserves ordinary devices' legacy
	// USB/IP import and URB paths and rejects retained-only devices.
	RetainedImportAuthorityID uint64 `default:"0" help:"Enable retained USB/IP imports under this explicit nonzero authority ID" env:"VIIPER_USB_RETAINED_IMPORT_AUTHORITY_ID"`
	// RetainedInputServiceMS is an experimental virtual-host scheduling policy,
	// not an advertised descriptor interval or a measured device input rate.
	// Zero preserves descriptor timing. Nonzero values affect retained IN only;
	// each imported scheduler snapshots the policy before activation.
	RetainedInputServiceMS int `default:"0" enum:"0,1,2,4" help:"EXPERIMENTAL retained IN virtual-host service interval in milliseconds: 0 preserves descriptor timing; 1, 2, or 4 override IN service only, without changing USB descriptors" env:"VIIPER_USB_RETAINED_INPUT_SERVICE_MS"`
	// RetainedImportDeviceAddressLimit is a non-CLI embedding/test ceiling for
	// retained registration allocation. Zero uses the USB/IP wire maximum.
	RetainedImportDeviceAddressLimit uint32 `kong:"-"`
}

func retainedInputServiceInterval(milliseconds int) (time.Duration, error) {
	switch milliseconds {
	case 0, 1, 2, 4:
		return time.Duration(milliseconds) * time.Millisecond, nil
	default:
		return 0, errRetainedInputServicePolicy
	}
}
