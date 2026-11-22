package hls

import (
	"strings"
	"testing"
)

func TestStripRemovesAdsAndKeepsContentKeys(t *testing.T) {
	playlist := `#EXTM3U
#EXT-X-VERSION:5
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-TARGETDURATION:5
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PROGRAM-DATE-TIME:2026-11-02T17:23:29.782Z
#EXT-X-KEY:METHOD=AES-128,URI="key0",IV=0x1
#EXTINF:5,
content0.ts
#EXT-X-PROGRAM-DATE-TIME:2026-11-02T17:23:34.782Z
#EXT-X-KEY:METHOD=NONE
#EXTINF:5,
https://cdn.example/1_ad/creative/ad0.ts
#EXT-X-DISCONTINUITY
#EXT-X-KEY:METHOD=AES-128,URI="key1",IV=0x2
#EXTINF:5.5,
content1.ts
#EXT-X-KEY:METHOD=NONE
#EXTINF:4,
https://cdn.example/_ad/bumper.ts
#EXT-X-DISCONTINUITY
#EXT-X-KEY:METHOD=AES-128,URI="key2",IV=0x3
#EXTINF:5,
content2.ts
#EXT-X-ENDLIST
`
	result := Strip(playlist)
	if result.AdSegments != 2 {
		t.Fatalf("got %d ad segments, want 2", result.AdSegments)
	}
	if result.SegmentCount != 3 {
		t.Fatalf("got %d content segments, want 3", result.SegmentCount)
	}
	if result.Duration != 15.5 {
		t.Fatalf("got duration %v, want 15.5", result.Duration)
	}
	if len(result.AdRanges) != 2 {
		t.Fatalf("got %d ad ranges, want 2", len(result.AdRanges))
	}
	if result.AdRanges[0].Start != 5 || result.AdRanges[0].End != 10 {
		t.Fatalf("unexpected first ad range: %+v", result.AdRanges[0])
	}
	if strings.Contains(result.Text, "METHOD=NONE") {
		t.Fatalf("ad key survived:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "#EXT-X-DISCONTINUITY") {
		t.Fatalf("ad discontinuity survived:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "17:23:34.782") {
		t.Fatalf("ad program-date-time survived:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "17:23:29.782") {
		t.Fatalf("content program-date-time was dropped:\n%s", result.Text)
	}
	for _, expected := range []string{"content0.ts", "content1.ts", "content2.ts", `URI="key2"`} {
		if !strings.Contains(result.Text, expected) {
			t.Fatalf("missing %q:\n%s", expected, result.Text)
		}
	}
	if strings.Count(result.Text, "#EXT-X-ENDLIST") != 1 {
		t.Fatalf("expected one ENDLIST:\n%s", result.Text)
	}
}

func TestStripPassesUnencryptedPlaylistThrough(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:5\n#EXTINF:5,\nsegment.ts\n"
	result := Strip(playlist)
	if result.AdSegments != 0 || result.SegmentCount != 1 || result.Duration != 5 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !strings.Contains(result.Text, "segment.ts") || !strings.Contains(result.Text, "#EXT-X-ENDLIST") {
		t.Fatalf("unexpected text:\n%s", result.Text)
	}
}

func TestAbsolutize(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"seg_keyfile_0.key\"\n#EXTINF:5,\nseg_00000.ts\n"
	got := Absolutize(playlist, "https://cdn.example/a/b/playlist.m3u8?jwt=x")
	if !strings.Contains(got, `URI="https://cdn.example/a/b/seg_keyfile_0.key"`) {
		t.Fatalf("key not absolutized:\n%s", got)
	}
	if !strings.Contains(got, "https://cdn.example/a/b/seg_00000.ts") {
		t.Fatalf("segment not absolutized:\n%s", got)
	}
}

func TestChunkOffsetPatterns(t *testing.T) {
	cases := map[string]int64{
		"https://cdn/636_pluto/clip/abc/713464-1241617/hls/text_en_cc/seg_00000.vtt":  713464,
		"https://cdn/636_pluto/clip/abc/hls/0-898524/en-cc/00000.vtt":                 0,
		"https://cdn/636_pluto/clip/abc/hls/0-246147/en/en.m3u8_0000000000.vtt":       0,
		"https://cdn/636_pluto/clip/abc/1150901-1696238/hls/text_en_cc/seg_00230.vtt": 1150901,
	}
	for input, want := range cases {
		got, ok := chunkOffsetMS(input)
		if !ok || got != want {
			t.Fatalf("chunkOffsetMS(%q) = %d, %v; want %d, true", input, got, ok, want)
		}
	}
}
