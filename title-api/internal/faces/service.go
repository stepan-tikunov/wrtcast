package faces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"title-api/internal/tmdb"
)

// Of the people of an episode played, references are made of them all,
// those in the episode first (see people); a pause is told Preparing while
// the first of them are yet to be made of: the episode's, else the first
// billed of the title, at most leadLimit.
const (
	castLead  = 20
	leadLimit = 40
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
	EpisodeCast(ctx context.Context, id uint64, season, episode int32) ([]tmdb.Person, error)
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
	// The pauses whose faces are being found, by their IDs.
	finding map[int64]chan struct{}
}

// How long the faces of a pause are found at most, and how long a request
// waits for them before it is answered they are still being found.
const (
	findLimit = 2 * time.Minute
	findWait  = 15 * time.Second
)

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
		wake: make(chan struct{}, 1), finding: map[int64]chan struct{}{},
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
	// The faces are still being found: Again waits for them.
	Finding bool `json:"finding"`
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

// people are the people of an episode of a title on TMDB, all of them:
// those TMDB tells are in the episode first, as billed in it (the series'
// cast, then its guest stars), then the rest of the title's cast, as
// billed, of a series all its seasons'. lead is how many of them first a
// pause waits for the references of.
func (s *Service) people(ctx context.Context, m tmdb.Match, season, episode int32) (people []tmdb.Person, lead int, err error) {
	cast, err := s.tmdb.Cast(ctx, m.Kind, m.ID)
	if err != nil {
		return nil, 0, err
	}
	var inEpisode []tmdb.Person
	if m.Kind == tmdb.KindTV && season > 0 && episode > 0 {
		if inEpisode, err = s.tmdb.EpisodeCast(ctx, m.ID, season, episode); err != nil {
			log.Printf("faces %s s%de%d: %v", scopeOf(m), season, episode, err)
		}
	}
	// Someone of the cast is told as the cast tells them, in the episode
	// too.
	billed := map[uint64]tmdb.Person{}
	for _, p := range cast {
		billed[p.ID] = p
	}
	seen := map[uint64]bool{}
	add := func(list []tmdb.Person) {
		for _, p := range list {
			if p.ID != 0 && !seen[p.ID] {
				seen[p.ID] = true
				if b, ok := billed[p.ID]; ok {
					p = b
				}
				people = append(people, p)
			}
		}
	}
	add(inEpisode)
	lead = len(people)
	if lead == 0 {
		lead = castLead
	}
	add(cast)
	return people, min(lead, leadLimit, len(people)), nil
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
	people, lead, err := s.people(ctx, m, season, episode)
	if err != nil {
		return false, err
	}
	return s.enqueue(ctx, ids(people), lead)
}

// enqueue puts the people with no references first in the queue, in
// their order: whether any of the first lead of them are yet to have them.
func (s *Service) enqueue(ctx context.Context, people []uint64, lead int) (bool, error) {
	done, err := s.store.prepared(ctx, preparedKey, people)
	if err != nil {
		return false, err
	}
	var missing []uint64
	preparing := false
	s.mu.Lock()
	for i, p := range people {
		if _, ok := done[p]; !ok {
			preparing = preparing || i < lead
			if p != s.current {
				missing = append(missing, p)
			}
		}
	}
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

// Recognize keeps the paused frame and starts finding its faces and
// telling who they are, in the background: the frame at once, the faces
// none yet (Finding), which Again waits for.
func (s *Service) Recognize(ctx context.Context, r Request) (Result, error) {
	started := time.Now()
	m, ok := s.tmdb.Match(ctx, r.Title)
	if !ok {
		return Result{}, ErrNotMatched
	}
	people, lead, err := s.people(ctx, m, r.Season, r.Episode)
	if err != nil {
		return Result{}, err
	}
	preparing, err := s.enqueue(ctx, ids(people), lead)
	if err != nil {
		return Result{}, err
	}
	jpeg, err := pausedFrame(ctx, r.Segment, r.Offset)
	if err != nil {
		log.Printf("faces %s s%de%d at %.1f: %v", r.Title, r.Season, r.Episode, r.Time, err)
		return Result{}, err
	}
	picture, err := decodeJPEG(jpeg)
	if err != nil {
		return Result{}, fmt.Errorf("paused frame: %w", err)
	}
	frame, scale := scaled(picture)
	p := pause{
		title: r.Title, scope: scopeOf(m), season: r.Season, episode: r.Episode, t: r.Time,
		width: frame.Bounds().Dx(), height: frame.Bounds().Dy(), model: model, at: time.Now(),
	}
	if p.id, _, err = s.store.addPause(ctx, p, nil); err != nil {
		return Result{}, err
	}
	if err := s.pictures.put(framePath, p.id, encodeJPEG(frame, 82)); err != nil {
		return Result{}, err
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.finding[p.id] = done
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.finding, p.id)
			s.mu.Unlock()
			close(done)
		}()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), findLimit)
		defer cancel()
		if err := s.find(ctx, p, people, jpeg, picture, scale, started); err != nil {
			log.Printf("faces %s %s s%de%d at %.1f: %v", r.Title, p.scope, r.Season, r.Episode, r.Time, err)
		}
	}()
	result := Result{
		Pause: p.id, Frame: frameURL(p.id), Width: p.width, Height: p.height, Faces: []Face{},
		Preparing: preparing, Finding: true,
	}
	return result, nil
}

// find finds the faces of a pause kept, tells who they are, and keeps
// them and their pictures.
func (s *Service) find(ctx context.Context, p pause, people []tmdb.Person, jpeg []byte, picture *image.RGBA,
	scale float64, started time.Time) error {
	framed := time.Now()
	found, err := s.detect(ctx, jpeg, pauseLook, true)
	if err != nil {
		return err
	}
	detected := time.Now()
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
	if err := s.guess(ctx, p.scope, people, sightings); err != nil {
		return err
	}
	sightingIDs, err := s.store.addSightings(ctx, p, sightings)
	if err != nil {
		return err
	}
	for i, box := range boxes {
		if err := s.pictures.put(cropPath, sightingIDs[i], encodeJPEG(crop(picture, box), 88)); err != nil {
			return err
		}
	}
	sure := 0
	for _, f := range sightings {
		if f.sure {
			sure++
		}
	}
	log.Printf("faces %s %s s%de%d at %.1f: %d faces, %d sure, of %d people (frame %s, faces %s, all %s)",
		p.title, p.scope, p.season, p.episode, p.t, len(sightings), sure, len(people),
		framed.Sub(started).Round(time.Millisecond), detected.Sub(framed).Round(time.Millisecond),
		time.Since(started).Round(time.Millisecond))
	return nil
}

// Again tells anew who is in a pause kept, by the references made since;
// while its faces are being found, it waits for them a while (else
// Finding, with none).
func (s *Service) Again(ctx context.Context, id int64) (Result, error) {
	s.mu.Lock()
	done := s.finding[id]
	s.mu.Unlock()
	finding := false
	if done != nil {
		wait := time.NewTimer(findWait)
		select {
		case <-done:
		case <-wait.C:
			finding = true
		case <-ctx.Done():
			wait.Stop()
			return Result{}, ctx.Err()
		}
		wait.Stop()
	}
	p, sightings, err := s.store.pause(ctx, id)
	if err != nil {
		return Result{}, err
	}
	m, ok := s.tmdb.Match(ctx, p.title)
	if !ok || scopeOf(m) != p.scope {
		return Result{}, ErrNotMatched
	}
	people, lead, err := s.people(ctx, m, p.season, p.episode)
	if err != nil {
		return Result{}, err
	}
	preparing, err := s.enqueue(ctx, ids(people), lead)
	if err != nil {
		return Result{}, err
	}
	if finding {
		return Result{
			Pause: p.id, Frame: frameURL(p.id), Width: p.width, Height: p.height, Faces: []Face{},
			Preparing: preparing, Finding: true,
		}, nil
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
