package hls

import "testing"

func TestParseSubtitleSegmentsMatchesKodiOffsets(t *testing.T) {
	playlist := "#EXTM3U\n" +
		"#EXTINF:5,\n" +
		"https://cdn.example/636_pluto/clip/abc/713464-1241617/hls/text_en_cc/seg_00000.vtt\n" +
		"#EXTINF:5,\n" +
		"https://cdn.example/636_pluto/clip/abc/713464-1241617/hls/text_en_cc/seg_00001.vtt\n" +
		"#EXTINF:5,\n" +
		"https://cdn.example/playout/placeholder/subtitles/seg_00002.vtt\n" +
		"#EXTINF:0.133,\n" +
		"https://cdn.example/prd/placeholders/empty_90k.vtt\n"
	segments, duration, skipped := parseSubtitleSegments(playlist, "https://cdn.example/subtitle/subs/en/playlist.m3u8")
	if len(segments) != 2 {
		t.Fatalf("got %d segments, want 2", len(segments))
	}
	if segments[0].OffsetMS != 713464 || segments[1].OffsetMS != 713464 {
		t.Fatalf("unexpected offsets: %+v", segments)
	}
	if duration != 10.133 {
		t.Fatalf("got duration %v, want 10.133", duration)
	}
	if skipped != 1 {
		t.Fatalf("got %d skipped segments, want 1", skipped)
	}
}
