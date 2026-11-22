// Package mobile embeds the Plutonium engine in the Android app.
//
// The Go engine is exposed to Kotlin as two calls: Start launches the
// catalog/HLS HTTP handler on a loopback port and returns its base URL;
// Stop shuts it down. The Android UI talks to that URL over 127.0.0.1.
package mobile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nuvio-plutonium/internal/addon"
	"nuvio-plutonium/internal/hls"
	"nuvio-plutonium/internal/pluto"
)

// EngineVersion is the version reported by the embedded engine.
const engineVersion = "1.2.0"

type engine struct {
	base   string
	server *http.Server
	cancel context.CancelFunc
	done   chan struct{}
}

var (
	mu     sync.Mutex
	active *engine
)

// EngineVersion reports the embedded engine version.
func EngineVersion() string {
	return engineVersion
}

// Start launches the Plutonium engine on a loopback port and returns the base
// URL (for example http://127.0.0.1:41234). cacheDir must be writable.
//
// xff is the X-Forwarded-For sent to Pluto; an empty value uses the caller's
// real address (only useful inside the US). country is US, CA, or GB.
// tmdbKey is optional and only adds cast photos. subtitleOffset is the caption
// sync delay in seconds (Pluto's captions run ahead of the video; 2 matches
// the Kodi addon's default).
func Start(cacheDir, xff, country, tmdbKey string, subtitleOffset float64) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if active != nil {
		return active.base, nil
	}

	cacheDir = strings.TrimSpace(cacheDir)
	if cacheDir == "" {
		return "", errors.New("cache directory is required")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache directory: %w", err)
	}

	country = strings.ToUpper(strings.TrimSpace(country))
	if country != "US" && country != "CA" && country != "GB" {
		country = "US"
	}
	xff = strings.TrimSpace(xff)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	client := pluto.New(pluto.Options{
		XFF:     xff,
		Country: country,
		Logger:  logger,
	})
	subtitles := hls.NewSubtitleGenerator(
		client,
		filepath.Join(cacheDir, "subtitles"),
		7*24*time.Hour,
		32,
		logger,
	)
	manager := hls.NewManager(client, subtitles, 24*time.Hour, time.Duration(subtitleOffset*float64(time.Second)), logger)
	handler := addon.New(client, manager, addon.Config{
		Version:    engineVersion,
		XFF:        xff,
		TMDBAPIKey: strings.TrimSpace(tmdbKey),
	}, logger)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// subs.m3u8/subs.vtt wait for background caption generation. A cold
		// merge can take a few minutes on a slow connection, so the loopback
		// server must not cut the response off mid-generation.
		WriteTimeout:   10 * time.Minute,
		MaxHeaderBytes: 1 << 20,
	}
	done := make(chan struct{})
	go manager.Cleanup(ctx)
	go func() {
		defer close(done)
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("engine server stopped", "error", serveErr)
		}
	}()

	active = &engine{
		base:   "http://" + listener.Addr().String(),
		server: server,
		cancel: cancel,
		done:   done,
	}
	logger.Info("engine started", "base", active.base, "country", country)
	return active.base, nil
}

// Stop shuts down the engine and releases its port.
func Stop() error {
	mu.Lock()
	current := active
	active = nil
	mu.Unlock()
	if current == nil {
		return nil
	}
	current.cancel()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := current.server.Shutdown(shutdownCtx)
	select {
	case <-current.done:
	case <-time.After(5 * time.Second):
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
