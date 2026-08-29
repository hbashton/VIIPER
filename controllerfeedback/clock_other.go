//go:build !windows

package controllerfeedback

// HostMonotonicMicroseconds fails closed off Windows because CFBK v1 requires
// the Windows QPC host clock. Callers must negotiate a future contract version
// rather than substituting a process-relative monotonic origin.
func HostMonotonicMicroseconds() (uint64, bool) {
	return 0, false
}
