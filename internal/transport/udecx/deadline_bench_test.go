package udecx

import (
	"context"
	"testing"
	"time"
)

func BenchmarkLegacyInputDeadlineContext(b *testing.B) {
	parent := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		ctx, cancel := context.WithTimeout(parent, time.Hour)
		_ = ctx
		cancel()
	}
}

func BenchmarkReusableInputDeadlineTimer(b *testing.B) {
	timer := time.NewTimer(time.Hour)
	stopInputDeadlineTimer(timer)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		timer.Reset(time.Hour)
		stopInputDeadlineTimer(timer)
	}
}
