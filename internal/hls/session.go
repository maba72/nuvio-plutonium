package hls

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"nuvio-plutonium/internal/pluto"
	"nuvio-plutonium/internal/vtt"
)

var (
	audioAttrRE = regexp.MustCompile(`(?i)(^|,)\s*AUDIO="[^"]*"`)
	subsAttrRE  = regexp.MustCompile(`(?i)(^|,)\s*SUBTITLES="[^"]*"`)
)

// SubtitleRendition describes the upstream subtitle playlist.
type SubtitleRendition struct {
	URI      string
	Language string
	Name     string
}

// MasterVariant is a video variant from Pluto's master playlist.
type MasterVariant struct {
	Attributes string
	URL        string
}

// Master is the parsed upstream master playlist.
type Master struct {
	Variants []MasterVariant
	Subtitle *SubtitleRendition
}

// Variant is a cleaned video media playlist.
type Variant struct {
	Text         string
	Duration     float64
	SegmentCount int
	AdSegments   int
	AdRanges     []Range
}

type variantJob struct {
	done    chan struct{}
	variant *Variant
	err     error
}

// Session holds one title's playback state.
type Session struct {
	ID        string
	MediaID   string
	MasterURL string
	Created   time.Time

	client         *pluto.Client
	logger         *slog.Logger
	master         *Master
	subtitleOffset time.Duration

	variantMu   sync.Mutex
	variants    map[int]*Variant
	variantJobs map[int]*variantJob

	subsOnce sync.Once
	subsDone chan struct{}
	subsMu   sync.Mutex
	subs     *Subtitle
	subsErr  error

	// rebasedMu caches the ad-free rendition of the merged subtitle track.
	// The on-disk subtitle stays canonical (original timeline) so the same
	// cached track can be reused across sessions with different ad breaks.
	rebasedMu   sync.Mutex
	rebased     *Subtitle
	rebasedErr  error
	rebasedDone bool

	durationMu sync.Mutex
	duration   float64
}

// Manager creates and expires playback sessions.
type Manager struct {
	client         *pluto.Client
	subtitles      *SubtitleGenerator
	logger         *slog.Logger
	sessionTTL     time.Duration
	subtitleOffset time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
}

// DefaultSubtitleOffset matches the Kodi addon's default "Subtitle sync delay"
// of 2 seconds. Pluto's captions consistently run ahead of the video, so a
// positive offset delays the cues to line up with the picture.
const DefaultSubtitleOffset = 2 * time.Second

func NewManager(client *pluto.Client, subtitles *SubtitleGenerator, sessionTTL, subtitleOffset time.Duration, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if sessionTTL <= 0 {
		sessionTTL = 24 * time.Hour
	}
	return &Manager{
		client:         client,
		subtitles:      subtitles,
		logger:         logger,
		sessionTTL:     sessionTTL,
		subtitleOffset: subtitleOffset,
		sessions:       make(map[string]*Session),
	}
}

// NewSession resolves the raw master and returns a session whose URLs can be
// served to the player. Subtitles are generated in the background so the
// stream endpoint never waits for them (mirroring the Kodi addon, which starts
// playback first and attaches captions once they are ready).
func (m *Manager) NewSession(ctx context.Context, mediaID string) (*Session, error) {
	masterURL, err := m.client.MasterURL(ctx, mediaID)
	if err != nil {
		return nil, err
	}
	session := &Session{
		ID:             randomID(),
		MediaID:        mediaID,
		MasterURL:      masterURL,
		Created:        time.Now(),
		client:         m.client,
		logger:         m.logger,
		subtitleOffset: m.subtitleOffset,
		variants:       make(map[int]*Variant),
		variantJobs:    make(map[int]*variantJob),
		subsDone:       make(chan struct{}),
	}
	master, err := parseMaster(ctx, m.client, masterURL)
	if err != nil {
		return nil, err
	}
	session.master = master

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	go func() {
		if _, err := session.Variant(context.Background(), 0); err != nil {
			m.logger.Debug("variant prewarm failed", "media", mediaID, "error", err)
		}
	}()
	if master.Subtitle != nil {
		go m.generateSubtitles(session, master.Subtitle.URI)
	} else {
		session.finishSubtitles(nil, errors.New("pluto master had no subtitle rendition"))
	}
	m.logger.Info("session ready",
		"media", mediaID,
		"variants", len(master.Variants),
		"subtitles", master.Subtitle != nil,
		"id", session.ID,
	)
	return session, nil
}

func (m *Manager) generateSubtitles(session *Session, subtitleURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	subtitle, err := m.subtitles.Get(ctx, session.MediaID, subtitleURL)
	if err != nil {
		m.logger.Warn("subtitle generation failed", "media", session.MediaID, "error", err)
	}
	session.finishSubtitles(subtitle, err)
}

func (s *Session) finishSubtitles(subtitle *Subtitle, err error) {
	s.subsOnce.Do(func() {
		s.subsMu.Lock()
		s.subs = subtitle
		s.subsErr = err
		s.subsMu.Unlock()
		close(s.subsDone)
	})
}

// Get returns an unexpired session.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	if time.Since(session.Created) > m.sessionTTL {
		delete(m.sessions, id)
		return nil, false
	}
	return session, true
}

// Count reports the number of live sessions.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// SubtitleJobs reports the number of subtitle jobs held by the generator.
func (m *Manager) SubtitleJobs() int {
	if m.subtitles == nil {
		return 0
	}
	return m.subtitles.JobCount()
}

// Cleanup periodically removes expired sessions.
func (m *Manager) Cleanup(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-m.sessionTTL)
			m.mu.Lock()
			for id, session := range m.sessions {
				if session.Created.Before(cutoff) {
					delete(m.sessions, id)
				}
			}
			m.mu.Unlock()
		}
	}
}

// MasterPlaylist renders the cleaned master with our subtitle rendition and
// audio muxed into the video variants.
func (s *Session) MasterPlaylist() (string, error) {
	if s.master == nil || len(s.master.Variants) == 0 {
		return "", errors.New("no playable variants")
	}
	includeSubtitles := s.shouldIncludeSubtitles()
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:5\n")
	if includeSubtitles {
		language := "en"
		if lang := strings.ToLower(strings.TrimSpace(s.master.Subtitle.Language)); strings.HasPrefix(lang, "en") {
			language = lang
		}
		b.WriteString(`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI="subs.m3u8",LANGUAGE="`)
		b.WriteString(language)
		b.WriteString("\"\n")
	}
	for index, variant := range s.master.Variants {
		attributes := stripAttribute(variant.Attributes, audioAttrRE)
		attributes = stripAttribute(attributes, subsAttrRE)
		if includeSubtitles {
			if attributes != "" {
				attributes += ","
			}
			attributes += `SUBTITLES="subs"`
		}
		b.WriteString("#EXT-X-STREAM-INF:")
		b.WriteString(attributes)
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("v/%d.m3u8\n", index))
	}
	return b.String(), nil
}

func (s *Session) shouldIncludeSubtitles() bool {
	if s.master == nil || s.master.Subtitle == nil {
		return false
	}
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	if s.subs != nil {
		return s.subs.CueCount > 0
	}
	return s.subsErr == nil
}

// Variant returns a cleaned video media playlist.
func (s *Session) Variant(ctx context.Context, index int) (*Variant, error) {
	if s.master == nil || index < 0 || index >= len(s.master.Variants) {
		return nil, fmt.Errorf("variant %d not found", index)
	}
	s.variantMu.Lock()
	if variant, ok := s.variants[index]; ok {
		s.variantMu.Unlock()
		return variant, nil
	}
	if job, ok := s.variantJobs[index]; ok {
		s.variantMu.Unlock()
		select {
		case <-job.done:
			return job.variant, job.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	job := &variantJob{done: make(chan struct{})}
	s.variantJobs[index] = job
	s.variantMu.Unlock()

	started := time.Now()
	source := s.master.Variants[index].URL
	playlist, err := s.client.GetText(ctx, source, 25*time.Second)
	var variant *Variant
	if err == nil {
		playlist = Absolutize(playlist, source)
		result := Strip(playlist)
		variant = &Variant{
			Text:         result.Text,
			Duration:     result.Duration,
			SegmentCount: result.SegmentCount,
			AdSegments:   result.AdSegments,
			AdRanges:     result.AdRanges,
		}
	}

	s.variantMu.Lock()
	delete(s.variantJobs, index)
	if err == nil && variant != nil {
		s.variants[index] = variant
	}
	job.variant = variant
	job.err = err
	close(job.done)
	s.variantMu.Unlock()

	if err != nil {
		s.logger.Warn("variant clean failed", "media", s.MediaID, "variant", index, "error", err)
		return nil, err
	}
	s.durationMu.Lock()
	if variant.Duration > s.duration {
		s.duration = variant.Duration
	}
	s.durationMu.Unlock()
	s.logger.Info("variant ready",
		"media", s.MediaID,
		"variant", index,
		"segments", variant.SegmentCount,
		"ad_segments", variant.AdSegments,
		"duration", roundSeconds(variant.Duration),
		"elapsed", time.Since(started).Round(time.Millisecond),
	)
	return variant, nil
}

func stripAttribute(attributes string, pattern *regexp.Regexp) string {
	attributes = pattern.ReplaceAllString(attributes, "$1")
	for strings.Contains(attributes, ",,") {
		attributes = strings.ReplaceAll(attributes, ",,", ",")
	}
	attributes = strings.Trim(attributes, ",")
	return strings.TrimSpace(attributes)
}

func parseMaster(ctx context.Context, client *pluto.Client, masterURL string) (*Master, error) {
	text, err := client.GetText(ctx, masterURL, 25*time.Second)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(masterURL)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	master := &Master{}
	var subtitleCandidate *SubtitleRendition
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			attributes := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			if strings.EqualFold(attributes["TYPE"], "SUBTITLES") && attributes["URI"] != "" {
				uri := attributes["URI"]
				if base != nil {
					if parsed, err := url.Parse(uri); err == nil {
						uri = base.ResolveReference(parsed).String()
					}
				}
				uri = ensureJWT(uri, masterURL)
				candidate := &SubtitleRendition{
					URI:      uri,
					Language: attributes["LANGUAGE"],
					Name:     attributes["NAME"],
				}
				if subtitleCandidate == nil || isEnglish(candidate) {
					subtitleCandidate = candidate
				}
			}
			continue
		}
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		attributes := strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
		next := index + 1
		for next < len(lines) {
			candidate := strings.TrimSpace(lines[next])
			if candidate != "" && !strings.HasPrefix(candidate, "#") {
				break
			}
			next++
		}
		if next >= len(lines) {
			continue
		}
		uri := strings.TrimSpace(lines[next])
		if base != nil {
			if parsed, err := url.Parse(uri); err == nil {
				uri = base.ResolveReference(parsed).String()
			}
		}
		uri = ensureJWT(uri, masterURL)
		master.Variants = append(master.Variants, MasterVariant{Attributes: attributes, URL: uri})
		index = next
	}
	master.Subtitle = subtitleCandidate
	if len(master.Variants) == 0 {
		return nil, errors.New("pluto master had no variants")
	}
	return master, nil
}

func ensureJWT(rawURL, masterURL string) string {
	target, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if target.Query().Get("jwt") != "" {
		return target.String()
	}
	master, err := url.Parse(masterURL)
	if err != nil {
		return target.String()
	}
	token := master.Query().Get("jwt")
	if token == "" {
		return target.String()
	}
	query := target.Query()
	query.Set("jwt", token)
	target.RawQuery = query.Encode()
	return target.String()
}

func parseAttributes(value string) map[string]string {
	out := make(map[string]string)
	remaining := value
	for len(remaining) > 0 {
		remaining = strings.TrimLeft(remaining, " ,")
		if remaining == "" {
			break
		}
		equals := strings.IndexByte(remaining, '=')
		if equals < 0 {
			break
		}
		key := strings.ToUpper(strings.TrimSpace(remaining[:equals]))
		remaining = remaining[equals+1:]
		var val string
		if strings.HasPrefix(remaining, `"`) {
			remaining = remaining[1:]
			end := strings.IndexByte(remaining, '"')
			if end < 0 {
				val = remaining
				remaining = ""
			} else {
				val = remaining[:end]
				remaining = remaining[end+1:]
			}
		} else {
			end := strings.IndexByte(remaining, ',')
			if end < 0 {
				val = remaining
				remaining = ""
			} else {
				val = remaining[:end]
				remaining = remaining[end+1:]
			}
		}
		if key != "" {
			out[key] = strings.TrimSpace(val)
		}
	}
	return out
}

func isEnglish(rendition *SubtitleRendition) bool {
	language := strings.ToLower(strings.TrimSpace(rendition.Language))
	if language == "en" || strings.HasPrefix(language, "en-") || strings.HasPrefix(language, "en_") {
		return true
	}
	return strings.Contains(strings.ToLower(rendition.Name), "english")
}

// SubtitlePlaylist renders a one-segment WebVTT media playlist.
func (s *Session) SubtitlePlaylist(ctx context.Context) (string, error) {
	subtitle, err := s.Subtitle(ctx)
	if err != nil {
		return "", err
	}
	if subtitle.CueCount == 0 {
		return "", errors.New("subtitle track is empty")
	}
	duration := subtitle.Duration
	if duration <= 0 {
		s.durationMu.Lock()
		duration = s.duration
		s.durationMu.Unlock()
	}
	if duration <= 0 {
		duration = 1
	}
	target := int(math.Ceil(duration))
	if target < 1 {
		target = 1
	}
	return fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:%.3f,\nsubs.vtt\n#EXT-X-ENDLIST\n", target, duration), nil
}

// Subtitle waits for the subtitle job and returns the merged track, retimed
// to the ad-free timeline of this session's cleaned video playlist.
func (s *Session) Subtitle(ctx context.Context) (*Subtitle, error) {
	select {
	case <-s.subsDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.subsMu.Lock()
	sub, subErr := s.subs, s.subsErr
	s.subsMu.Unlock()
	if subErr != nil {
		return nil, subErr
	}
	if sub == nil {
		return nil, errors.New("subtitle track unavailable")
	}

	s.rebasedMu.Lock()
	defer s.rebasedMu.Unlock()
	if s.rebasedDone {
		return s.rebased, s.rebasedErr
	}
	rebased, err := s.rebaseSubtitle(ctx, sub)
	if err == nil {
		s.rebased = rebased
		s.rebasedDone = true
	}
	return rebased, err
}

// rebaseSubtitle applies two transforms to the canonical (original-timeline)
// caption track, mirroring the Kodi addon:
//
//  1. It shifts cues left by the ad windows removed from the video playlist, so
//     captions line up with the stitched-together picture.
//  2. It applies a fixed sync-delay offset (Kodi's subtitle_offset, default
//     +2s) because Pluto's captions consistently run ahead of the video.
//
// Both are applied here at serve time so the on-disk cache stays canonical and
// the offset/rebase can be changed without re-downloading any VTT segments.
func (s *Session) rebaseSubtitle(ctx context.Context, sub *Subtitle) (*Subtitle, error) {
	cues := vtt.Parse(sub.VTT, 0)
	adWindows := 0

	variant, err := s.Variant(ctx, 0)
	if err != nil {
		s.logger.Warn("ad rebase unavailable, applying sync offset only",
			"media", s.MediaID, "error", err)
	} else {
		windows := make([]vtt.Window, 0, len(variant.AdRanges))
		for _, r := range variant.AdRanges {
			if r.End <= r.Start {
				continue
			}
			windows = append(windows, vtt.Window{
				Start: int64(r.Start*1000 + 0.5),
				End:   int64(r.End*1000 + 0.5),
			})
		}
		if len(windows) > 0 {
			cues = vtt.RebaseAfterAds(cues, windows)
			adWindows = len(windows)
		}
	}

	// Pluto's captions run ahead of the picture. Delay every cue by the
	// configured offset (Kodi defaults to 2 seconds) after rebasing so the
	// final times are relative to the ad-free video timeline.
	cues = vtt.Shift(cues, s.subtitleOffset.Milliseconds())

	text := vtt.Render(cues)
	duration := float64(vtt.DurationMS(cues)) / 1000
	if duration <= 0 {
		duration = sub.Duration
	}
	s.logger.Info("captions retimed to ad-free timeline",
		"media", s.MediaID,
		"ad_windows", adWindows,
		"offset_ms", s.subtitleOffset.Milliseconds(),
		"cues", len(cues),
		"duration", roundSeconds(duration),
	)
	return &Subtitle{VTT: text, Duration: duration, CueCount: len(cues)}, nil
}

func randomID() string {
	var body [16]byte
	if _, err := rand.Read(body[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	text := make([]byte, 32)
	hex.Encode(text, body[:])
	return string(text)
}
