package faces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"title-api/internal/tmdb"
)

// Of a title on TMDB, the people its pauses are told by: the cast as
// billed, of a series all its seasons', and an episode's guest stars.
const (
	castLimit  = 20
	guestLimit = 10
)

// A paused frame's faces told, the largest, and the smallest of them.
var pauseLook = look{size: 640, minSide: 40, most: 5}

var (
	// ErrNotMatched is a title not matched with TMDB, whose cast is not
	// known.
	ErrNotMatched = errors.New("the title is not matched with TMDB")
	// ErrNoPause is a pause not kept.
	ErrNoPause = errNoPause
)

// TMDB is what of TMDB tells the people of a title and their photos.
type TMDB interface {
	Match(ctx context.Context, title string) (tmdb.Match, bool)
	Cast(ctx context.Context, kind string, id uint64) ([]tmdb.Person, error)
	Guests(ctx context.Context, id uint64, season, episode int32) ([]tmdb.Person, error)
	Photos(ctx context.Context, id uint64) (tmdb.Photos, error)
}

// Images are TMDB's pictures, as /v1/tmdb/images serves them.
type Images interface {
	Image(ctx context.Context, size, name string) ([]byte, string, error)
}

// Service tells who is in the paused frames and makes the references of
// the people of the titles paused in the background, one at a time.
type Service struct {
	store    *store
	detector *detector
	tmdb     TMDB
	images   Images
	pictures *pictures

	mu sync.Mutex
	// The people to make references for, the latest asked for first, and
	// the one they are being made for.
	queue   []uint64
	current uint64
	wake    chan struct{}
	// Pauses waiting for the faces service, which the references wait for.
	urgent atomic.Int32
}

// New is a Service asking the faces service at url, keeping the pictures
// of the pauses in dir.
func New(ctx context.Context, db *sql.DB, url, dir string, tmdb TMDB, images Images) (*Service, error) {
	store, err := openStore(ctx, db)
	if err != nil {
		return nil, err
	}
	pictures, err := openPictures(dir)
	if err != nil {
		return nil, err
	}
	return &Service{
		store: store, detector: newDetector(url), tmdb: tmdb, images: images, pictures: pictures,
		wake: make(chan struct{}, 1),
	}, nil
}

// Request is a pause: the title, the episode (0 and 0 for a movie), where
// on the video (t, for the record) and offset seconds into the segment
// there, as it was served.
type Request struct {
	Title           string
	Season, Episode int32
	Time, Offset    float64
	Segment         []byte
}

// Result is who is in a paused frame. Pictures are paths of title-api.
type Result struct {
	Pause  int64  `json:"pause"`
	Frame  string `json:"frame"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Faces  []Face `json:"faces"`
	// References of some of the people are yet to be made; asked again
	// later (Again), the faces may be told better.
	Preparing bool `json:"preparing"`
}

// Face is a face of the frame, its box in the frame's pixels, a picture
// of it, and whom it was taken for: someone surely or maybe, or the two
// most alike if not surely, or no one.
type Face struct {
	Box          [4]float64    `json:"box"`
	Crop         string        `json:"crop"`
	Person       *Person       `json:"person,omitempty"`
	Score        float64       `json:"score,omitempty"`
	Sure         bool          `json:"sure"`
	Alternatives []Alternative `json:"alternatives,omitempty"`
}

// Person is someone of the title: their portrait of TMDB (a file of
// /v1/tmdb/images) and the best picture of them seen in its pauses, if
// one was surely them.
type Person struct {
	ID        uint64 `json:"id"`
	Name      string `json:"name"`
	Character string `json:"character,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Portrait  string `json:"portrait,omitempty"`
}

type Alternative struct {
	Person Person  `json:"person"`
	Score  float64 `json:"score"`
}

func scopeOf(m tmdb.Match) string { return m.Kind + ":" + strconv.FormatUint(m.ID, 10) }

// people are the people of an episode of a title on TMDB, as billed, the
// guest stars last.
func (s *Service) people(ctx context.Context, m tmdb.Match, season, episode int32) ([]tmdb.Person, error) {
	cast, err := s.tmdb.Cast(ctx, m.Kind, m.ID)
	if err != nil {
		return nil, err
	}
	var result []tmdb.Person
	seen := map[uint64]bool{}
	add := func(list []tmdb.Person, limit int) {
		n := 0
		for _, p := range list {
			if p.ID != 0 && !seen[p.ID] && n < limit {
				seen[p.ID] = true
				result = append(result, p)
				n++
			}
		}
	}
	add(cast, castLimit)
	if m.Kind == tmdb.KindTV && season > 0 && episode > 0 {
		guests, err := s.tmdb.Guests(ctx, m.ID, season, episode)
		if err != nil {
			log.Printf("faces %s s%de%d: %v", scopeOf(m), season, episode, err)
		}
		add(guests, guestLimit)
	}
	return result, nil
}

func ids(people []tmdb.Person) []uint64 {
	result := make([]uint64, len(people))
	for i, p := range people {
		result[i] = p.ID
	}
	return result
}

// Prepare makes the references of the people of an episode in the
// background, if they aren't yet: whether some are to be made.
func (s *Service) Prepare(ctx context.Context, title string, season, episode int32) (bool, error) {
	m, ok := s.tmdb.Match(ctx, title)
	if !ok {
		return false, ErrNotMatched
	}
	people, err := s.people(ctx, m, season, episode)
	if err != nil {
		return false, err
	}
	return s.enqueue(ctx, ids(people))
}

// enqueue puts the people with no references first in the queue, in
// their order: whether there are any.
func (s *Service) enqueue(ctx context.Context, people []uint64) (bool, error) {
	done, err := s.store.prepared(ctx, preparedKey, people)
	if err != nil {
		return false, err
	}
	var missing []uint64
	s.mu.Lock()
	for _, p := range people {
		if _, ok := done[p]; !ok && p != s.current {
			missing = append(missing, p)
		}
	}
	preparing := len(missing) > 0 || s.current != 0 && contains(people, s.current)
	if len(missing) == 0 {
		s.mu.Unlock()
		return preparing, nil
	}
	queue := append([]uint64(nil), missing...)
	for _, p := range s.queue {
		if !contains(missing, p) {
			queue = append(queue, p)
		}
	}
	s.queue = queue
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return preparing, nil
}

func contains(list []uint64, x uint64) bool {
	for _, y := range list {
		if y == x {
			return true
		}
	}
	return false
}

// Run makes the references of the people queued, one at a time, and trims
// the pictures kept every hour, until ctx is done.
func (s *Service) Run(ctx context.Context) {
	trim := time.NewTicker(time.Hour)
	defer trim.Stop()
	s.trim(ctx)
	for {
		s.mu.Lock()
		var person uint64
		if len(s.queue) > 0 {
			person, s.queue = s.queue[0], s.queue[1:]
		}
		s.current = person
		s.mu.Unlock()
		if person != 0 {
			s.prepare(ctx, person)
			s.mu.Lock()
			s.current = 0
			s.mu.Unlock()
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-trim.C:
			s.trim(ctx)
		}
	}
}

func (s *Service) trim(ctx context.Context) {
	kept, err := s.store.kept(ctx)
	if err != nil {
		log.Printf("faces pictures: %v", err)
		return
	}
	if err := s.pictures.trim(kept); err != nil {
		log.Printf("faces pictures: %v", err)
	}
}

// detect asks the faces service, references after pauses.
func (s *Service) detect(ctx context.Context, picture []byte, l look, urgent bool) (detection, error) {
	if urgent {
		s.urgent.Add(1)
		defer s.urgent.Add(-1)
	} else {
		for s.urgent.Load() > 0 {
			select {
			case <-ctx.Done():
				return detection{}, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	result, err := s.detector.detect(ctx, picture, l)
	if err == nil && result.Model != model {
		err = fmt.Errorf("faces service: vectors of %s, not of %s", result.Model, model)
	}
	return result, err
}

// Recognize tells who is in the paused frame, keeping it, its faces and
// what they were taken for.
func (s *Service) Recognize(ctx context.Context, r Request) (Result, error) {
	started := time.Now()
	m, ok := s.tmdb.Match(ctx, r.Title)
	if !ok {
		return Result{}, ErrNotMatched
	}
	people, err := s.people(ctx, m, r.Season, r.Episode)
	if err != nil {
		return Result{}, err
	}
	preparing, err := s.enqueue(ctx, ids(people))
	if err != nil {
		return Result{}, err
	}
	jpeg, err := pausedFrame(ctx, r.Segment, r.Offset)
	if err != nil {
		log.Printf("faces %s s%de%d at %.1f: %v", r.Title, r.Season, r.Episode, r.Time, err)
		return Result{}, err
	}
	decoded := time.Now()
	found, err := s.detect(ctx, jpeg, pauseLook, true)
	if err != nil {
		return Result{}, err
	}
	detected := time.Now()
	picture, err := decodeJPEG(jpeg)
	if err != nil {
		return Result{}, fmt.Errorf("paused frame: %w", err)
	}
	frame, scale := scaled(picture)
	var sightings []sighting
	// The faces' boxes in the segment's pixels, the crops cut by.
	var boxes [][4]float64
	for _, f := range found.Faces {
		if f.Embedding == nil {
			continue
		}
		boxes = append(boxes, f.Box)
		box := [4]float64{}
		for i, v := range f.Box {
			box[i] = math.Round(v*scale*10) / 10
		}
		sightings = append(sightings, sighting{
			box: box, vector: f.Embedding, quality: f.side(), det: f.Det, yaw: f.Yaw, blur: f.Blur,
		})
	}
	scope := scopeOf(m)
	if err := s.guess(ctx, scope, people, sightings); err != nil {
		return Result{}, err
	}
	p := pause{
		title: r.Title, scope: scope, season: r.Season, episode: r.Episode, t: r.Time,
		width: frame.Bounds().Dx(), height: frame.Bounds().Dy(), model: model, at: time.Now(),
	}
	id, sightingIDs, err := s.store.addPause(ctx, p, sightings)
	if err != nil {
		return Result{}, err
	}
	if err := s.pictures.put(framePath, id, encodeJPEG(frame, 82)); err != nil {
		return Result{}, err
	}
	for i, box := range boxes {
		sightings[i].id = sightingIDs[i]
		if err := s.pictures.put(cropPath, sightingIDs[i], encodeJPEG(crop(picture, box), 88)); err != nil {
			return Result{}, err
		}
	}
	p.id = id
	result, err := s.result(ctx, p, people, sightings)
	result.Preparing = preparing
	sure := 0
	for _, f := range sightings {
		if f.sure {
			sure++
		}
	}
	log.Printf("faces %s %s s%de%d at %.1f: %d faces, %d sure, of %d people (frame %s, faces %s, all %s)",
		r.Title, scope, r.Season, r.Episode, r.Time, len(sightings), sure, len(people),
		decoded.Sub(started).Round(time.Millisecond), detected.Sub(decoded).Round(time.Millisecond),
		time.Since(started).Round(time.Millisecond))
	return result, err
}

// Again tells anew who is in a pause kept, by the references made since.
func (s *Service) Again(ctx context.Context, id int64) (Result, error) {
	p, sightings, err := s.store.pause(ctx, id)
	if err != nil {
		return Result{}, err
	}
	m, ok := s.tmdb.Match(ctx, p.title)
	if !ok || scopeOf(m) != p.scope {
		return Result{}, ErrNotMatched
	}
	people, err := s.people(ctx, m, p.season, p.episode)
	if err != nil {
		return Result{}, err
	}
	preparing, err := s.enqueue(ctx, ids(people))
	if err != nil {
		return Result{}, err
	}
	if err := s.guess(ctx, p.scope, people, sightings); err != nil {
		return Result{}, err
	}
	if err := s.store.rematched(ctx, sightings); err != nil {
		return Result{}, err
	}
	result, err := s.result(ctx, p, people, sightings)
	result.Preparing = preparing
	return result, err
}

// guess takes the faces of a frame for the people, by their references.
func (s *Service) guess(ctx context.Context, scope string, people []tmdb.Person, sightings []sighting) error {
	refs, err := s.store.refs(ctx, scope, model, ids(people))
	if err != nil {
		return err
	}
	for i, g := range match(sightings, ids(people), refs) {
		sightings[i].person, sightings[i].score, sightings[i].sure = g.person, g.score, g.sure
		sightings[i].alternatives = g.alternatives
	}
	return nil
}

func (s *Service) result(ctx context.Context, p pause, people []tmdb.Person, sightings []sighting) (Result, error) {
	byID := map[uint64]tmdb.Person{}
	for _, person := range people {
		byID[person.ID] = person
	}
	portraits, err := s.store.portraits(ctx, p.scope, ids(people))
	if err != nil {
		return Result{}, err
	}
	person := func(id uint64) Person {
		x := byID[id]
		result := Person{ID: id, Name: x.Name, Character: x.Character, Profile: x.Profile}
		if crop, ok := portraits[id]; ok {
			result.Portrait = cropURL(crop)
		}
		return result
	}
	result := Result{Pause: p.id, Frame: frameURL(p.id), Width: p.width, Height: p.height, Faces: []Face{}}
	for _, f := range sightings {
		face := Face{Box: f.box, Crop: cropURL(f.id), Sure: f.sure}
		if f.person != 0 {
			x := person(f.person)
			face.Person, face.Score = &x, round(f.score)
		}
		for _, a := range f.alternatives {
			face.Alternatives = append(face.Alternatives, Alternative{Person: person(a.person), Score: round(a.score)})
		}
		result.Faces = append(result.Faces, face)
	}
	return result, nil
}

func round(x float64) float64 { return math.Round(x*1000) / 1000 }

func frameURL(id int64) string { return "/v1/faces/frames/" + strconv.FormatInt(id, 10) + ".jpg" }
func cropURL(id int64) string  { return "/v1/faces/crops/" + strconv.FormatInt(id, 10) + ".jpg" }

// Picture is a picture of a pause kept: its frame (frames) or a face
// (crops), by its ID.
func (s *Service) Picture(kind string, id int64) ([]byte, error) {
	return s.pictures.get(kind, id)
}

// Portrait is the best picture of someone seen in the pauses of a title on
// TMDB (scope, as tv:1405), that was surely them.
func (s *Service) Portrait(ctx context.Context, scope string, person uint64) ([]byte, error) {
	portraits, err := s.store.portraits(ctx, scope, []uint64{person})
	if err != nil {
		return nil, err
	}
	crop, ok := portraits[person]
	if !ok {
		return nil, os.ErrNotExist
	}
	return s.pictures.get(cropPath, crop)
}

// The kinds of pictures kept, as their directories and paths name them.
const (
	framePath = "frames"
	cropPath  = "crops"
)
