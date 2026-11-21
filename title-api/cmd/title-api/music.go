package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"

	"title-api/internal/music"
	"title-api/internal/provider"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
)

// A piece of audio taken at most: a segment's, some 150 KB for 4 s.
const musicAudioLimit = 4 << 20

var sourcePattern = regexp.MustCompile(`^[0-9A-Za-z._-]{1,40}$`)

func musicDisabled(response http.ResponseWriter, service *music.Service) bool {
	if service == nil {
		http.Error(response, "music is not set up (MUSIC_URL)", http.StatusServiceUnavailable)
		return true
	}
	return false
}

// musicVideo is the video of a music request: the title, the episode and
// the source; false if they are not valid, which is answered.
func musicVideo(response http.ResponseWriter, request *http.Request) (music.Video, bool) {
	title, season, episode, ok := episodeQuery(response, request)
	if !ok {
		return music.Video{}, false
	}
	source := request.URL.Query().Get("source")
	if !sourcePattern.MatchString(source) {
		http.Error(response, "invalid source", http.StatusBadRequest)
		return music.Video{}, false
	}
	return music.Video{Title: title, Season: season, Episode: episode, Source: source}, true
}

func musicReply(response http.ResponseWriter, body any, err error) {
	if err != nil {
		log.Printf("music: %v", err)
		http.Error(response, err.Error(), http.StatusBadGateway)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(response).Encode(body)
}

// musicAudioHandler serves POST
// /v1/music/audio?title_id=<id>&season=<n>&episode=<n>&source=<player>&start=<s>&stop=<s>,
// the body the audio of a segment of the video from start to stop seconds
// of it (ADTS AAC, as the router cuts it out): where its music is is told
// at once, what it is in the background (GET /v1/music).
func musicAudioHandler(service *music.Service) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, _ map[string]string) {
		if musicDisabled(response, service) {
			return
		}
		video, ok := musicVideo(response, request)
		if !ok {
			return
		}
		query := request.URL.Query()
		var at [2]float64
		for i, name := range []string{"start", "stop"} {
			value := query.Get(name)
			if !positionPattern.MatchString(value) {
				http.Error(response, "invalid "+name, http.StatusBadRequest)
				return
			}
			at[i], _ = strconv.ParseFloat(value, 64)
		}
		if at[1] <= at[0] || at[1]-at[0] > 60 {
			http.Error(response, "a piece of up to 60 s is wanted", http.StatusBadRequest)
			return
		}
		audio, err := io.ReadAll(io.LimitReader(request.Body, musicAudioLimit+1))
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		if len(audio) == 0 || len(audio) > musicAudioLimit {
			http.Error(response, "audio of up to 4 MB is wanted", http.StatusBadRequest)
			return
		}
		err = service.Audio(request.Context(), video, at[0], at[1], audio)
		musicReply(response, struct {
			OK bool `json:"ok"`
		}{true}, err)
	}
}

// musicHandler serves GET
// /v1/music?title_id=<id>&season=<n>&episode=<n>&source=<player>: the
// tracks heard in the video by now (see music.Result).
func musicHandler(service *music.Service) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, _ map[string]string) {
		if musicDisabled(response, service) {
			return
		}
		video, ok := musicVideo(response, request)
		if !ok {
			return
		}
		result, err := service.Tracks(request.Context(), video)
		musicReply(response, result, err)
	}
}

// musicMissesHandler serves DELETE
// /v1/music/misses[?title_id=<id>&season=<n>&episode=<n>&source=<player>]:
// what Shazam found nothing in, of the video or of all, is asked again
// (see music.Service.ForgetMisses).
func musicMissesHandler(service *music.Service) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, _ map[string]string) {
		if musicDisabled(response, service) {
			return
		}
		var video *music.Video
		if request.URL.Query().Has("title_id") {
			v, ok := musicVideo(response, request)
			if !ok {
				return
			}
			video = &v
		}
		n, err := service.ForgetMisses(request.Context(), video)
		musicReply(response, struct {
			Forgotten int64 `json:"forgotten"`
		}{n}, err)
	}
}

// musicVideoOf is the music's video of a provider's: the episode by its
// number, the source by its name hashed (as the router hashes it).
func musicVideoOf(v provider.Video) music.Video {
	return music.Video{Title: v.Title, Season: v.Season, Episode: music.EpisodeOf(v.Episode), Source: music.SourceOf(v.Source)}
}

// musicWanted tells whether the audio of a video from start is still to be
// taken (see provider.SegmentOptions.Wanted).
func musicWanted(ctx context.Context, service *music.Service) func(provider.Video, float64) bool {
	return func(v provider.Video, start float64) bool { return !service.Has(ctx, musicVideoOf(v), start) }
}

type audioFollower interface {
	FollowAudio(play provider.AudioPlay) error
}

// musicPlayHandler serves POST
// /v1/music/play?title_id=<id>&season=<n>&episode=<n>&source=<hash>, the
// body {"name", "master", "audio", "position", "playing"}: the TV plays a
// primary player's video (master) with the audio rendition named audio, at
// position seconds, playing or not; its audio is loaded ahead of there
// for the music (see provider.FollowAudio). name is the source's, which
// source is the hash of: the stream's and the dub's.
func musicPlayHandler(service *music.Service, follower audioFollower) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, _ map[string]string) {
		if musicDisabled(response, service) {
			return
		}
		video, ok := musicVideo(response, request)
		if !ok {
			return
		}
		var body struct {
			Name     string  `json:"name"`
			Master   string  `json:"master"`
			Audio    string  `json:"audio"`
			Position float64 `json:"position"`
			Playing  bool    `json:"playing"`
		}
		if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(response, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if music.SourceOf(body.Name) != video.Source || body.Audio == "" || body.Position < 0 {
			http.Error(response, "the name is not the source's, or no audio", http.StatusBadRequest)
			return
		}
		err := follower.FollowAudio(provider.AudioPlay{
			Video:  provider.Video{Title: video.Title, Season: video.Season, Episode: strconv.Itoa(int(video.Episode)), Source: body.Name},
			Master: body.Master, Audio: body.Audio, Position: body.Position, Playing: body.Playing,
		})
		if err != nil {
			http.Error(response, err.Error(), http.StatusServiceUnavailable)
			return
		}
		musicReply(response, struct {
			OK bool `json:"ok"`
		}{true}, nil)
	}
}

// musicSink takes the segments the relay passes on or loads ahead (see
// provider.KeepSegments) for the music service, one at a time; while it
// is behind, segments beyond the queue are left.
func musicSink(ctx context.Context, service *music.Service) func(provider.Segment) {
	queue := make(chan provider.Segment, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-queue:
				v := musicVideoOf(s.Video)
				if err := service.Segment(ctx, v, s.Start, s.End, s.Init, s.Data); err != nil && ctx.Err() == nil {
					log.Printf("music %s|%d|%d at %.1f: %v", v.Title, v.Season, v.Episode, s.Start, err)
				}
			}
		}
	}()
	return func(s provider.Segment) {
		select {
		case queue <- s:
		default:
			log.Printf("music: behind, a segment at %.1f left", s.Start)
		}
	}
}
