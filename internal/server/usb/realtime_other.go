//go:build !windows

package usb

func enterRealtimeMediaThread() (func(), error) {
	return func() {}, nil
}
