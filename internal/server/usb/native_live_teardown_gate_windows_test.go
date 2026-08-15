//go:build windows

package usb_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/transport/udecx"
)

const nativeLiveTeardownGateTimeout = 15 * time.Second

func openNativeLiveTeardownClient(ctx context.Context) (*udecx.Client, error) {
	var lastTemporary error
	for {
		client, err := udecx.Open(ctx)
		if err == nil {
			return client, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("acquire clean controller after live teardown: %w (last transient error: %v)",
				ctxErr, lastTemporary)
		}
		var temporary interface{ Temporary() bool }
		if !errors.As(err, &temporary) || !temporary.Temporary() {
			return nil, err
		}
		lastTemporary = err
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("acquire clean controller after live teardown: %w (last transient error: %v)",
				ctx.Err(), lastTemporary)
		case <-timer.C:
		}
	}
}

func waitForNativeLiveTeardown(ctx context.Context, client *udecx.Client) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	lastDiagnostic := "no lifecycle snapshot queried"
	var lastStats udecx.Stats
	timedOut := func() error {
		return fmt.Errorf("teardown did not complete within %s: %s; stats=%+v",
			nativeLiveTeardownGateTimeout, lastDiagnostic, lastStats)
	}
	for {
		if ctx.Err() != nil {
			return timedOut()
		}
		trace, err := client.QueryLifecycleTrace(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return timedOut()
			}
			return fmt.Errorf("query lifecycle trace: %w", err)
		}
		stats, err := client.QueryStats(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return timedOut()
			}
			return fmt.Errorf("query teardown stats: %w", err)
		}
		lastStats = stats
		audit, err := auditNativeLiveTeardown(trace, stats)
		if err != nil {
			return err
		}
		if audit.complete {
			if audit.diagnostic != "" {
				fmt.Fprintf(os.Stderr, "native live teardown gate notice: %s\n", audit.diagnostic)
			}
			return nil
		}
		lastDiagnostic = audit.diagnostic

		select {
		case <-ctx.Done():
			return timedOut()
		case <-ticker.C:
		}
	}
}

func runNativeLiveTeardownGate(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := openNativeLiveTeardownClient(ctx)
	if err != nil {
		return err
	}
	auditErr := waitForNativeLiveTeardown(ctx, client)
	closeErr := client.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close teardown-audit controller: %w", closeErr)
	}
	return errors.Join(auditErr, closeErr)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if os.Getenv(liveNativeTestEnvironment) == "1" &&
		os.Getenv(liveNativeCrashChild) != "1" {
		if err := runNativeLiveTeardownGate(nativeLiveTeardownGateTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "native live teardown gate failed: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}
