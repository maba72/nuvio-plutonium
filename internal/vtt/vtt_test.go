package vtt

import "testing"

func TestParseOffsetsAndSettings(t *testing.T) {
	body := "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:90000,LOCAL:00:00:00.000\n\n" +
		"00:00:02.000 --> 00:00:03.500 line:90% align:center\nHELLO\n\n" +
		"00:00:04.000 --> 00:00:05.000\n<i>WORLD</i>\n"
	cues := Parse(body, 1_150_901)
	if len(cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(cues))
	}
	if cues[0].Start != 1_152_901 || cues[0].End != 1_154_401 {
		t.Fatalf("unexpected first cue: %+v", cues[0])
	}
	if cues[0].Settings != "line:90% align:center" {
		t.Fatalf("unexpected settings: %q", cues[0].Settings)
	}
	if len(cues[0].Text) != 1 || cues[0].Text[0] != "HELLO" {
		t.Fatalf("unexpected text: %#v", cues[0].Text)
	}
}

func TestCleanDeduplicatesAdjacentSegments(t *testing.T) {
	cues := []Cue{
		{Start: 1000, End: 2000, Settings: "line:90%", Text: []string{"one"}},
		{Start: 1000, End: 2000, Settings: "line:90% align:center", Text: []string{"one"}},
		{Start: 2000, End: 3000, Text: []string{"two"}},
		{Start: 2500, End: 2600, Text: []string{""}},
	}
	cleaned := Clean(cues)
	if len(cleaned) != 3 {
		t.Fatalf("got %d cues, want 3", len(cleaned))
	}
	if cleaned[0].Start != 1000 || cleaned[0].Settings != "line:90%" {
		t.Fatalf("unexpected first cue: %+v", cleaned[0])
	}
	if cleaned[1].Start != 2000 || cleaned[2].Start != 2500 {
		t.Fatalf("unexpected order: %+v", cleaned)
	}
}

func TestCleanKeepsEmptyCues(t *testing.T) {
	cleaned := Clean([]Cue{{Start: 7356_903, End: 7425_978}})
	if len(cleaned) != 1 {
		t.Fatalf("got %d cues, want 1", len(cleaned))
	}
}

func TestRenderTimeFormat(t *testing.T) {
	body := Render([]Cue{{Start: 3_725_001, End: 3_726_002, Text: []string{"line"}}})
	want := "WEBVTT\n\n01:02:05.001 --> 01:02:06.002\nline\n\n"
	if body != want {
		t.Fatalf("got %q, want %q", body, want)
	}
}

func TestShortTimestamps(t *testing.T) {
	cues := Parse("WEBVTT\n\n02:03.500 --> 02:04.000\nshort\n", 0)
	if len(cues) != 1 || cues[0].Start != 123_500 || cues[0].End != 124_000 {
		t.Fatalf("unexpected cues: %+v", cues)
	}
}

func TestRebaseAfterAdsShiftsCuesAndDropsAdCues(t *testing.T) {
	// Two ad windows removed from the original timeline: 10-20s and 40-55s.
	windows := []Window{{Start: 10_000, End: 20_000}, {Start: 40_000, End: 55_000}}
	cues := []Cue{
		{Start: 2_000, End: 4_000, Text: []string{"before"}},
		{Start: 12_000, End: 14_000, Text: []string{"inside ad"}},
		{Start: 25_000, End: 27_000, Text: []string{"between"}},
		{Start: 45_000, End: 48_000, Text: []string{"inside ad 2"}},
		{Start: 60_000, End: 62_000, Text: []string{"after"}},
	}
	rebased := RebaseAfterAds(cues, windows)
	if len(rebased) != 3 {
		t.Fatalf("got %d cues, want 3: %+v", len(rebased), rebased)
	}
	// First cue is before any ad: unchanged.
	if rebased[0].Start != 2_000 || rebased[0].End != 4_000 {
		t.Fatalf("unexpected first cue: %+v", rebased[0])
	}
	// 25-27s minus the 10s first ad = 15-17s.
	if rebased[1].Start != 15_000 || rebased[1].End != 17_000 {
		t.Fatalf("unexpected second cue: %+v", rebased[1])
	}
	// 60-62s minus 10s + 15s of ads = 35-37s.
	if rebased[2].Start != 35_000 || rebased[2].End != 37_000 {
		t.Fatalf("unexpected third cue: %+v", rebased[2])
	}
}

func TestRebaseAfterAdsNoWindowsIsIdentity(t *testing.T) {
	cues := []Cue{{Start: 1_000, End: 2_000, Text: []string{"x"}}}
	rebased := RebaseAfterAds(cues, nil)
	if len(rebased) != 1 || rebased[0].Start != 1_000 || rebased[0].End != 2_000 {
		t.Fatalf("unexpected cues: %+v", rebased)
	}
}

func TestRebaseAfterAdsCueStartingBeforeAdIsKept(t *testing.T) {
	windows := []Window{{Start: 10_000, End: 20_000}}
	cues := []Cue{{Start: 8_000, End: 12_000, Text: []string{"overlap"}}}
	rebased := RebaseAfterAds(cues, windows)
	if len(rebased) != 1 || rebased[0].Start != 8_000 || rebased[0].End != 12_000 {
		t.Fatalf("unexpected cues: %+v", rebased)
	}
}

func TestShiftDelaysCuesAndClampsAtZero(t *testing.T) {
	cues := []Cue{
		{Start: 0, End: 1_000, Text: []string{"first"}},
		{Start: 5_000, End: 7_000, Text: []string{"second"}},
	}
	shifted := Shift(cues, 2_000)
	if len(shifted) != 2 {
		t.Fatalf("got %d cues, want 2", len(shifted))
	}
	if shifted[0].Start != 2_000 || shifted[0].End != 3_000 {
		t.Fatalf("unexpected first cue: %+v", shifted[0])
	}
	if shifted[1].Start != 7_000 || shifted[1].End != 9_000 {
		t.Fatalf("unexpected second cue: %+v", shifted[1])
	}
	// Original cues must not be mutated.
	if cues[1].Start != 5_000 || cues[1].End != 7_000 {
		t.Fatalf("input cues were mutated: %+v", cues[1])
	}
}

func TestShiftClampsNegativeTimes(t *testing.T) {
	cues := []Cue{{Start: 0, End: 1_000, Text: []string{"first"}}}
	shifted := Shift(cues, -500)
	if len(shifted) != 1 || shifted[0].Start != 0 || shifted[0].End != 500 {
		t.Fatalf("unexpected cues: %+v", shifted)
	}
}

func TestShiftZeroIsIdentity(t *testing.T) {
	cues := []Cue{{Start: 1_000, End: 2_000, Text: []string{"x"}}}
	if shifted := Shift(cues, 0); len(shifted) != 1 || shifted[0].Start != 1_000 {
		t.Fatalf("unexpected cues: %+v", shifted)
	}
}
