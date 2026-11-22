package addon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"nuvio-plutonium/internal/hls"
	"nuvio-plutonium/internal/pluto"
	"nuvio-plutonium/internal/tmdb"
)

// Config controls public URLs and upstream headers.
type Config struct {
	Version    string
	PublicURL  string
	XFF        string
	TMDBAPIKey string
}

// Server implements the Nuvio addon and HLS proxy HTTP handler.
type Server struct {
	pluto   *pluto.Client
	tmdb    *tmdb.Client
	manager *hls.Manager
	logger  *slog.Logger
	config  Config
	started time.Time
}

func New(client *pluto.Client, manager *hls.Manager, config Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if config.Version == "" {
		config.Version = "dev"
	}
	var tmdbClient *tmdb.Client
	if config.TMDBAPIKey != "" {
		tmdbClient = tmdb.New(config.TMDBAPIKey, logger)
	}
	return &Server{
		pluto:   client,
		tmdb:    tmdbClient,
		manager: manager,
		logger:  logger,
		config:  config,
		started: time.Now(),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	recorder := &statusRecorder{ResponseWriter: w}
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	cleanPath := strings.TrimSuffix(path, "/")
	if cleanPath == "" {
		cleanPath = "/"
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("request panic", "path", path, "panic", recovered)
			writeError(recorder, r, http.StatusInternalServerError, "internal error")
		}
		level := slog.LevelInfo
		if strings.HasPrefix(cleanPath, "/hls/") || cleanPath == "/health" {
			level = slog.LevelDebug
		}
		s.logger.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", path,
			"status", recorder.statusCode(),
			"duration", time.Since(started).Round(time.Millisecond),
		)
	}()

	recorder.Header().Set("Access-Control-Allow-Origin", "*")
	recorder.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	recorder.Header().Set("Access-Control-Allow-Headers", "*")
	recorder.Header().Set("Vary", "Origin")

	if r.Method == http.MethodOptions {
		recorder.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		recorder.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeError(recorder, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	switch {
	case cleanPath == "/health":
		s.handleHealth(recorder, r)
	case cleanPath == "/search":
		s.handleSearch(recorder, r)
	case cleanPath == "/manifest.json" || cleanPath == "/":
		writeJSON(recorder, r, http.StatusOK, Manifest(s.config.Version))
	case strings.HasPrefix(cleanPath, "/catalog/"):
		resource, mediaType, id, ok := splitResource(cleanPath)
		if !ok || resource != "catalog" {
			writeError(recorder, r, http.StatusNotFound, "not found")
			return
		}
		s.handleCatalog(recorder, r, mediaType, id)
	case strings.HasPrefix(cleanPath, "/meta/"):
		resource, mediaType, id, ok := splitResource(cleanPath)
		if !ok || resource != "meta" {
			writeError(recorder, r, http.StatusNotFound, "not found")
			return
		}
		s.handleMeta(recorder, r, mediaType, id)
	case strings.HasPrefix(cleanPath, "/stream/"):
		resource, mediaType, id, ok := splitResource(cleanPath)
		if !ok || resource != "stream" {
			writeError(recorder, r, http.StatusNotFound, "not found")
			return
		}
		s.handleStream(recorder, r, mediaType, id)
	case strings.HasPrefix(cleanPath, "/hls/"):
		s.handleHLS(recorder, r, cleanPath)
	default:
		writeError(recorder, r, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]any{
		"ok":            true,
		"version":       s.config.Version,
		"uptimeSeconds": int(time.Since(s.started).Seconds()),
		"sessions":      s.manager.Count(),
		"subtitleJobs":  s.manager.SubtitleJobs(),
		"upstreamCache": s.pluto.CacheStats(),
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, r, http.StatusOK, map[string]any{"metas": []any{}})
		return
	}
	data, err := s.pluto.Categories(r.Context())
	if err != nil {
		s.logger.Warn("search catalog fetch failed", "error", err)
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	metas := pluto.SearchItems(data, query, 80)
	writeJSON(w, r, http.StatusOK, map[string]any{"metas": metas})
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request, mediaType, rowID string) {
	row, ok := rowByID[rowID]
	if !ok || row.Type != mediaType {
		writeJSON(w, r, http.StatusOK, map[string]any{"metas": []any{}})
		return
	}
	data, err := s.pluto.Categories(r.Context())
	if err != nil {
		s.logger.Warn("catalog fetch failed", "row", rowID, "error", err)
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	items := pluto.FindCategory(data, row.Category)
	metas := make([]map[string]any, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		if itemType != mediaType {
			continue
		}
		meta := pluto.CatalogItem(item, mediaType)
		if meta == nil {
			continue
		}
		id, _ := meta["id"].(string)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		metas = append(metas, meta)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"metas": metas})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request, mediaType, rawID string) {
	if mediaType != "movie" && mediaType != "series" {
		writeError(w, r, http.StatusNotFound, "unsupported type")
		return
	}
	prefix := "pluto:movie:"
	if mediaType == "series" {
		prefix = "pluto:series:"
	}
	if !strings.HasPrefix(rawID, prefix) {
		writeError(w, r, http.StatusNotFound, "unsupported id")
		return
	}
	itemID := strings.TrimPrefix(rawID, prefix)
	data, err := s.pluto.Categories(r.Context())
	if err != nil {
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	item := pluto.FindItem(data, itemID)
	if item == nil {
		writeError(w, r, http.StatusNotFound, "item not found")
		return
	}
	var meta map[string]any
	if mediaType == "series" {
		meta, err = pluto.SeriesMeta(r.Context(), s.pluto, item)
		if err != nil {
			writeError(w, r, http.StatusBadGateway, err.Error())
			return
		}
	} else {
		meta = pluto.MetaForItem(item, mediaType)
	}

	// Enrich cast with TMDb profile photos when a TMDb API key is configured.
	if s.tmdb != nil {
		if enriched := enrichCastWithTMDb(r.Context(), s.tmdb, s.logger, meta); enriched {
			meta["cast"] = enrichedCast(meta)
		}
	}

	writeJSON(w, r, http.StatusOK, map[string]any{"meta": meta})
}

// castEntry mirrors the Stremio/Nuvio cast object shape: name plus optional poster.
type castEntry struct {
	Name   string `json:"name"`
	Poster string `json:"poster,omitempty"`
}

// enrichCastWithTMDb looks up the title on TMDb, fetches credits, and replaces
// the meta's string-only cast with objects that include profile photos. Returns
// true if enrichment was applied.
func enrichCastWithTMDb(ctx context.Context, client *tmdb.Client, logger *slog.Logger, meta map[string]any) bool {
	name, _ := meta["name"].(string)
	if name == "" {
		return false
	}
	year, _ := meta["releaseInfo"].(string)
	if year == "" {
		if ri, ok := meta["releaseInfo"]; ok {
			year = fmt.Sprint(ri)
		}
	}

	tmdbID, mediaType, err := client.FindID(ctx, name, year)
	if err != nil {
		logger.Debug("tmdb find id failed", "title", name, "error", err)
		return false
	}

	cast, err := client.Cast(ctx, tmdbID, mediaType)
	if err != nil {
		logger.Debug("tmdb cast failed", "title", name, "id", tmdbID, "error", err)
		return false
	}
	if len(cast) == 0 {
		return false
	}

	// Store the enriched cast under a private key so handleMeta can swap it in.
	meta["_tmdbCast"] = cast
	meta["_tmdbMediaType"] = mediaType
	return true
}

// enrichedCast returns the TMDb-enriched cast if present, otherwise the original.
func enrichedCast(meta map[string]any) []any {
	if raw, ok := meta["_tmdbCast"].([]tmdb.CastMember); ok {
		entries := make([]any, 0, len(raw))
		for _, m := range raw {
			entries = append(entries, castEntry{Name: m.Name, Poster: m.Poster})
		}
		delete(meta, "_tmdbCast")
		delete(meta, "_tmdbMediaType")
		return entries
	}
	return nil
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, mediaType, rawID string) {
	if mediaType != "movie" && mediaType != "series" {
		writeError(w, r, http.StatusNotFound, "unsupported type")
		return
	}
	mediaID, ok := streamMediaID(rawID)
	if !ok {
		writeJSON(w, r, http.StatusOK, map[string]any{"streams": []any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	session, err := s.manager.NewSession(ctx, mediaID)
	if err != nil {
		s.logger.Warn("stream resolve failed", "media", mediaID, "error", err)
		writeError(w, r, http.StatusBadGateway, "could not resolve Pluto stream: "+err.Error())
		return
	}
	base := s.publicBaseURL(r)
	stream := map[string]any{
		"name":  "Pluto TV",
		"title": "Pluto TV • Ad-free",
		"url":   base + "/hls/" + session.ID + "/master.m3u8",
		"type":  "hls",
	}
	headers := map[string]string{"User-Agent": pluto.UserAgent}
	if s.config.XFF != "" {
		headers["X-Forwarded-For"] = s.config.XFF
	}
	stream["behaviorHints"] = map[string]any{
		"proxyHeaders": map[string]any{"request": headers},
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"streams": []any{stream}})
}

func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 {
		writeError(w, r, http.StatusNotFound, "not found")
		return
	}
	session, ok := s.manager.Get(parts[1])
	if !ok {
		writeError(w, r, http.StatusNotFound, "session expired")
		return
	}
	action := parts[2]
	var (
		text        string
		contentType = "application/vnd.apple.mpegurl"
		err         error
	)
	switch action {
	case "master.m3u8":
		text, err = session.MasterPlaylist()
	case "subs.m3u8":
		text, err = session.SubtitlePlaylist(r.Context())
	case "subs.vtt":
		subtitle, subtitleErr := session.Subtitle(r.Context())
		contentType = "text/vtt; charset=utf-8"
		if subtitleErr != nil {
			s.logger.Debug("subtitle unavailable", "session", session.ID, "error", subtitleErr)
			text = "WEBVTT\n\n"
		} else {
			text = subtitle.VTT
		}
	case "v":
		if len(parts) < 4 {
			writeError(w, r, http.StatusNotFound, "missing variant index")
			return
		}
		index, parseErr := strconv.Atoi(strings.TrimSuffix(parts[3], ".m3u8"))
		if parseErr != nil {
			writeError(w, r, http.StatusNotFound, "invalid variant index")
			return
		}
		variant, variantErr := session.Variant(r.Context(), index)
		if variantErr != nil {
			err = variantErr
		} else {
			text = variant.Text
		}
	default:
		writeError(w, r, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.logger.Warn("hls request failed", "session", session.ID, "action", action, "error", err)
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	writeText(w, r, http.StatusOK, contentType, "no-store", text)
}

func (s *Server) publicBaseURL(r *http.Request) string {
	if base := strings.TrimSpace(s.config.PublicURL); base != "" {
		return strings.TrimRight(base, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := firstHeaderValue(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = forwarded
	}
	host := firstHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return scheme + "://" + host
}

func streamMediaID(rawID string) (string, bool) {
	for _, prefix := range []string{"pluto:movie:", "pluto:episode:"} {
		if strings.HasPrefix(rawID, prefix) && len(rawID) > len(prefix) {
			return strings.TrimPrefix(rawID, prefix), true
		}
	}
	return "", false
}

func splitResource(path string) (resource, mediaType, id string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 {
		return "", "", "", false
	}
	resource = parts[0]
	mediaType = parts[1]
	id = strings.TrimSuffix(parts[2], ".json")
	if decoded, err := url.PathUnescape(id); err == nil {
		id = decoded
	}
	if id == "" {
		return "", "", "", false
	}
	return resource, mediaType, id, true
}

func firstHeaderValue(value string) string {
	if index := strings.IndexByte(value, ','); index >= 0 {
		value = value[:index]
	}
	return strings.TrimSpace(value)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

func (r *statusRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	writeBody(w, r, status, "application/json; charset=utf-8", "no-store", body)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	body, _ := json.Marshal(map[string]string{"error": message})
	writeBody(w, r, status, "application/json; charset=utf-8", "no-store", body)
}

func writeText(w http.ResponseWriter, r *http.Request, status int, contentType, cacheControl, text string) {
	writeBody(w, r, status, contentType, cacheControl, []byte(text))
}

func writeBody(w http.ResponseWriter, r *http.Request, status int, contentType, cacheControl string, body []byte) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
