package marking

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"title-api/internal/marker"
	"title-api/internal/provider"

	_ "modernc.org/sqlite"
)

func newService(t *testing.T) *Service {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	service, err := New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func episodes(title string, numbers ...string) []provider.UnmarkedEpisode {
	var result []provider.UnmarkedEpisode
	for _, number := range numbers {
		result = append(result, provider.UnmarkedEpisode{Title: title, Season: 1, Episode: number, Master: "https://cdn/" + number})
	}
	return result
}

func queued(s *Service) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []string
	for _, e := range s.queue {
		result = append(result, e.Title+"/"+e.Episode)
	}
	return result
}

func enable(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	if err := s.store.setSetting(ctx, "key", "sk-ant-test-abcd"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.setSetting(ctx, "enabled", "1"); err != nil {
		t.Fatal(err)
	}
}

func TestUnmarkedQueuesOnlyTitlesFirstPlayedWhileOn(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	s.Opened(ctx, "a")
	s.Unmarked(episodes("a", "1"))
	if got := queued(s); len(got) != 0 {
		t.Fatalf("queued while off: %v", got)
	}
	enable(t, s)
	s.Unmarked(episodes("b", "1"))
	if got := queued(s); len(got) != 0 {
		t.Fatalf("queued a title not first played: %v", got)
	}
	s.Unmarked(episodes("a", "1", "2", "3"))
	s.Unmarked(episodes("a", "3", "4"))
	got := queued(s)
	want := []string{"a/3", "a/4", "a/1", "a/2"}
	if len(got) != len(want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("queue = %v, want %v", got, want)
		}
	}
}

func TestUnmarkedSkipsMarkedAndFailedUntilRetry(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	enable(t, s)
	s.Opened(ctx, "a")
	list := episodes("a", "1", "2", "3", "4")
	sections := []provider.Section{{Kind: provider.SectionIntro, Start: 1, End: 60, Skippable: true}}
	if err := s.store.done(ctx, list[0], sections, 3000, "m", 0.03, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.store.tried(ctx, list[1], stateNoSubtitles, errNoSubtitles, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.tried(ctx, list[2], stateFailed, errors.New("x"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.store.tried(ctx, list[3], stateFailed, errors.New("x"), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	s.Unmarked(list)
	if got := queued(s); len(got) != 1 || got[0] != "a/4" {
		t.Fatalf("queue = %v, want [a/4]", got)
	}
	got, duration := s.Sections("a", 1, "1")
	if len(got) != 1 || got[0] != sections[0] || duration != 3000 {
		t.Fatalf("sections = %v of %v s", got, duration)
	}
	if got, _ := s.Sections("a", 1, "2"); got != nil {
		t.Fatalf("sections of an episode not marked = %v", got)
	}
}

func TestSettingsHideTheKey(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	enable(t, s)
	settings, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || !settings.KeySet || settings.KeyHint != "abcd" {
		t.Fatalf("settings = %+v", settings)
	}
	off, empty := false, ""
	settings, err = s.Update(ctx, &off, &empty)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Enabled || settings.KeySet || settings.KeyHint != "" {
		t.Fatalf("settings = %+v", settings)
	}
}

func TestSectionsOf(t *testing.T) {
	result := marker.Result{
		Times: map[string]float64{
			"recap_end": 88.8, "intro_start": 88.8, "intro_end": 199.6,
			"credits_start": 3170.6, "credits_end": 3230,
		},
		Skippable: true,
	}
	got := sectionsOf(result, 3221.2)
	want := []provider.Section{
		{Kind: provider.SectionRecap, Start: 0, End: 88.8, Skippable: true},
		{Kind: provider.SectionIntro, Start: 88.8, End: 199.6, Skippable: true},
		{Kind: provider.SectionCredits, Start: 3170.6, End: 3221.2, Skippable: true},
	}
	if len(got) != len(want) {
		t.Fatalf("sections = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("sections = %v, want %v", got, want)
		}
	}
}

type fakeSource map[string]string

func (f fakeSource) Fetch(_ context.Context, rawURL string) ([]byte, error) {
	body, ok := f[rawURL]
	if !ok {
		return nil, errors.New("404")
	}
	return []byte(body), nil
}

func (f fakeSource) VideoSegments(context.Context, string) ([]provider.VideoSegment, error) {
	return nil, nil
}

func TestSubtitlesPrefersFullEnglish(t *testing.T) {
	full := "WEBVTT\n\n00:00:10.000 --> 00:00:12.000\nOne\n\n00:01:30.000 --> 00:01:32.000\nTwo\n\n00:03:00.000 --> 00:03:02.000\nThree\n"
	// The Russian track has a line more.
	source := fakeSource{"ru": full + "\n00:03:10.000 --> 00:03:12.000\nFour\n", "en-forced": full, "en": full}
	tracks := []provider.SubtitleTrack{
		{Language: "ru", PlaylistURL: "ru"},
		{Language: "en", Forced: true, PlaylistURL: "en-forced"},
		{Language: "uk", PlaylistURL: "missing"},
		{Language: "en", PlaylistURL: "en"},
	}
	cues, err := subtitles(context.Background(), source, tracks, 200)
	if err != nil || len(cues) != 3 {
		t.Fatalf("cues = %v, %v", cues, err)
	}
	if _, err := subtitles(context.Background(), source, tracks[1:3], 200); !errors.Is(err, errNoSubtitles) {
		t.Fatalf("err = %v, want no subtitles", err)
	}
}
