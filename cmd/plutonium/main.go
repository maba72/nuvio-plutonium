package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"nuvio-plutonium/internal/addon"
	"nuvio-plutonium/internal/hls"
	"nuvio-plutonium/internal/pluto"
)

var version = "1.0.0"

func main() {
	host := flag.String("host", "0.0.0.0", "listen address")
	port := flag.Int("port", 8789, "listen port")
	xff := flag.String("xff", os.Getenv("PLUTO_XFF"), "X-Forwarded-For sent to Pluto (US IP recommended)")
	country := flag.String("country", "US", "Pluto country code")
	publicURL := flag.String("public-url", "", "public base URL for HLS links (default: derive from request Host)")
	cacheDir := flag.String("cache-dir", "", "cache directory (default: user cache dir /nuvio-plutonium)")
	sessionTTL := flag.Duration("session-ttl", 24*time.Hour, "playback session lifetime")
	subtitleTTL := flag.Duration("subtitle-cache-ttl", 7*24*time.Hour, "retimed subtitle disk cache lifetime")
	_ = flag.Duration("subtitle-prewarm-timeout", 15*time.Second, "deprecated; subtitles are always generated in the background")
	subtitleOffset := flag.Float64("subtitle-offset", hls.DefaultSubtitleOffset.Seconds(), "subtitle sync delay in seconds; delay captions that run ahead of the picture")
	subtitleWorkers := flag.Int("subtitle-workers", 48, "parallel VTT segment downloads")
	logLevel := flag.String("log-level", "info", "debug, info, warn, or error")
	tmdbAPIKey := flag.String("tmdb-api-key", os.Getenv("TMDB_API_KEY"), "TMDb API key for cast photos (optional)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "invalid port")
		os.Exit(2)
	}
	if *subtitleWorkers < 1 {
		fmt.Fprintln(os.Stderr, "subtitle-workers must be at least 1")
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(*logLevel),
	}))
	slog.SetDefault(logger)

	resolvedCacheDir := strings.TrimSpace(*cacheDir)
	if resolvedCacheDir == "" {
		if base, err := os.UserCacheDir(); err == nil && base != "" {
			resolvedCacheDir = filepath.Join(base, "nuvio-plutonium")
		} else {
			resolvedCacheDir = filepath.Join(os.TempDir(), "nuvio-plutonium")
		}
	}
	if err := os.MkdirAll(resolvedCacheDir, 0o755); err != nil {
		logger.Error("could not create cache directory", "path", resolvedCacheDir, "error", err)
		os.Exit(1)
	}

	client := pluto.New(pluto.Options{
		XFF:     *xff,
		Country: *country,
		Logger:  logger,
	})
	subtitles := hls.NewSubtitleGenerator(client, resolvedCacheDir, *subtitleTTL, *subtitleWorkers, logger)
	manager := hls.NewManager(client, subtitles, *sessionTTL, time.Duration(*subtitleOffset*float64(time.Second)), logger)
	handler := addon.New(client, manager, addon.Config{
		Version:    version,
		PublicURL:  *publicURL,
		XFF:        *xff,
		TMDBAPIKey: *tmdbAPIKey,
	}, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go manager.Cleanup(ctx)

	address := net.JoinHostPort(*host, strconv.Itoa(*port))
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Info("plutonium listening",
		"version", version,
		"address", address,
		"manifest", "http://"+address+"/manifest.json",
		"cache", resolvedCacheDir,
		"xff", *xff,
		"country", *country,
	)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
	logger.Info("plutonium stopped")
}

func parseLogLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
