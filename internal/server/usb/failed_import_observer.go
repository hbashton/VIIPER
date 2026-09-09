package usb

// SetFailedImportObserver installs a cold lifecycle notification, never a replacement for import
// authentication. Called outside all topology/retained locks, before replying
// with the ordinary no-device error (no fabricated protocol/version errors).
func (s *Server) SetFailedImportObserver(observer func(string)) {
	s.failedImportObserverMu.Lock()
	s.failedImportObserver = observer
	s.failedImportObserverMu.Unlock()
}

func (s *Server) observeFailedImport(alias string) {
	s.failedImportObserverMu.RLock()
	observer := s.failedImportObserver
	s.failedImportObserverMu.RUnlock()
	if observer != nil {
		observer(alias)
	}
}
