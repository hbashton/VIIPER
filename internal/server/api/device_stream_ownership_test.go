package api

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDeviceStreamReplacementWaitsForDisplacedHandlerCleanup(t *testing.T) {
	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 17, devID: "4"}
	firstServer, firstClient := net.Pipe()
	defer firstClient.Close() //nolint:errcheck
	first := coordinator.claim(key, firstServer)
	require.True(t, first.waitForTurn(context.Background()))

	secondServer, secondClient := net.Pipe()
	defer secondClient.Close() //nolint:errcheck
	second := coordinator.claim(key, secondServer)

	// Claiming the replacement closes the displaced transport immediately.
	readDone := make(chan error, 1)
	go func() {
		var one [1]byte
		_, err := firstClient.Read(one[:])
		readDone <- err
	}()
	select {
	case err := <-readDone:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("displaced stream transport was not closed")
	}

	secondTurn := make(chan bool, 1)
	go func() {
		secondTurn <- second.waitForTurn(context.Background())
	}()
	select {
	case <-secondTurn:
		t.Fatal("replacement entered before displaced handler cleanup completed")
	case <-time.After(25 * time.Millisecond):
	}

	var staleFinalize atomic.Int32
	var staleCleanup atomic.Int32
	first.finish(time.Millisecond, time.Hour, context.Background(), func() {
		staleFinalize.Add(1)
	}, func() { staleCleanup.Add(1) })
	require.True(t, <-secondTurn)

	// The superseded generation must neither finalize shared stream state nor
	// arm its cleanup callback.
	time.Sleep(20 * time.Millisecond)
	require.Zero(t, staleFinalize.Load())
	require.Zero(t, staleCleanup.Load())
	second.abandon()
}

func TestDeviceStreamCurrentGenerationFinalizesBeforeLaterClaim(t *testing.T) {
	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 21, devID: "3"}
	firstServer, firstClient := net.Pipe()
	defer firstServer.Close() //nolint:errcheck
	defer firstClient.Close() //nolint:errcheck
	first := coordinator.claim(key, firstServer)
	require.True(t, first.waitForTurn(context.Background()))

	finalizeStarted := make(chan struct{})
	allowFinalize := make(chan struct{})
	var finalizeCalls atomic.Int32
	first.finish(0, time.Hour, context.Background(), func() {
		finalizeCalls.Add(1)
		close(finalizeStarted)
		<-allowFinalize
	}, nil)
	<-finalizeStarted

	secondServer, secondClient := net.Pipe()
	defer secondServer.Close() //nolint:errcheck
	defer secondClient.Close() //nolint:errcheck
	claimed := make(chan *deviceStreamLease, 1)
	go func() { claimed <- coordinator.claim(key, secondServer) }()

	select {
	case <-claimed:
		t.Fatal("later generation claimed device before current finalization completed")
	case <-time.After(25 * time.Millisecond):
	}

	close(allowFinalize)
	second := <-claimed
	require.Equal(t, int32(1), finalizeCalls.Load())
	require.True(t, second.waitForTurn(context.Background()))

	// finish is idempotent even if multiple teardown paths converge on it.
	first.finish(0, time.Hour, context.Background(), func() {
		finalizeCalls.Add(1)
	}, nil)
	require.Equal(t, int32(1), finalizeCalls.Load())
	second.abandon()
}

func TestDeviceStreamRapidReplacementOnlyFinalizesNewestGeneration(t *testing.T) {
	const generationCount = 64

	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 22, devID: "9"}
	leases := make([]*deviceStreamLease, 0, generationCount)
	clients := make([]net.Conn, 0, generationCount)

	for generation := 0; generation < generationCount; generation++ {
		server, client := net.Pipe()
		clients = append(clients, client)
		leases = append(leases, coordinator.claim(key, server))
	}
	defer func() {
		for _, client := range clients {
			_ = client.Close()
		}
	}()

	var finalizeCalls atomic.Int32
	for generation := 0; generation < generationCount-1; generation++ {
		leases[generation].finish(time.Millisecond, time.Hour, context.Background(), func() {
			finalizeCalls.Add(1)
		}, nil)
	}
	require.Zero(t, finalizeCalls.Load(),
		"a displaced generation finalized shared stream state")

	latest := leases[generationCount-1]
	require.True(t, latest.waitForTurn(context.Background()))
	latest.finish(0, time.Hour, context.Background(), func() {
		finalizeCalls.Add(1)
	}, nil)
	require.Eventually(t, func() bool {
		return finalizeCalls.Load() == 1
	}, time.Second, time.Millisecond)
}

func TestDeviceStreamNewestWaitingGenerationWins(t *testing.T) {
	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 18, devID: "1"}
	firstServer, firstClient := net.Pipe()
	defer firstClient.Close() //nolint:errcheck
	first := coordinator.claim(key, firstServer)
	require.True(t, first.waitForTurn(context.Background()))

	secondServer, secondClient := net.Pipe()
	defer secondClient.Close() //nolint:errcheck
	second := coordinator.claim(key, secondServer)
	thirdServer, thirdClient := net.Pipe()
	defer thirdClient.Close() //nolint:errcheck
	third := coordinator.claim(key, thirdServer)

	secondTurn := make(chan bool, 1)
	go func() { secondTurn <- second.waitForTurn(context.Background()) }()
	thirdTurn := make(chan bool, 1)
	go func() { thirdTurn <- third.waitForTurn(context.Background()) }()

	first.abandon()
	require.False(t, <-secondTurn)
	second.abandon()
	require.True(t, <-thirdTurn)
	third.abandon()
}

func TestDeviceStreamReconnectCancelsPendingCleanup(t *testing.T) {
	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 19, devID: "2"}
	firstServer, firstClient := net.Pipe()
	defer firstClient.Close() //nolint:errcheck
	first := coordinator.claim(key, firstServer)
	require.True(t, first.waitForTurn(context.Background()))

	var cleanupCalls atomic.Int32
	first.finish(time.Hour, 40*time.Millisecond, context.Background(), nil, func() {
		cleanupCalls.Add(1)
	})

	secondServer, secondClient := net.Pipe()
	defer secondClient.Close() //nolint:errcheck
	second := coordinator.claim(key, secondServer)
	require.True(t, second.waitForTurn(context.Background()))
	time.Sleep(75 * time.Millisecond)
	require.Zero(t, cleanupCalls.Load())
	second.abandon()
}

func TestInitialCleanupCannotRemoveActivelyClaimedDevice(t *testing.T) {
	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 20, devID: "7"}
	var cleanupCalls atomic.Int32
	coordinator.scheduleCleanup(key, 40*time.Millisecond,
		context.Background(), func() { cleanupCalls.Add(1) })

	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck
	lease := coordinator.claim(key, server)
	require.True(t, lease.waitForTurn(context.Background()))
	time.Sleep(75 * time.Millisecond)
	require.Zero(t, cleanupCalls.Load())
	lease.abandon()
}

func TestDeviceStreamCloseFirstReconnectCancelsPendingFinalization(t *testing.T) {
	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 23, devID: "5"}
	firstServer, firstClient := net.Pipe()
	defer firstServer.Close() //nolint:errcheck
	defer firstClient.Close() //nolint:errcheck
	first := coordinator.claim(key, firstServer)
	require.True(t, first.waitForTurn(context.Background()))

	var finalizeCalls atomic.Int32
	first.finish(75*time.Millisecond, time.Hour, context.Background(), func() {
		finalizeCalls.Add(1)
	}, nil)

	// This is the natural reconnect order: the old stream has completely
	// finished before the replacement claims the same virtual device.
	time.Sleep(10 * time.Millisecond)
	secondServer, secondClient := net.Pipe()
	defer secondServer.Close() //nolint:errcheck
	defer secondClient.Close() //nolint:errcheck
	second := coordinator.claim(key, secondServer)
	require.True(t, second.waitForTurn(context.Background()))

	time.Sleep(100 * time.Millisecond)
	require.Zero(t, finalizeCalls.Load(),
		"old close-first timer finalized replacement-owned state")
	second.abandon()
}

func TestDeviceStreamNoReplacementFinalizesAfterReconnectGrace(t *testing.T) {
	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 24, devID: "6"}
	server, client := net.Pipe()
	defer server.Close() //nolint:errcheck
	defer client.Close() //nolint:errcheck
	lease := coordinator.claim(key, server)
	require.True(t, lease.waitForTurn(context.Background()))

	var finalizeCalls atomic.Int32
	lease.finish(30*time.Millisecond, time.Hour, context.Background(), func() {
		finalizeCalls.Add(1)
	}, nil)
	require.Zero(t, finalizeCalls.Load(), "finalized before reconnect grace")
	require.Eventually(t, func() bool {
		return finalizeCalls.Load() == 1
	}, time.Second, time.Millisecond)
}

func TestDeviceStreamCleanupForcesPendingFinalizationExactlyOnce(t *testing.T) {
	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 25, devID: "8"}
	server, client := net.Pipe()
	defer server.Close() //nolint:errcheck
	defer client.Close() //nolint:errcheck
	lease := coordinator.claim(key, server)
	require.True(t, lease.waitForTurn(context.Background()))

	var finalizeCalls atomic.Int32
	var cleanupCalls atomic.Int32
	var cleanupBeforeFinalize atomic.Bool
	lease.finish(time.Hour, 25*time.Millisecond, context.Background(), func() {
		finalizeCalls.Add(1)
	}, func() {
		if finalizeCalls.Load() != 1 {
			cleanupBeforeFinalize.Store(true)
		}
		cleanupCalls.Add(1)
	})
	require.Eventually(t, func() bool {
		return cleanupCalls.Load() == 1
	}, time.Second, time.Millisecond)
	require.False(t, cleanupBeforeFinalize.Load(),
		"cleanup ran before pending stream finalization")
	require.Equal(t, int32(1), finalizeCalls.Load())
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), finalizeCalls.Load())
}

func TestDeviceStreamCloseFirstReconnectStressRejectsStaleTimers(t *testing.T) {
	const generations = 64

	var coordinator deviceStreamCoordinator
	key := deviceStreamKey{busID: 26, devID: "10"}
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck
	lease := coordinator.claim(key, server)
	require.True(t, lease.waitForTurn(context.Background()))

	var staleFinalizations atomic.Int32
	for generation := 0; generation < generations; generation++ {
		lease.finish(75*time.Millisecond, time.Hour, context.Background(), func() {
			staleFinalizations.Add(1)
		}, nil)
		require.NoError(t, server.Close())

		server, client = net.Pipe()
		defer client.Close() //nolint:errcheck
		lease = coordinator.claim(key, server)
		require.True(t, lease.waitForTurn(context.Background()))
	}

	time.Sleep(100 * time.Millisecond)
	require.Zero(t, staleFinalizations.Load())
	lease.abandon()
	require.NoError(t, server.Close())
}
