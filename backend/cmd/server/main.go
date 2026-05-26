// Package main is the single binary that runs both the web UI / HTTP API
// and (optionally) a folder watcher. Mode is driven by env vars, not flags,
// so the same image works under docker compose without arg gymnastics.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yourorg/supertonic-docx/internal/api"
	"github.com/yourorg/supertonic-docx/internal/tts"
)

func main() {
	cfg := loadConfig()

	log.Printf("supertonic-docx starting")
	log.Printf("  assets:  %s", cfg.AssetsDir)
	log.Printf("  outbox:  %s", cfg.OutboxDir)
	log.Printf("  inbox:   %s (watcher=%v)", cfg.InboxDir, cfg.InboxDir != "")
	log.Printf("  port:    %s", cfg.Port)

	if err := os.MkdirAll(cfg.OutboxDir, 0o755); err != nil {
		log.Fatalf("create outbox: %v", err)
	}

	// Engine is the shared Supertonic ONNX runtime. Loaded once, reused for
	// every synthesis call across HTTP requests and folder-watcher events.
	engine, err := tts.NewEngine(cfg.AssetsDir)
	if err != nil {
		log.Fatalf("load supertonic: %v", err)
	}
	defer engine.Close()

	log.Printf("  voices:  %v", engine.Voices())
	log.Printf("  langs:   %d available", len(engine.Languages()))

	jobs := api.NewJobStore(cfg.OutboxDir, engine, cfg.ParaGapMs)

	srv := api.NewServer(api.ServerOpts{
		Jobs:        jobs,
		OutboxDir:   cfg.OutboxDir,
		FrontendDir: cfg.FrontendDir,
	})

	httpServer := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: srv,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Watcher runs in its own goroutine if an inbox is configured.
	if cfg.InboxDir != "" {
		if err := os.MkdirAll(cfg.InboxDir, 0o755); err != nil {
			log.Fatalf("create inbox: %v", err)
		}
		go runWatcher(ctx, cfg, jobs)
	}

	go func() {
		log.Printf("listening on http://0.0.0.0:%s", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

type config struct {
	AssetsDir     string
	InboxDir      string
	OutboxDir     string
	FrontendDir   string
	Port          string
	ParaGapMs     int
	DefaultVoices []string
	DefaultLangs  []string
}

func loadConfig() config {
	c := config{
		AssetsDir:     getenv("SUPERTONIC_ASSETS", "/app/assets"),
		InboxDir:      getenv("SUPERTONIC_INBOX", "/app/inbox"),
		OutboxDir:     getenv("SUPERTONIC_OUTBOX", "/app/outbox"),
		FrontendDir:   getenv("SUPERTONIC_FRONTEND", "/app/frontend"),
		Port:          getenv("SUPERTONIC_PORT", "8080"),
		ParaGapMs:     getenvInt("SUPERTONIC_PARA_GAP_MS", 300),
		DefaultVoices: splitCSV(getenv("SUPERTONIC_DEFAULT_VOICES", "M1,F1")),
		DefaultLangs:  splitCSV(getenv("SUPERTONIC_DEFAULT_LANGS", "en")),
	}
	// Allow an explicit empty string to disable the watcher.
	if v, ok := os.LookupEnv("SUPERTONIC_INBOX"); ok && v == "" {
		c.InboxDir = ""
	}
	c.AssetsDir = filepath.Clean(c.AssetsDir)
	c.OutboxDir = filepath.Clean(c.OutboxDir)
	if c.InboxDir != "" {
		c.InboxDir = filepath.Clean(c.InboxDir)
	}
	return c
}

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
