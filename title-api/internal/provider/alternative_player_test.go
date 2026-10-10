package provider

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealAlternativePlayer(t *testing.T) {
	if os.Getenv("MOVIE_PROVIDER_INTEGRATION") == "" {
		t.Skip("set MOVIE_PROVIDER_INTEGRATION=1 to call the live service")
	}
	client, err := NewClient(Config{BaseURL: os.Getenv("MOVIE_PROVIDER_BASE_URL"), RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, page, _, err := client.movieDocument(ctx, client.ExternalTitleID("43222"))
	if err != nil {
		t.Fatal(err)
	}
	urls := alternativePlayerURLs(page, client.baseURL.Scheme)
	if len(urls) == 0 {
		t.Fatalf("alternative player not found; token_movie offset=%d, cdn offset=%d", strings.Index(string(page), "token_movie"), strings.Index(string(page), "cdn."))
	}
	streams, err := client.alternativeStreams(ctx, urls[0], 1, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) == 0 {
		t.Fatal("alternative player returned no streams")
	}
}

func TestAlternativePlayerBootstrapAndSelection(t *testing.T) {
	t.Parallel()
	page := []byte(`<iframe data-src="https:\/\/cdn.example\/?token_movie=abc123&amp;token=def456"></iframe>`)
	urls := alternativePlayerURLs(page, "https")
	if len(urls) != 1 || urls[0] != "https://cdn.example/?token_movie=abc123&token=def456" {
		t.Fatalf("unexpected player URLs: %#v", urls)
	}

	catalog := alternativeCatalog{Type: "serial", All: map[string]map[string]map[string]alternativeFile{
		"1": {"2": {"t66": {ID: 985376, Translation: "Dub"}}},
	}}
	file, err := catalog.selectedFile(1, "2")
	if err != nil || file.ID != 985376 {
		t.Fatalf("selectedFile() = %+v, %v", file, err)
	}
}

func TestPlayerChallenge(t *testing.T) {
	t.Parallel()
	input := "1noQZdRf5yQxlcFB4oU2hIiRzHz2l2nDVVEc9VYx5IFVo81z81Bz41nyZOMTeVVEkTTT33EUNRNaMQVNMOMNMMdW"
	want := "cVRfWV9zdEo2MkV8MTc5MTY1OTQ4M3xBN3UyNE5zRUZiNzI4aHFQMzF1Q2doVlVnN2IxZnoyODRhMV81TVBleE1n"
	if got := playerChallenge(input); got != want {
		t.Fatalf("playerChallenge() = %q, want %q", got, want)
	}
}

func TestAlternativePlaybackStreams(t *testing.T) {
	t.Parallel()
	var playback alternativePlayback
	playback.HLSSource = append(playback.HLSSource, struct {
		Label   string            `json:"label"`
		Quality map[string]string `json:"quality"`
	}{Label: "(Russian) Dub", Quality: map[string]string{
		"720":  "https://cdn.example/720/master.m3u8 or https://failover.example/720/master.m3u8",
		"1080": "https://cdn.example/1080/master.m3u8",
	}})
	playback.Tracks = append(playback.Tracks, struct {
		Kind     string `json:"kind"`
		Label    string `json:"label"`
		Source   string `json:"src"`
		Language string `json:"language"`
		Default  bool   `json:"default"`
	}{Kind: "captions", Label: "Russian", Source: "https://cdn.example/sub.vtt", Language: "rus", Default: true})

	streams := playback.streams()
	if len(streams) != 2 || !strings.Contains(streams[0].AudioTracks[0].Name, "1080p") ||
		streams[0].AudioTracks[0].Language != "ru" || len(streams[0].Subtitles) != 1 {
		t.Fatalf("unexpected streams: %+v", streams)
	}
}
