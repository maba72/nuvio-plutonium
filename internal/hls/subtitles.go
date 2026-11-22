package hls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"nuvio-plutonium/internal/pluto"
	"nuvio-plutonium/internal/vtt"
)

var (
	placeholderRE = regexp.MustCompile(`(?i)/playout/placeholder/subtitles/`)
	chunkRE       = regexp.MustCompile(`/([0-9]+)-([0-9]+|end)/(hls/)?(text_en_cc|en-cc|en)/`)
)

// Subtitle is a merged, retimed WebVTT track for one title.
type Subtitle struct {
	VTT      string
	Duration float64
	CueCount int
}

type subtitleJob struct {
	done chan struct{}
	sub  *Subtitle
	err  error
}

type subtitleMeta struct {
	Duration float64   `json:"duration"`
	CueCount int       `json:"cueCount"`
	Created  time.Time `json:"created"`
}

// SubtitleGenerator merges Pluto's chunk-relative VTT segments into one
// ad-free WebVTT timeline.
type SubtitleGenerator struct {
	client  *pluto.Client
	dir     string
	ttl     time.Duration
	workers int
	logger  *slog.Logger

	mu   sync.Mutex
	jobs map[string]*subtitleJob
}

func NewSubtitleGenerator(client *pluto.Client, cacheDir string, ttl time.Duration, workers int, logger *slog.Logger) *SubtitleGenerator {
	if workers <= 0 {
		workers = 32
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SubtitleGenerator{
		client:  client,
		dir:     filepath.Join(cacheDir, "subtitles"),
		ttl:     ttl,
		workers: workers,
		logger:  logger,
		jobs:    make(map[string]*subtitleJob),
	}
}

// Get returns a merged subtitle track, using the on-disk cache when possible.
func (g *SubtitleGenerator) Get(ctx context.Context, mediaID, playlistURL string) (*Subtitle, error) {
	key := subtitleKey(mediaID, playlistURL)
	if cached, ok := g.readDisk(key); ok {
		return cached, nil
	}

	g.mu.Lock()
	if job, ok := g.jobs[key]; ok {
		g.mu.Unlock()
		select {
		case <-job.done:
			return job.sub, job.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	job := &subtitleJob{done: make(chan struct{})}
	g.jobs[key] = job
	g.mu.Unlock()

	sub, err := g.generate(mediaID, playlistURL)

	g.mu.Lock()
	job.sub = sub
	job.err = err
	close(job.done)
	// Drop the job after completion; concurrent waiters already hold the job
	// pointer, and later callers use the disk cache. Keeping successful jobs
	// forever would retain a full WebVTT track for every title ever played.
	delete(g.jobs, key)
	g.mu.Unlock()
	return sub, err
}

func subtitleKey(mediaID, playlistURL string) string {
	path := playlistURL
	if parsed, err := url.Parse(playlistURL); err == nil {
		path = parsed.Path
	}
	// Bump when the merge/dedupe format changes so old cached tracks are not
	// reused after an upgrade.
	sum := sha256.Sum256([]byte("v2|" + mediaID + "|" + path))
	return hex.EncodeToString(sum[:16])
}

func (g *SubtitleGenerator) readDisk(key string) (*Subtitle, bool) {
	vttPath := filepath.Join(g.dir, key+".vtt")
	info, err := os.Stat(vttPath)
	if err != nil {
		return nil, false
	}
	if g.ttl > 0 && time.Since(info.ModTime()) > g.ttl {
		return nil, false
	}
	body, err := os.ReadFile(vttPath)
	if err != nil {
		return nil, false
	}
	text := string(body)
	meta := subtitleMeta{}
	if raw, err := os.ReadFile(filepath.Join(g.dir, key+".json")); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}
	if meta.Duration <= 0 {
		cues := vtt.Parse(text, 0)
		meta.Duration = float64(vtt.DurationMS(cues)) / 1000
		meta.CueCount = len(cues)
	}
	return &Subtitle{VTT: text, Duration: meta.Duration, CueCount: meta.CueCount}, true
}

func (g *SubtitleGenerator) generate(mediaID, playlistURL string) (*Subtitle, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	playlist, err := g.client.GetText(ctx, playlistURL, 30*time.Second)
	if err != nil {
		return nil, err
	}
	segments, contentDuration, skipped := parseSubtitleSegments(playlist, playlistURL)
	if len(segments) == 0 {
		return nil, errors.New("subtitle playlist had no content segments")
	}

	results := make([][]vtt.Cue, len(segments))
	jobs := make(chan int)
	workerCount := g.workers
	if workerCount > len(segments) {
		workerCount = len(segments)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := 0
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				segment := segments[index]
				body, err := g.fetchSegment(ctx, segment.URL)
				if err != nil {
					mu.Lock()
					failures++
					mu.Unlock()
					g.logger.Debug("subtitle segment failed",
						"media", mediaID, "index", index, "error", err)
					continue
				}
				results[index] = vtt.Parse(body, segment.OffsetMS)
			}
		}()
	}
sendLoop:
	for index := range segments {
		select {
		case jobs <- index:
		case <-ctx.Done():
			break sendLoop
		}
	}
	close(jobs)
	wg.Wait()

	var all []vtt.Cue
	for _, cues := range results {
		all = append(all, cues...)
	}
	cleaned := vtt.Clean(all)
	text := vtt.Render(cleaned)
	cueDuration := float64(vtt.DurationMS(cleaned)) / 1000
	duration := contentDuration
	if cueDuration > duration {
		duration = cueDuration
	}
	if duration <= 0 {
		duration = cueDuration
	}

	if err := g.writeDisk(mediaID, playlistURL, text, duration, len(cleaned)); err != nil {
		g.logger.Warn("could not write subtitle cache", "media", mediaID, "error", err)
	}
	g.logger.Info("subtitles ready",
		"media", mediaID,
		"segments", len(segments),
		"skipped", skipped,
		"cues", len(cleaned),
		"duration", roundSeconds(duration),
		"failures", failures,
		"elapsed", time.Since(started).Round(time.Millisecond),
	)
	return &Subtitle{VTT: text, Duration: duration, CueCount: len(cleaned)}, nil
}

func (g *SubtitleGenerator) fetchSegment(ctx context.Context, segmentURL string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		body, err := g.client.GetText(ctx, segmentURL, 20*time.Second)
		if err == nil {
			return body, nil
		}
		lastErr = err
		var httpErr *pluto.HTTPError
		if errors.As(err, &httpErr) && !httpErr.Retryable() {
			break
		}
		if attempt < 2 {
			select {
			case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	return "", lastErr
}

type subtitleSegment struct {
	URL      string
	OffsetMS int64
}

func parseSubtitleSegments(playlist, playlistURL string) ([]subtitleSegment, float64, int) {
	base, _ := url.Parse(playlistURL)
	lines := strings.Split(strings.ReplaceAll(playlist, "\r\n", "\n"), "\n")
	segments := make([]subtitleSegment, 0, 1600)
	var pendingDuration float64
	var contentDuration float64
	skipped := 0
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF") {
			if match := extinfRE.FindStringSubmatch(line); match != nil {
				pendingDuration, _ = strconv.ParseFloat(match[1], 64)
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		segmentURL := line
		if base != nil {
			if parsed, err := url.Parse(line); err == nil {
				segmentURL = base.ResolveReference(parsed).String()
			}
		}
		if placeholderRE.MatchString(segmentURL) {
			// Ad placeholder: drop both the segment and its duration.
			pendingDuration = 0
			continue
		}
		offsetMS, ok := chunkOffsetMS(segmentURL)
		if !ok {
			// Pluto also emits empty filler VTTs (for example
			// siloh-ns1.plutotv.net/prd/placeholders/empty_90k.vtt) that carry
			// no captions but are part of the subtitle timeline. Keep their
			// duration so the served track matches the video, but do not try
			// to parse them at offset zero.
			contentDuration += pendingDuration
			pendingDuration = 0
			skipped++
			continue
		}
		segments = append(segments, subtitleSegment{URL: segmentURL, OffsetMS: offsetMS})
		contentDuration += pendingDuration
		pendingDuration = 0
	}
	return segments, contentDuration, skipped
}

func chunkOffsetMS(segmentURL string) (int64, bool) {
	match := chunkRE.FindStringSubmatch(segmentURL)
	if match == nil {
		return 0, false
	}
	offset, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return offset, true
}

func (g *SubtitleGenerator) writeDisk(mediaID, playlistURL, text string, duration float64, cueCount int) error {
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return err
	}
	key := subtitleKey(mediaID, playlistURL)
	vttPath := filepath.Join(g.dir, key+".vtt")
	metaPath := filepath.Join(g.dir, key+".json")
	if err := writeFileAtomic(vttPath, []byte(text), 0o644); err != nil {
		return err
	}
	meta, err := json.Marshal(subtitleMeta{
		Duration: duration,
		CueCount: cueCount,
		Created:  time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return writeFileAtomic(metaPath, meta, 0o644)
}

func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(body); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// JobCount reports the number of in-flight or completed subtitle jobs.
func (g *SubtitleGenerator) JobCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.jobs)
}

func roundSeconds(value float64) float64 {
	return float64(int64(value*1000+0.5)) / 1000
}
