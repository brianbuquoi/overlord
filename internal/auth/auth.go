// Package auth provides API key authentication for the Overlord HTTP API.
// Keys are loaded from environment variables, immediately hashed with bcrypt,
// and the plaintext is zeroed in memory.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"sync"
	"time"
	"unsafe"

	"github.com/brianbuquoi/overlord/internal/config"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is the bcrypt cost factor for hashing API keys.
// Tests can override this via SetCostForTesting to avoid slow CI runs.
var bcryptCost = 12

// SetCostForTesting overrides the bcrypt cost factor and returns a restore
// function. It is NOT safe for concurrent use — call it from TestMain or
// before any parallel subtests start.
func SetCostForTesting(cost int) func() {
	prev := bcryptCost
	bcryptCost = cost
	// Regenerate the dummy hash at the new cost so timing stays consistent.
	h, _ := bcrypt.GenerateFromPassword([]byte("overlord-dummy-key-for-timing"), cost)
	oldDummy := dummyHash
	dummyHash = h
	return func() {
		bcryptCost = prev
		dummyHash = oldDummy
	}
}

// Scope represents an API permission scope.
type Scope string

const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
	ScopeAdmin Scope = "admin"
)

// ErrUnauthorized is returned when authentication fails.
var ErrUnauthorized = errors.New("unauthorized")

// ScopeSet is a set of scopes. Write implies read. Admin implies all.
type ScopeSet map[Scope]bool

// HasScope reports whether the set includes the required scope.
// Admin implies all scopes; write implies read.
func (s ScopeSet) HasScope(required Scope) bool {
	if s[ScopeAdmin] {
		return true
	}
	if required == ScopeRead && s[ScopeWrite] {
		return true
	}
	return s[required]
}

// APIKey is a loaded and hashed API key.
type APIKey struct {
	Name      string
	Scopes    ScopeSet
	HashedKey []byte
}

// dummyHash is a pre-computed bcrypt hash used for timing-safe dummy comparisons
// when no keys match or when no keys are configured. This prevents timing oracles.
var dummyHash []byte

func init() {
	// Pre-compute a dummy hash at startup. Error is impossible for a fixed input.
	h, _ := bcrypt.GenerateFromPassword([]byte("overlord-dummy-key-for-timing"), bcryptCost)
	dummyHash = h
}

// LoadKeys reads API key plaintext from environment variables, hashes each
// with bcrypt, and returns APIKey structs. The plaintext is zeroed after
// hashing. Returns an error if any referenced env var is unset or empty.
func LoadKeys(entries []config.AuthKeyConfig) ([]APIKey, error) {
	keys := make([]APIKey, 0, len(entries))
	for _, e := range entries {
		plaintext := os.Getenv(e.KeyEnv)
		if plaintext == "" {
			return nil, fmt.Errorf("auth key %q: environment variable %s is not set or empty", e.Name, e.KeyEnv)
		}

		if len(plaintext) > 72 {
			return nil, fmt.Errorf("API key for %q exceeds bcrypt's 72-byte limit — use a key of 72 bytes or fewer", e.Name)
		}

		hashed, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcryptCost)
		if err != nil {
			return nil, fmt.Errorf("auth key %q: bcrypt hash failed: %w", e.Name, err)
		}

		// Zero the plaintext in memory. We can't guarantee the GC hasn't
		// copied the string, but we clear what we can.
		zeroString(&plaintext)

		scopes := make(ScopeSet, len(e.Scopes))
		for _, s := range e.Scopes {
			scopes[Scope(s)] = true
		}

		keys = append(keys, APIKey{
			Name:      e.Name,
			Scopes:    scopes,
			HashedKey: hashed,
		})
	}
	return keys, nil
}

// stringHeader mirrors the runtime representation of a Go string.
// Used by zeroString to access the original backing memory.
type stringHeader struct {
	Data unsafe.Pointer
	Len  int
}

// zeroString overwrites the original string's backing memory in place.
// This is best-effort: the Go runtime may have copied the string internally
// (e.g. during env variable reads), so this is defense-in-depth, not a
// guarantee. Unlike []byte(*s), this accesses the original backing array
// directly via unsafe.Pointer so the actual memory is zeroed.
func zeroString(s *string) {
	hdr := (*stringHeader)(unsafe.Pointer(s))
	if hdr.Len > 0 && hdr.Data != nil {
		b := unsafe.Slice((*byte)(hdr.Data), hdr.Len)
		for i := range b {
			b[i] = 0
		}
	}
	*s = ""
}

// Authenticate checks a bearer token against the loaded keys. Returns the
// matching APIKey on success or ErrUnauthorized on failure.
//
// To prevent timing oracles that reveal key count or match position, the
// function always iterates ALL keys regardless of whether a match is found.
// When no keys are configured, a dummy bcrypt comparison is performed so
// the response time is indistinguishable from a populated key list.
func Authenticate(keys []APIKey, token string) (*APIKey, error) {
	if token == "" {
		// Still do a dummy comparison to prevent timing oracle on empty token.
		bcrypt.CompareHashAndPassword(dummyHash, []byte(""))
		return nil, ErrUnauthorized
	}

	tokenBytes := []byte(token)
	var matched *APIKey

	// Always iterate all keys — no early return on match.
	for i := range keys {
		err := bcrypt.CompareHashAndPassword(keys[i].HashedKey, tokenBytes)
		if err == nil {
			matched = &keys[i]
			// Do NOT break or return here — constant-time iteration.
		}
	}

	if len(keys) == 0 {
		// Dummy comparison so empty-config timing matches populated-config.
		bcrypt.CompareHashAndPassword(dummyHash, tokenBytes)
	}

	if matched != nil {
		return matched, nil
	}
	return nil, ErrUnauthorized
}

// maxTrackedIPs is the maximum number of distinct IP-group entries the
// tracker will hold at once. IPv4 is tracked per /32 and IPv6 per /64 so
// an attacker with a single routed prefix cannot spray 2^64 distinct
// entries into the map.
const maxTrackedIPs = 100_000


// BruteForceTracker tracks authentication failures per IP for brute force
// protection. After maxFailures within windowDuration, subsequent requests
// are rejected with 429 regardless of key validity.
//
// A background cleanup goroutine sweeps expired entries every 5 minutes.
// The tracker caps itself at maxIPCap (default 100,000). Once the map
// crosses 90% of that cap, RecordFailure bulk-evicts the least-recently-seen
// live entries down to 80%, amortising the eviction cost so the hot path
// is not penalised on every insert at saturation.
type BruteForceTracker struct {
	mu                sync.Mutex
	failures          map[string]*ipFailures
	maxFailures       int
	windowDuration    time.Duration
	maxIPCap          int
	evictionThreshold int // 90% of maxIPCap; once reached, bulk-evict.
	evictionTarget    int // 80% of maxIPCap; bulk eviction drops to this.
	logger            *slog.Logger
}

type ipFailures struct {
	count     int
	windowEnd time.Time
	lastSeen  time.Time
}

// NewBruteForceTracker creates a new tracker with the given limits.
// Pass a context that is cancelled on server shutdown so the cleanup
// goroutine stops cleanly. Pass nil for logger to use the default logger.
func NewBruteForceTracker(maxFailures int, window time.Duration, opts ...BruteForceOption) *BruteForceTracker {
	t := &BruteForceTracker{
		failures:       make(map[string]*ipFailures),
		maxFailures:    maxFailures,
		windowDuration: window,
		maxIPCap:       maxTrackedIPs,
	}
	for _, o := range opts {
		o(t)
	}
	t.evictionThreshold = (t.maxIPCap * 9) / 10
	t.evictionTarget = (t.maxIPCap * 8) / 10
	return t
}

// BruteForceOption configures optional BruteForceTracker behavior.
type BruteForceOption func(*BruteForceTracker)

// WithCleanup starts a background goroutine that sweeps expired entries
// every 5 minutes. The goroutine stops when ctx is cancelled.
func WithCleanup(ctx context.Context) BruteForceOption {
	return func(t *BruteForceTracker) {
		go t.cleanupLoop(ctx)
	}
}

// WithLogger sets the logger for warning messages (e.g. IP cap reached).
func WithLogger(l *slog.Logger) BruteForceOption {
	return func(t *BruteForceTracker) {
		t.logger = l
	}
}

// WithMaxIPs overrides the default IP cap (maxTrackedIPs). Intended for testing.
func WithMaxIPs(cap int) BruteForceOption {
	return func(t *BruteForceTracker) {
		t.maxIPCap = cap
	}
}

// cleanupLoop periodically removes expired entries from the failures map.
func (t *BruteForceTracker) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.mu.Lock()
			now := time.Now()
			for ip, entry := range t.failures {
				if now.After(entry.windowEnd) {
					delete(t.failures, ip)
				}
			}
			t.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// normalizeIP reduces a client IP to the tracking key. IPv4 addresses are
// preserved as /32; IPv6 addresses are masked to /64 so that an attacker
// holding an IPv6 prefix cannot get 2^64 free authentication attempts by
// rotating through addresses within their subnet. If the string does not
// parse as an IP it is returned unchanged — this covers test fixtures like
// "testclient" and non-standard RemoteAddr values without breaking them.
func normalizeIP(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	// IPv6: mask to /64.
	return parsed.Mask(net.CIDRMask(64, 128)).String()
}

// IsBlocked reports whether the given IP has exceeded the failure threshold.
func (t *BruteForceTracker) IsBlocked(ip string) bool {
	ip = normalizeIP(ip)
	t.mu.Lock()
	defer t.mu.Unlock()

	f, ok := t.failures[ip]
	if !ok {
		return false
	}

	if time.Now().After(f.windowEnd) {
		delete(t.failures, ip)
		return false
	}

	return f.count >= t.maxFailures
}

// RecordFailure records an authentication failure for the given IP. IPv6
// addresses are coalesced to /64 to prevent subnet-spray bypass. When the
// tracker crosses evictionThreshold (90% of maxIPCap), a bulk sweep prunes
// expired entries and, if still above evictionTarget (80% of maxIPCap),
// evicts the oldest live entries. Evicting in bulk amortises the cost so
// one eviction buys another ~10% of capacity before the next sweep.
func (t *BruteForceTracker) RecordFailure(ip string) {
	ip = normalizeIP(ip)
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	f, ok := t.failures[ip]
	if !ok || now.After(f.windowEnd) {
		// New insert path. Check eviction at 90% — this is the near-capacity
		// threshold, not the hard cap. Evicting here prevents the map from
		// saturating and forcing an eviction on every subsequent insert.
		if !ok && len(t.failures) >= t.evictionThreshold {
			t.bulkEvictLocked(now)
		}
		t.failures[ip] = &ipFailures{
			count:     1,
			windowEnd: now.Add(t.windowDuration),
			lastSeen:  now,
		}
		return
	}

	f.count++
	f.lastSeen = now
}

// bulkEvictLocked drops expired entries first, then — if the map is still
// above evictionTarget — evicts the least-recently-seen live entries until
// it falls back to evictionTarget. Callers must hold t.mu.
func (t *BruteForceTracker) bulkEvictLocked(now time.Time) {
	// Phase 1: expired entries.
	pruned := 0
	for ip, entry := range t.failures {
		if now.After(entry.windowEnd) {
			delete(t.failures, ip)
			pruned++
		}
	}
	if len(t.failures) <= t.evictionTarget {
		if t.logger != nil && pruned > 0 {
			t.logger.Info("brute force tracker pruned expired entries",
				"pruned", pruned,
				"tracked_ips", len(t.failures),
			)
		}
		return
	}

	// Phase 2: sort remaining live entries by lastSeen and drop the oldest
	// until we are back at evictionTarget.
	type ipEntry struct {
		ip       string
		lastSeen time.Time
	}
	remaining := make([]ipEntry, 0, len(t.failures))
	for ip, entry := range t.failures {
		remaining = append(remaining, ipEntry{ip: ip, lastSeen: entry.lastSeen})
	}
	sort.Slice(remaining, func(i, j int) bool { return remaining[i].lastSeen.Before(remaining[j].lastSeen) })
	toEvict := len(t.failures) - t.evictionTarget
	evicted := 0
	for i := 0; i < toEvict && i < len(remaining); i++ {
		delete(t.failures, remaining[i].ip)
		evicted++
	}
	if t.logger != nil {
		t.logger.Warn("brute force tracker evicting entries",
			"evicted", evicted,
			"pruned_expired", pruned,
			"remaining", len(t.failures),
			"capacity", t.maxIPCap,
		)
	}
}

// TrackedIPs returns the number of IPs currently in the failures map.
// Intended for testing and monitoring.
func (t *BruteForceTracker) TrackedIPs() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.failures)
}

// Cleanup runs a single cleanup sweep, removing expired entries.
// Exposed for testing; production code uses the background cleanupLoop.
func (t *BruteForceTracker) Cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for ip, entry := range t.failures {
		if now.After(entry.windowEnd) {
			delete(t.failures, ip)
		}
	}
}

// RecordSuccess is a no-op. It intentionally does NOT reset the failure
// counter for the given IP. See SEC3-001: an attacker with one valid
// credential could previously reset their brute force window indefinitely
// by interleaving valid and invalid requests. Failures now expire naturally
// via the sliding window.
func (t *BruteForceTracker) RecordSuccess(_ string) {
	// No-op: failures expire naturally via windowDuration.
}

// WindowEnd returns the time when the current failure window expires for an
// IP, or the zero time if the IP has no active failures. Used to calculate
// the Retry-After header on 429 responses.
func (t *BruteForceTracker) WindowEnd(ip string) time.Time {
	ip = normalizeIP(ip)
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.failures[ip]
	if !ok {
		return time.Time{}
	}
	return f.windowEnd
}
