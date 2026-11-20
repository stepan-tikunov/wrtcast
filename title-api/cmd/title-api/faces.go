package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"

	"title-api/internal/faces"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
)

// A pause's segment taken at most.
const facesSegmentLimit = 32 << 20

var (
	titleIDPattern = regexp.MustCompile(`^[0-9A-Za-z._:-]{1,200}$`)
	numberPattern  = regexp.MustCompile(`^[0-9]{1,4}$`)
	picturePattern = regexp.MustCompile(`^([0-9]{1,15})\.jpg$`)
	scopePattern   = regexp.MustCompile(`^(movie|tv):[0-9]{1,10}$`)
)

func facesReply(response http.ResponseWriter, body any, err error) {
	switch {
	case errors.Is(err, faces.ErrNotMatched):
		http.Error(response, err.Error(), http.StatusConflict)
	case errors.Is(err, faces.ErrNoFrame):
		http.Error(response, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, faces.ErrNoPause):
		http.Error(response, err.Error(), http.StatusNotFound)
	case err != nil:
		log.Printf("faces: %v", err)
		http.Error(response, err.Error(), http.StatusBadGateway)
	default:
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(body)
	}
}

// episodeQuery is the title, season and episode of a faces request; false
// if they are not valid, which is answered.
func episodeQuery(response http.ResponseWriter, request *http.Request) (string, int32, int32, bool) {
	query := request.URL.Query()
	title := query.Get("title_id")
	if !titleIDPattern.MatchString(title) {
		http.Error(response, "invalid title_id", http.StatusBadRequest)
		return "", 0, 0, false
	}
	var numbers [2]int32
	for i, name := range []string{"season", "episode"} {
		value := query.Get(name)
		if value == "" {
			continue
		}
		if !numberPattern.MatchString(value) {
			http.Error(response, "invalid "+name, http.StatusBadRequest)
			return "", 0, 0, false
		}
		n, _ := strconv.Atoi(value)
		numbers[i] = int32(n)
	}
	return title, numbers[0], numbers[1], true
}

func facesDisabled(response http.ResponseWriter, service *faces.Service) bool {
	if service == nil {
		http.Error(response, "faces are not set up (FACES_URL)", http.StatusServiceUnavailable)
		return true
	}
	return false
}

// facesHandler serves POST
// /v1/faces?title_id=<id>&season=<n>&episode=<n>&t=<seconds>&offset=<seconds>,
// the body the segment the TV is paused in as it was served (an fMP4
// one's init segment first), offset the seconds into it of the pause, t
// where that is on the video: the paused frame, as soon as it is cut out,
// its faces being found (see faces.Result), which
// GET /v1/faces/pauses/{id} answers.
func facesHandler(service *faces.Service) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, _ map[string]string) {
		if facesDisabled(response, service) {
			return
		}
		title, season, episode, ok := episodeQuery(response, request)
		if !ok {
			return
		}
		query := request.URL.Query()
		var at [2]float64
		for i, name := range []string{"t", "offset"} {
			value := query.Get(name)
			if !positionPattern.MatchString(value) {
				http.Error(response, "invalid "+name, http.StatusBadRequest)
				return
			}
			at[i], _ = strconv.ParseFloat(value, 64)
		}
		if at[1] > 60 {
			http.Error(response, "offset past the segment", http.StatusBadRequest)
			return
		}
		segment, err := io.ReadAll(io.LimitReader(request.Body, facesSegmentLimit+1))
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		if len(segment) == 0 || len(segment) > facesSegmentLimit {
			http.Error(response, "a segment of up to 32 MB is wanted", http.StatusBadRequest)
			return
		}
		result, err := service.Recognize(request.Context(), faces.Request{
			Title: title, Season: season, Episode: episode, Time: at[0], Offset: at[1], Segment: segment,
		})
		facesReply(response, result, err)
	}
}

// facesAgainHandler serves GET /v1/faces/pauses/{id}: who is in a pause
// kept by POST /v1/faces, told by the references made by now; while its
// faces are being found, it waits for them a while.
func facesAgainHandler(service *faces.Service) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, pathParams map[string]string) {
		if facesDisabled(response, service) {
			return
		}
		id, err := strconv.ParseInt(pathParams["id"], 10, 64)
		if err != nil || id <= 0 {
			http.Error(response, "invalid id", http.StatusBadRequest)
			return
		}
		result, err := service.Again(request.Context(), id)
		facesReply(response, result, err)
	}
}

// facesPrepareHandler serves POST
// /v1/faces/prepare?title_id=<id>&season=<n>&episode=<n>: the references
// of the people of an episode played are made in the background, so that
// its pauses find them ready; whether some are to be made.
func facesPrepareHandler(service *faces.Service) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, _ map[string]string) {
		if facesDisabled(response, service) {
			return
		}
		title, season, episode, ok := episodeQuery(response, request)
		if !ok {
			return
		}
		preparing, err := service.Prepare(request.Context(), title, season, episode)
		facesReply(response, struct {
			Preparing bool `json:"preparing"`
		}{preparing}, err)
	}
}

func servePicture(response http.ResponseWriter, data []byte, err error, cache string) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		http.Error(response, "no such picture", http.StatusNotFound)
	case err != nil:
		http.Error(response, err.Error(), http.StatusInternalServerError)
	default:
		response.Header().Set("Content-Type", "image/jpeg")
		response.Header().Set("Cache-Control", cache)
		_, _ = response.Write(data)
	}
}

// facesPictureHandler serves GET /v1/faces/frames/{file} and
// /v1/faces/crops/{file}: a pause's frame or a face of it, which never
// change.
func facesPictureHandler(service *faces.Service, kind string) runtime.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request, pathParams map[string]string) {
		if facesDisabled(response, service) {
			return
		}
		match := picturePattern.FindStringSubmatch(pathParams["file"])
		if match == nil {
			http.Error(response, "invalid file", http.StatusBadRequest)
			return
		}
		id, _ := strconv.ParseInt(match[1], 10, 64)
		data, err := service.Picture(kind, id)
		servePicture(response, data, err, "public, max-age=31536000, immutable")
	}
}

// facesPortraitHandler serves GET /v1/faces/portraits/{scope}/{person},
// scope a title on TMDB (tv:1405, movie:550) and person someone's TMDB ID:
// the best picture of them seen in its pauses that was surely them, which
// a better one may replace.
func facesPortraitHandler(service *faces.Service) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, pathParams map[string]string) {
		if facesDisabled(response, service) {
			return
		}
		person, err := strconv.ParseUint(pathParams["person"], 10, 64)
		if !scopePattern.MatchString(pathParams["scope"]) || err != nil || person == 0 {
			http.Error(response, "invalid scope or person", http.StatusBadRequest)
			return
		}
		data, err := service.Portrait(request.Context(), pathParams["scope"], person)
		servePicture(response, data, err, "public, max-age=3600")
	}
}
