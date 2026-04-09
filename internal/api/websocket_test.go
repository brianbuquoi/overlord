package api

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/broker"
)

// TestWSHubShutdownBeforeEvents verifies that shutting down a wsHub
// immediately (before any events are published) and then calling
// unregister/stopped concurrently does not panic or deadlock.
func TestWSHubShutdownBeforeEvents(t *testing.T) {
	bus := broker.NewEventBus()
	logger := slog.Default()
	hub := newWSHub(bus, logger)

	// Shut down immediately — no events were ever published.
	hub.shutdown()

	if !hub.stopped() {
		t.Fatal("expected hub to be stopped after shutdown")
	}

	// Calling shutdown again must not panic (idempotent).
	hub.shutdown()
}

// TestWSHubConcurrentUnregisterDuringShutdown verifies that calling
// unregister from 10 goroutines concurrently with shutdown does not
// panic, deadlock, or trigger a data race.
func TestWSHubConcurrentUnregisterDuringShutdown(t *testing.T) {
	bus := broker.NewEventBus()
	logger := slog.Default()
	hub := newWSHub(bus, logger)

	// Shut down immediately before any events.
	hub.shutdown()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &wsClient{hub: hub, send: make(chan []byte, 1)}
			hub.unregister(c)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success — no panic, no deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: concurrent unregister did not complete within 5s")
	}
}

// TestWSHubConcurrentShutdown verifies that calling shutdown from
// multiple goroutines simultaneously does not panic or deadlock.
func TestWSHubConcurrentShutdown(t *testing.T) {
	bus := broker.NewEventBus()
	logger := slog.Default()
	hub := newWSHub(bus, logger)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hub.shutdown()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: concurrent shutdown did not complete within 5s")
	}

	if !hub.stopped() {
		t.Fatal("expected hub to be stopped after concurrent shutdowns")
	}
}

// TestWSHubRegisterAfterShutdown verifies that registering a client
// after shutdown is a safe no-op.
func TestWSHubRegisterAfterShutdown(t *testing.T) {
	bus := broker.NewEventBus()
	logger := slog.Default()
	hub := newWSHub(bus, logger)
	hub.shutdown()

	c := &wsClient{hub: hub, send: make(chan []byte, 1)}
	hub.register(c)

	hub.mu.RLock()
	_, found := hub.clients[c]
	hub.mu.RUnlock()

	if found {
		t.Fatal("client should not be registered on a stopped hub")
	}
}
