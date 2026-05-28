package main

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/yourorg/supertonic-docx/internal/api"
)

// runWatcher watches cfg.InboxDir for new .docx files and submits each one
// as a job using the configured default voices and languages.
//
// We use fsnotify for the event stream, but with a debounce: editors and
// sync tools (rsync, Dropbox, etc.) often touch a file several times before
// it's "done", so we wait for the file to be quiet for 2 seconds before
// processing.
func runWatcher(ctx context.Context, cfg config, jobs *api.JobStore) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("watcher: %v (disabled)", err)
		return
	}
	defer w.Close()

	if err := w.Add(cfg.InboxDir); err != nil {
		log.Printf("watcher add %s: %v (disabled)", cfg.InboxDir, err)
		return
	}
	log.Printf("watching %s for *.docx", cfg.InboxDir)

	// path -> last-seen time. We process once the path goes 2s without a write.
	pending := map[string]time.Time{}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if !isDocx(ev.Name) {
				continue
			}
			// Track create / write events; remove events drop the entry.
			if ev.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				pending[ev.Name] = time.Now()
			}
			if ev.Op&fsnotify.Remove != 0 {
				delete(pending, ev.Name)
			}

		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)

		case <-ticker.C:
			now := time.Now()
			for path, t := range pending {
				if now.Sub(t) < 2*time.Second {
					continue
				}
				delete(pending, path)
				log.Printf("watcher: submitting %s", path)
				_, err := jobs.SubmitFromPath(path, cfg.DefaultVoices, cfg.DefaultLangs, 0)
				if err != nil {
					log.Printf("watcher: submit %s: %v", path, err)
				}
			}
		}
	}
}

func isDocx(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".docx")
}
