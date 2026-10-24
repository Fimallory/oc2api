// Command oc2api exposes OpenCode Zen free models through an OpenAI
// Responses-compatible endpoint while reproducing the client shape the free
// tier requires.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"oc2api/internal/cache"
	"oc2api/internal/catalog"
	"oc2api/internal/config"
	"oc2api/internal/gateway"
	"oc2api/internal/server"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config.json (optional)")
	listen := flag.String("listen", "", "override the listen address")
	debug := flag.Bool("debug", false, "log every upstream attempt")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *debug {
		cfg.Debug = true
	}

	logger := log.New(os.Stderr, "oc2api ", log.LstdFlags|log.Lmsgprefix)
	logger.Printf("lane=%s upstream=%s exits=%d free_only=%v cache=%v",
		cfg.Lane, cfg.Upstream, len(cfg.Proxies)+boolInt(cfg.Direct), cfg.FreeOnly, cfg.CacheEnabled)

	cat := catalog.New("")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat.Start(ctx, time.Duration(cfg.CatalogRefreshMinutes)*time.Minute, logger.Printf)

	pool, err := gateway.NewPool(cfg)
	if err != nil {
		logger.Fatalf("exits: %v", err)
	}

	var store *cache.Cache
	if cfg.CacheEnabled {
		store = cache.New(cfg.CacheMaxEntries, time.Duration(cfg.CacheTTLSeconds)*time.Second)
	}

	handler := server.New(cfg, cat, pool, store, logger).Handler()
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Printf("listening on http://%s/v1/responses", cfg.Listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	logger.Printf("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
