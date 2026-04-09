package config

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/fsnotify/fsnotify"
)

// Watch monitors the config file for changes via SIGHUP and filesystem events.
// When a change is detected, it reloads and validates the config, then calls onChange.
// This function blocks until the context signals done; run it in a goroutine.
func Watch(path string, onChange func(*Config)) error {
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

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					reload(path, onChange)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("config watcher error: %v", err)
			case _, ok := <-sighup:
				if !ok {
					return
				}
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
