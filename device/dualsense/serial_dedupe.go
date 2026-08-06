package dualsense

import "sync"

var (
	identityMu sync.Mutex
	serials    = map[string]struct{}{}
	macs       = map[string]struct{}{}
)
