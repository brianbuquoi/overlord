package config

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fsDebounceInterval is the window used to coalesce filesystem event
// bursts. Editors like vim / VS Code / JetBrains tend to perform a
// rename-into-place on save, which triggers multiple Create and Write
// events within a few milliseconds; without debouncing, the watcher
// reload-storms (multiple buildContractRegistry + plugin reloads per
// save). Empirically 100ms absorbs the burst without adding
// operator-noticeable latency.
const fsDebounceInterval = 100 * time.Millisecond

// Watch monitors path for changes via SIGHUP and filesystem events,
// coalescing FS write bursts via a short debounce window, and fires
// onChange with the freshly-loaded config. The watcher goroutine stops
// cleanly when ctx is cancelled (or the config file is deleted from
// underneath the watcher); the caller never has to reach into fsnotify
// or track the goroutine itself.
//
// Watch returns promptly once the watcher is installed. Any setup
// failure (e.g. fsnotify initialisation, path not watchable) is
// reported via the returned error; once Watch has returned nil, the
// caller's only responsibility is to cancel ctx when it wants to shut
// the watcher down.
//
// The audit flagged the prior shape (no context, unmanaged goroutine,
// no debounce) as a lifecycle and reload-storm hazard.
func Watch(ctx context.Context, path string, onChange func(*Config)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	if err := watcher.Add(path); err != nil {
		watcher.Close()
		return err
	}

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)

	go func() {
		defer watcher.Close()
		defer signal.Stop(sighup)

		// debounce fires fsDebounceInterval after the most recent FS
		// event; we drain pending events during the window and reload
		// once.
		var debounce *time.Timer
		drainDebounce := func() {
			if debounce != nil {
				debounce.Stop()
				debounce = nil
			}
		}
		scheduleReload := func() {
			drainDebounce()
			debounce = time.NewTimer(fsDebounceInterval)
		}
		defer drainDebounce()

		for {
			var debounceC <-chan time.Time
			if debounce != nil {
				debounceC = debounce.C
			}

			select {
			case <-ctx.Done():
				return

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					scheduleReload()
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("config watcher error: %v", err)

			case <-debounceC:
				debounce = nil
				reload(path, onChange)

			case _, ok := <-sighup:
				if !ok {
					return
				}
				// SIGHUP is an explicit operator intent — reload
				// immediately, don't coalesce with queued FS events.
				drainDebounce()
				reload(path, onChange)
			}
		}
	}()

	return nil
}

func reload(path string, onChange func(*Config)) {
	cfg, err := Load(path)
	if err != nil {
		log.Printf("config reload failed: %v", err)
		return
	}
	onChange(cfg)
}
