// Package vtt parses, retimes, deduplicates, and renders WebVTT cues.
package vtt

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	fullTimeRE  = regexp.MustCompile(`^(\d{1,3}):(\d{2}):(\d{2})[.,](\d{1,3})\s*-->\s*(\d{1,3}):(\d{2}):(\d{2})[.,](\d{1,3})(.*)$`)
	shortTimeRE = regexp.MustCompile(`^(\d{2}):(\d{2})[.,](\d{1,3})\s*-->\s*(\d{2}):(\d{2})[.,](\d{1,3})(.*)$`)
)

// Cue is a single WebVTT cue. Times are milliseconds in the output timeline.
type Cue struct {
	Start    int64
	End      int64
	Settings string
	Text     []string
}

// Parse extracts cues from a WebVTT fragment and applies offsetMS to every cue.
func Parse(data string, offsetMS int64) []Cue {
	data = strings.TrimPrefix(data, "\ufeff")
	lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	cues := make([]Cue, 0, 16)
	var current *Cue
	flush := func() {
		if current == nil {
			return
		}
		current.Text = trimText(current.Text)
		cues = append(cues, *current)
		current = nil
	}
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if start, end, settings, ok := parseTimestampLine(trimmed); ok {
			flush()
			current = &Cue{
				Start:    start + offsetMS,
				End:      end + offsetMS,
				Settings: settings,
			}
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		if current == nil {
			// WEBVTT header, X-TIMESTAMP-MAP, NOTE blocks, cue identifiers.
			continue
		}
		current.Text = append(current.Text, trimmed)
	}
	flush()
	return cues
}

func parseTimestampLine(line string) (int64, int64, string, bool) {
	if !strings.Contains(line, "-->") {
		return 0, 0, "", false
	}
	if m := fullTimeRE.FindStringSubmatch(line); m != nil {
		start := timestampMS(m[1], m[2], m[3], m[4])
		end := timestampMS(m[5], m[6], m[7], m[8])
		return start, end, strings.TrimSpace(m[9]), true
	}
	if m := shortTimeRE.FindStringSubmatch(line); m != nil {
		start := timestampMS("0", m[1], m[2], m[3])
		end := timestampMS("0", m[4], m[5], m[6])
		return start, end, strings.TrimSpace(m[7]), true
	}
	return 0, 0, "", false
}

func timestampMS(hours, minutes, seconds, fraction string) int64 {
	h, _ := strconv.ParseInt(hours, 10, 64)
	m, _ := strconv.ParseInt(minutes, 10, 64)
	s, _ := strconv.ParseInt(seconds, 10, 64)
	frac := int64(0)
	if fraction != "" {
		value, _ := strconv.ParseInt(fraction, 10, 64)
		switch len(fraction) {
		case 1:
			frac = value * 100
		case 2:
			frac = value * 10
		default:
			frac = value
		}
	}
	return ((h*60+m)*60+s)*1000 + frac
}

func trimText(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Clean sorts cues and removes duplicates, mirroring the Kodi addon's
// canonical subtitle path. Cues from adjacent overlapping VTT segments repeat
// with identical times and text but sometimes different cue settings, so
// settings are deliberately excluded from the duplicate key. Empty cues are
// kept because Pluto uses them to clear the screen between caption runs (the
// final one also extends the track to the end of the film). The input order is
// preserved for equal timestamps so the first fetched segment wins, as in
// Kodi.
func Clean(cues []Cue) []Cue {
	sort.SliceStable(cues, func(i, j int) bool {
		if cues[i].Start != cues[j].Start {
			return cues[i].Start < cues[j].Start
		}
		return cues[i].End < cues[j].End
	})
	out := make([]Cue, 0, len(cues))
	seen := make(map[string]struct{}, len(cues))
	for _, cue := range cues {
		if cue.Start < 0 {
			cue.Start = 0
		}
		if cue.End <= cue.Start {
			continue
		}
		key := fmt.Sprintf("%d|%d|%s", cue.Start, cue.End, strings.Join(cue.Text, "\n"))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, cue)
	}
	return out
}

// Window is a half-open interval on the original (ad-inclusive) timeline.
// Times are milliseconds.
type Window struct {
	Start int64
	End   int64
}

// RebaseAfterAds maps captions from Pluto's original timeline onto the
// ad-free timeline produced by the HLS cleaner. A cue that starts inside a
// removed ad window is dropped; every other cue is shifted left by the total
// length of all ad windows that end at or before its start.
//
// This mirrors the Kodi addon's rebase_after_ad_windows() helper. Pluto's
// caption chunk offsets and the cleaner's ad ranges both refer to the same
// original timeline, so applying the shift here keeps captions in sync with
// the stitched-together video. Settings and text are preserved verbatim.
func RebaseAfterAds(cues []Cue, windows []Window) []Cue {
	if len(windows) == 0 {
		return cues
	}
	// Sort once so the cumulative deduction can be computed in a single pass.
	ordered := make([]Window, len(windows))
	copy(ordered, windows)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Start != ordered[j].Start {
			return ordered[i].Start < ordered[j].Start
		}
		return ordered[i].End < ordered[j].End
	})

	out := make([]Cue, 0, len(cues))
	next := 0
	var deducted int64
	for _, cue := range cues {
		// Advance past every ad window that ends at or before this cue starts.
		for next < len(ordered) && ordered[next].End <= cue.Start {
			deducted += ordered[next].End - ordered[next].Start
			next++
		}
		// A cue beginning inside an ad window has no picture to accompany on
		// the ad-free timeline, so it is discarded.
		if next < len(ordered) && ordered[next].Start <= cue.Start && cue.Start < ordered[next].End {
			continue
		}
		cue.Start -= deducted
		cue.End -= deducted
		if cue.End <= cue.Start {
			continue
		}
		out = append(out, cue)
	}
	return out
}

// Shift adds deltaMS to every cue's start and end times. A positive delta
// delays the captions, which is used to counter Pluto's captions running ahead
// of the video picture. Cues shifted before zero are clamped to zero.
func Shift(cues []Cue, deltaMS int64) []Cue {
	if deltaMS == 0 {
		return cues
	}
	out := make([]Cue, len(cues))
	copy(out, cues)
	for i := range out {
		out[i].Start += deltaMS
		out[i].End += deltaMS
		if out[i].Start < 0 {
			out[i].Start = 0
		}
		if out[i].End < 0 {
			out[i].End = 0
		}
	}
	return out
}

// Render formats cues as a standalone WebVTT document.
func Render(cues []Cue) string {
	var b strings.Builder
	b.Grow(64 + len(cues)*80)
	b.WriteString("WEBVTT\n\n")
	for _, cue := range cues {
		b.WriteString(FormatTime(cue.Start))
		b.WriteString(" --> ")
		b.WriteString(FormatTime(cue.End))
		if cue.Settings != "" {
			b.WriteString(" ")
			b.WriteString(cue.Settings)
		}
		b.WriteString("\n")
		for _, line := range cue.Text {
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// FormatTime renders milliseconds as HH:MM:SS.mmm.
func FormatTime(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	ms -= h * 3_600_000
	m := ms / 60_000
	ms -= m * 60_000
	s := ms / 1000
	ms -= s * 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

// DurationMS returns the end time of the last cue in milliseconds.
func DurationMS(cues []Cue) int64 {
	var end int64
	for _, cue := range cues {
		if cue.End > end {
			end = cue.End
		}
	}
	return end
}
