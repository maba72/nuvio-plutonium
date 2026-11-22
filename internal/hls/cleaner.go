// Package hls cleans Pluto HLS playlists and builds retimed subtitle tracks.
package hls

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	extinfRE    = regexp.MustCompile(`^#EXTINF:\s*([0-9.]+)`)
	adRE        = regexp.MustCompile(`(?i)(/1_ad/creative/|plutotv_kidsbumpers|plutotv_filler|_ad(/|%2F|_bumper)|dai\.google\.com)`)
	uriAttrRE   = regexp.MustCompile(`URI="([^"]*)"`)
	targetDurRE = regexp.MustCompile(`(?m)^#EXT-X-TARGETDURATION:\s*([0-9]+)`)
)

// Range is a half-open interval in seconds.
type Range struct {
	Start float64
	End   float64
}

// CleanResult is a media playlist with ad segments removed.
type CleanResult struct {
	Text         string
	AdRanges     []Range
	Duration     float64
	SegmentCount int
	AdSegments   int
}

// Strip removes Pluto's SSAI ad segments from a media playlist.
//
// Pluto marks VOD ads with #EXT-X-KEY:METHOD=NONE and/or an ad-specific URL.
// Ad segments are dropped together with their buffered EXTINF, key, map, and
// discontinuity tags so no phantom period boundary survives the cut.
func Strip(playlist string) CleanResult {
	playlist = strings.TrimPrefix(playlist, "\ufeff")
	// Mirror the Kodi addon's guard: Pluto VOD content is AES-128 encrypted and
	// ad segments are METHOD=NONE. An unencrypted playlist is not the SSAI VOD
	// shape this cleaner understands, so pass it through rather than risk
	// dropping content.
	if !strings.Contains(playlist, "METHOD=AES-128") {
		return passthrough(playlist)
	}
	lines := strings.Split(strings.ReplaceAll(playlist, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	pending := make([]string, 0, 4)
	var (
		cursor     float64
		adStart    float64
		adActive   bool
		lastWasAd  bool
		noneRegion bool
		pendingDur float64
		maxDur     float64
		duration   float64
		segments   int
		adSegments int
		adRanges   []Range
	)
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, pending...)
		pending = pending[:0]
	}
	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "#EXT-X-KEY:"):
			if strings.Contains(strings.ToUpper(trimmed), "METHOD=NONE") {
				noneRegion = true
			} else if strings.Contains(strings.ToUpper(trimmed), "METHOD=AES-128") {
				noneRegion = false
			}
			pending = append(pending, trimmed)
			continue
		case strings.HasPrefix(trimmed, "#EXTINF"):
			if match := extinfRE.FindStringSubmatch(trimmed); match != nil {
				pendingDur, _ = strconv.ParseFloat(match[1], 64)
			} else {
				pendingDur = 0
			}
			pending = append(pending, trimmed)
			continue
		case strings.HasPrefix(trimmed, "#EXT-X-DISCONTINUITY"):
			// A discontinuity immediately after a stripped ad belongs to the
			// ad/content boundary and must not survive.
			if lastWasAd {
				continue
			}
			pending = append(pending, trimmed)
			continue
		case strings.HasPrefix(trimmed, "#EXT-X-MAP:"),
			strings.HasPrefix(trimmed, "#EXT-X-BYTERANGE"),
			strings.HasPrefix(trimmed, "#EXT-X-GAP"):
			pending = append(pending, trimmed)
			continue
		case strings.HasPrefix(trimmed, "#EXT-X-PROGRAM-DATE-TIME:"):
			// Keep the date associated with a surviving content segment, but
			// drop dates that belonged to stripped ad segments. Kodi buffers
			// these alongside EXTINF/KEY for the same reason.
			pending = append(pending, trimmed)
			continue
		case strings.HasPrefix(trimmed, "#EXT-X-CUE-OUT"),
			strings.HasPrefix(trimmed, "#EXT-X-CUE-IN"):
			continue
		case strings.HasPrefix(trimmed, "#EXT-X-ENDLIST"):
			continue
		case strings.HasPrefix(trimmed, "#"):
			out = append(out, trimmed)
			continue
		}

		// Media segment.
		if noneRegion || adRE.MatchString(trimmed) {
			pending = pending[:0]
			if !adActive {
				adActive = true
				adStart = cursor
			}
			cursor += pendingDur
			pendingDur = 0
			lastWasAd = true
			adSegments++
			continue
		}
		flushPending()
		out = append(out, trimmed)
		if adActive {
			adRanges = append(adRanges, Range{Start: adStart, End: cursor})
			adActive = false
		}
		if pendingDur > maxDur {
			maxDur = pendingDur
		}
		duration += pendingDur
		cursor += pendingDur
		pendingDur = 0
		lastWasAd = false
		segments++
	}
	flushPending()
	if adActive {
		adRanges = append(adRanges, Range{Start: adStart, End: cursor})
	}
	if len(out) > 0 && out[0] != "#EXTM3U" {
		out = append([]string{"#EXTM3U"}, out...)
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "#EXT-X-PLAYLIST-TYPE:") {
		joined = insertAfterFirst(joined, "#EXT-X-PLAYLIST-TYPE:VOD")
	}
	if maxDur > 0 {
		needed := int(math.Ceil(maxDur))
		if match := targetDurRE.FindStringSubmatch(joined); match != nil {
			current, _ := strconv.Atoi(match[1])
			if current < needed {
				joined = targetDurRE.ReplaceAllString(joined, fmt.Sprintf("#EXT-X-TARGETDURATION:%d", needed))
			}
		} else {
			joined = insertAfterFirst(joined, fmt.Sprintf("#EXT-X-TARGETDURATION:%d", needed))
		}
	}
	joined = strings.TrimRight(joined, "\n") + "\n#EXT-X-ENDLIST\n"
	return CleanResult{
		Text:         joined,
		AdRanges:     adRanges,
		Duration:     duration,
		SegmentCount: segments,
		AdSegments:   adSegments,
	}
}

func passthrough(playlist string) CleanResult {
	lines := strings.Split(strings.ReplaceAll(playlist, "\r\n", "\n"), "\n")
	var duration float64
	segments := 0
	out := make([]string, 0, len(lines))
	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#EXT-X-ENDLIST") {
			continue
		}
		if match := extinfRE.FindStringSubmatch(trimmed); match != nil {
			if value, err := strconv.ParseFloat(match[1], 64); err == nil {
				duration += value
			}
		}
		if !strings.HasPrefix(trimmed, "#") {
			segments++
		}
		out = append(out, trimmed)
	}
	joined := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n#EXT-X-ENDLIST\n"
	return CleanResult{
		Text:         joined,
		Duration:     duration,
		SegmentCount: segments,
	}
}

func insertAfterFirst(playlist, line string) string {
	if strings.HasPrefix(playlist, "#EXTM3U") {
		if newline := strings.IndexByte(playlist, '\n'); newline >= 0 {
			return playlist[:newline+1] + line + "\n" + playlist[newline+1:]
		}
		return playlist + "\n" + line
	}
	return line + "\n" + playlist
}

// Absolutize resolves relative segment and URI attributes against baseURL.
func Absolutize(playlist, baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return playlist
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return playlist
	}
	lines := strings.Split(strings.ReplaceAll(playlist, "\r\n", "\n"), "\n")
	for index, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			lines[index] = ""
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if strings.Contains(trimmed, `URI="`) {
				trimmed = uriAttrRE.ReplaceAllStringFunc(trimmed, func(attribute string) string {
					match := uriAttrRE.FindStringSubmatch(attribute)
					if len(match) != 2 {
						return attribute
					}
					return `URI="` + resolveURL(base, match[1]) + `"`
				})
			}
			lines[index] = trimmed
			continue
		}
		lines[index] = resolveURL(base, trimmed)
	}
	return strings.Join(lines, "\n")
}

func resolveURL(base *url.URL, reference string) string {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return reference
	}
	parsed, err := url.Parse(reference)
	if err != nil {
		return reference
	}
	if parsed.IsAbs() {
		return reference
	}
	return base.ResolveReference(parsed).String()
}
