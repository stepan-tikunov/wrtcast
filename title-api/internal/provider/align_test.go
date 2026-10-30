package provider

import (
	"math"
	"math/rand/v2"
	"testing"
)

func near(got, want, tolerance float64) bool { return math.Abs(got-want) <= tolerance }

func TestAlignmentSections(t *testing.T) {
	ref := sectionsReference{
		duration: 3205,
		sections: []Section{
			{Kind: SectionRecap, Start: 0, End: 96.476, Skippable: true},
			{Kind: SectionIntro, Start: 96.476, End: 208.248, Skippable: true},
			{Kind: SectionCredits, Start: 3137.35, End: 3205.214, Skippable: true},
		},
	}
	// The alternative video begins 2.1 s later into the episode and has
	// a logo at its end.
	known := alignment{Scale: 1, Anchors: []anchor{{Primary: 210, Offset: -2.1}}}
	got := known.sections(ref, 3209.5)
	want := []Section{
		{Kind: SectionRecap, Start: 0, End: 94.376, Skippable: true},
		{Kind: SectionIntro, Start: 94.376, End: 206.148, Skippable: true},
		{Kind: SectionCredits, Start: 3135.25, End: 3209.5, Skippable: true},
	}
	if len(got) != len(want) {
		t.Fatalf("sections = %+v", got)
	}
	for index := range want {
		if got[index].Kind != want[index].Kind || !near(got[index].Start, want[index].Start, 1e-6) ||
			!near(got[index].End, want[index].End, 1e-6) {
			t.Errorf("section %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}

func TestAlignmentInterpolatesBetweenAnchors(t *testing.T) {
	known := alignment{Scale: 1, Anchors: []anchor{{Primary: 100, Offset: 1}, {Primary: 1100, Offset: 3}}}
	for _, test := range []struct{ primary, want float64 }{
		{50, 51}, {100, 101}, {600, 602}, {1100, 1103}, {3000, 3003},
	} {
		if got := known.at(test.primary); !near(got, test.want, 1e-9) {
			t.Errorf("at(%v) = %v, want %v", test.primary, got, test.want)
		}
	}
}

func TestLengthScale(t *testing.T) {
	for _, test := range []struct{ alternative, primary, want float64 }{
		{3209.5, 3205, 1},
		{3205 * 24 / 25, 3205, 24.0 / 25},
		{3205 * 25 / 24, 3205, 25.0 / 24},
		{3205 * 1.1, 3205, 0},
		{3205, 0, 0},
	} {
		if got := lengthScale(test.alternative, test.primary); got != test.want {
			t.Errorf("lengthScale(%v, %v) = %v, want %v", test.alternative, test.primary, got, test.want)
		}
	}
}

func TestFragmentRun(t *testing.T) {
	kept := []tappedFragment{
		{start: 0, end: 6, init: "a"},
		{start: 12, end: 18, init: "a"},
		{start: 18, end: 24, init: "a"},
		{start: 24, end: 30, init: "b"},
	}
	run := fragmentRun(kept, kept[1])
	if len(run) != 2 || run[0].start != 12 || run[1].end != 24 {
		t.Fatalf("run = %+v", run)
	}
}

func TestNextEpisode(t *testing.T) {
	seasons := []seriesSeason{
		{Season: "1", Episodes: []seriesEpisode{{Episode: "2"}, {Episode: "10"}, {Episode: "1"}}},
		{Season: "2", Episodes: []seriesEpisode{{Episode: "1"}}},
	}
	for _, test := range []struct {
		season      int32
		episode     string
		wantSeason  int32
		wantEpisode string
		ok          bool
	}{
		{1, "1", 1, "2", true},
		{1, "2", 1, "10", true},
		{1, "10", 2, "1", true},
		{2, "1", 0, "", false},
	} {
		season, episode, ok := nextEpisode(seasons, test.season, test.episode)
		if ok != test.ok || season != test.wantSeason || string(episode.Episode) != test.wantEpisode {
			t.Errorf("nextEpisode(%d, %s) = %d, %s, %v", test.season, test.episode, season, episode.Episode, ok)
		}
	}
}

func TestLightestVariantAndSegments(t *testing.T) {
	master := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1080592,CODECS=\"avc1.64001f,mp4a.40.2\",RESOLUTION=1280x720\n" +
		"index-v1.m3u8?a=1\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=900000,CODECS=\"hvc1.1.6.L93,mp4a.40.2\",RESOLUTION=640x360\n" +
		"index-h.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=384799,CODECS=\"avc1.64001e,mp4a.40.2\",RESOLUTION=640x360\n" +
		"index-v2.m3u8?a=2\n"
	variant, err := lightestVariant("https://cdn.example/video.mp4/master.m3u8?x=1", master)
	if err != nil || variant != "https://cdn.example/video.mp4/index-v2.m3u8?a=2" {
		t.Fatalf("lightestVariant = %q, %v", variant, err)
	}
	playlist := "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.0,\nseg-1.m4s\n#EXTINF:4.5,\nseg-2.m4s\n#EXT-X-ENDLIST\n"
	segments, err := parseMediaPlaylist("https://cdn.example/v/index.m3u8", playlist)
	if err != nil || len(segments) != 2 {
		t.Fatalf("parseMediaPlaylist = %+v, %v", segments, err)
	}
	if segments[1].url != "https://cdn.example/v/seg-2.m4s" || segments[1].start != 6 || segments[1].init != "https://cdn.example/v/init.mp4" {
		t.Errorf("segment = %+v", segments[1])
	}
	if _, err := parseMediaPlaylist("https://cdn.example/v/index.m3u8", "#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:6,\na.ts\n"); err == nil {
		t.Error("an encrypted playlist parsed")
	}
}

// randomFrames are frames of a video whose every frame looks different,
// from start, rate a second.
func randomFrames(random *rand.Rand, count int, start, rate float64) frames {
	result := frames{}
	for index := range count {
		gray := make([]byte, frameWidth*frameHeight)
		for pixel := range gray {
			gray[pixel] = byte(random.IntN(256))
		}
		result.print = append(result.print, fingerprint(gray))
		result.times = append(result.times, start+float64(index)/rate)
	}
	return result
}

func TestMatchOffsetFindsShift(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	primary := randomFrames(random, 24*40, 180, 24)
	// The alternative's frames from 200 s of the primary video, 2.1 s
	// earlier on its own timeline and at 23.976 frames a second, rounded
	// to them.
	var alt frames
	for index := range 24 * 10 {
		at := 200 + float64(index)/24
		source := int(math.Round((at - 180) * 24))
		alt.print = append(alt.print, primary.print[source])
		alt.times = append(alt.times, at-2.1)
	}
	result, err := matchOffset(alt, primary, 1, -20, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !result.confident() || !near(result.offset, -2.1, 0.03) {
		t.Fatalf("result = %+v", result)
	}
}

func TestMatchOffsetDistrustsStillScenes(t *testing.T) {
	random := rand.New(rand.NewPCG(3, 4))
	still := randomFrames(random, 1, 0, 24).print[0]
	var primary, alt frames
	for index := range 24 * 40 {
		primary.print = append(primary.print, still)
		primary.times = append(primary.times, float64(index)/24)
	}
	for index := range 24 * 10 {
		alt.print = append(alt.print, still)
		alt.times = append(alt.times, 15+float64(index)/24)
	}
	result, err := matchOffset(alt, primary, 1, -5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if result.confident() {
		t.Fatalf("a still scene matched: %+v", result)
	}
}

func TestFingerprintFlat(t *testing.T) {
	gray := make([]byte, frameWidth*frameHeight)
	if fingerprint(gray) != nil {
		t.Error("a black frame has a fingerprint")
	}
}
