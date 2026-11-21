package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The audio rendition of the dub the TV plays of a primary video is loaded
// ahead of where it plays, each segment once and told; what the sink has
// is not loaded.
func TestFollowAudio(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	asked := map[string]int{}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/master.m3u8":
			_, _ = w.Write([]byte("#EXTM3U\n" +
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="1",LANGUAGE="ru",URI="a1.m3u8"` + "\n" +
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="2",LANGUAGE="en",URI="a2.m3u8"` + "\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=1000,AUDIO=\"a\"\nv.m3u8\n"))
		case "/a2.m3u8":
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:6,\na2-0.ts\n#EXTINF:6,\na2-1.ts\n#EXTINF:6,\na2-2.ts\n#EXT-X-ENDLIST\n"))
		default:
			if strings.HasPrefix(r.URL.Path, "/a2-") {
				_, _ = w.Write([]byte("audio " + r.URL.Path))
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cdn.Close)
	client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	client.session = &session{client: cdn.Client(), cookies: map[string]map[string]string{}, cookieOrder: map[string][]string{}}
	var told []Segment
	if err := client.KeepSegments(SegmentOptions{Dir: t.TempDir(), Limit: 1 << 20, Ahead: 10,
		Sink: func(s Segment) {
			mu.Lock()
			told = append(told, s)
			mu.Unlock()
		},
		// The first is had already.
		Wanted: func(v Video, start float64) bool { return start != 0 },
	}); err != nil {
		t.Fatal(err)
	}
	video := Video{Title: "t", Season: 1, Episode: "2", Source: "Основной плеер | English"}
	if err := client.FollowAudio(AudioPlay{Video: video, Master: cdn.URL + "/master.m3u8", Audio: "2", Position: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the audio ahead", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(told) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if s := told[0]; s.Video != video || s.Start != 6 || s.End != 12 || string(s.Data) != "audio /a2-1.ts" {
		t.Fatalf("told %+v", s)
	}
	if asked["/a2-0.ts"] != 0 || asked["/a2-2.ts"] != 0 || asked["/a1.m3u8"] != 0 {
		t.Fatalf("asked %v", asked)
	}
}
