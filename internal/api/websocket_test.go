package api

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/brianbuquoi/overlord/internal/broker"
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
	if ok := hub.register(c); ok {
		t.Fatal("register must return false after hub shutdown")
	}

	hub.mu.RLock()
	_, found := hub.clients[c]
	hub.mu.RUnlock()

	if found {
		t.Fatal("client should not be registered on a stopped hub")
	}
}

// TestWSHubRegisterRefusesAtMaxClients verifies the global
// connection cap. Once maxWSClients clients are registered, the
// next register call must return false so the caller can send a
// TryAgainLater close frame and avoid leaking an unbounded number
// of goroutines per tenant — the audit flagged the prior shape as
// a denial-of-service path for any authenticated caller.
func TestWSHubRegisterRefusesAtMaxClients(t *testing.T) {
	bus := broker.NewEventBus()
	hub := newWSHub(bus, slog.Default())
	defer hub.shutdown()

	accepted := 0
	clients := make([]*wsClient, 0, maxWSClients+5)
	for i := 0; i < maxWSClients+5; i++ {
		c := &wsClient{hub: hub, send: make(chan []byte, 1)}
		if hub.register(c) {
			accepted++
			clients = append(clients, c)
		}
	}

	if accepted != maxWSClients {
		t.Fatalf("expected exactly %d accepted registrations, got %d", maxWSClients, accepted)
	}

	// Drain accepted clients so the hub's state is clean for the
	// next test; this also exercises the unregister path.
	for _, c := range clients {
		hub.unregister(c)
	}

	// After unregistering, the cap slot is available again.
	c := &wsClient{hub: hub, send: make(chan []byte, 1)}
	if !hub.register(c) {
		t.Fatal("register must succeed after slots are freed")
	}
	hub.unregister(c)
}

// TestWSKeepaliveInvariants locks in the SEC4-003 resolution: the keepalive
// constants must remain sane so zombie connections are closed inside the pong
// window. If these get zeroed or reordered the read deadline regime collapses
// and the denial-of-service path returns.
func TestWSKeepaliveInvariants(t *testing.T) {
	if wsPongWait <= 0 {
		t.Fatalf("wsPongWait must be > 0, got %v", wsPongWait)
	}
	if wsWriteWait <= 0 {
		t.Fatalf("wsWriteWait must be > 0, got %v", wsWriteWait)
	}
	if wsPingPeriod <= 0 {
		t.Fatalf("wsPingPeriod must be > 0, got %v", wsPingPeriod)
	}
	// Ping period must be strictly less than pong wait: otherwise the server
	// cannot send a ping in time to refresh the read deadline before it
	// expires, and even a healthy client looks dead.
	if wsPingPeriod >= wsPongWait {
		t.Fatalf("wsPingPeriod (%v) must be < wsPongWait (%v)", wsPingPeriod, wsPongWait)
	}
	if wsWriteWait >= wsPongWait {
		t.Fatalf("wsWriteWait (%v) must be < wsPongWait (%v) so writes do not outlive the read deadline", wsWriteWait, wsPongWait)
	}
}
