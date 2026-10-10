package provider

import "testing"

func TestParseMaster(t *testing.T) {
	t.Parallel()
	const masterURL = "https://cdn.example/movie/master.m3u8?token=one"
	const playlist = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",NAME="rus0",DEFAULT=YES,LANGUAGE="ru",URI="audio/a1.m3u8?token=two"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="failover-audio-0",NAME="rus0",DEFAULT=YES,LANGUAGE="ru",URI="backup/audio/a1.m3u8?token=two"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",FORCED=NO,URI="subs/en.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=631938,CODECS="avc1.64001f,mp4a.40.2",RESOLUTION=1272x530,AUDIO="audio0"
video/v1.m3u8?token=three
`
	stream, err := parseMaster(masterURL, playlist, []string{"Рус. Дублированный"}, []SubtitleTrack{{
		ID: "1", Name: "Рус. полные", Language: "ru", PlaylistURL: "https://cdn.example/subs/ru.vtt",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if stream.MasterPlaylistURL != masterURL {
		t.Fatalf("master URL = %q", stream.MasterPlaylistURL)
	}
	if len(stream.AudioTracks) != 1 || stream.AudioTracks[0].ID != "rus0" ||
		stream.AudioTracks[0].Name != "Рус. Дублированный" || !stream.AudioTracks[0].Default {
		t.Fatalf("unexpected audio tracks: %+v", stream.AudioTracks)
	}
	if len(stream.Subtitles) != 2 || stream.Subtitles[1].Language != "en" {
		t.Fatalf("unexpected subtitles: %+v", stream.Subtitles)
	}
}
