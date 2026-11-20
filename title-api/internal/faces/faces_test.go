package faces

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"title-api/internal/tmdb"

	_ "modernc.org/sqlite"
)

// unit is a vector of unit length along a few dimensions, mixed.
func unit(weights map[int]float64) vector {
	v := make(vector, dimensions)
	var norm float64
	for i, w := range weights {
		norm += w * w
		v[i] = float32(w)
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(norm))
	}
	return v
}

func TestVectorRoundTrip(t *testing.T) {
	v := unit(map[int]float64{0: 1, 7: 2, 511: -3})
	var w vector
	if err := w.decode(v.bytes()); err != nil {
		t.Fatal(err)
	}
	if math.Abs(w.dot(v)-1) > 1e-6 {
		t.Fatalf("decoded vector differs: %v", w.dot(v))
	}
	text, _ := json.Marshal(base64.StdEncoding.EncodeToString(v.bytes()))
	var x vector
	if err := json.Unmarshal(text, &x); err != nil || math.Abs(x.dot(v)-1) > 1e-6 {
		t.Fatalf("JSON vector: %v %v", err, x.dot(v))
	}
	m := mean([]vector{unit(map[int]float64{0: 1}), unit(map[int]float64{1: 1})})
	if math.Abs(m.dot(m)-1) > 1e-6 || math.Abs(float64(m[0])-math.Sqrt(0.5)) > 1e-6 {
		t.Fatalf("mean: %v %v", m.dot(m), m[0])
	}
}

func TestMatch(t *testing.T) {
	alice, bob := unit(map[int]float64{0: 1}), unit(map[int]float64{1: 1})
	refs := map[uint64][]reference{
		1: {{person: 1, source: sourceProfile, vector: alice}, {person: 1, source: sourceTagged, vector: unit(map[int]float64{0: 1, 2: 0.3})}},
		2: {{person: 2, source: sourceProfile, vector: bob}},
	}
	clear := sighting{quality: 120, det: 0.9}
	faces := []sighting{
		// Like Alice and a little like Bob.
		{vector: unit(map[int]float64{0: 1, 1: 0.3, 3: 0.8}), quality: 120, det: 0.9},
		// Like Alice too, less: Alice is taken, so Bob or no one.
		{vector: unit(map[int]float64{0: 0.6, 1: 0.5, 4: 1}), quality: 120, det: 0.9},
		// Turned away: never sure.
		{vector: unit(map[int]float64{1: 1, 5: 0.2}), quality: 120, det: 0.9, yaw: 0.8},
		// Like no one.
		{vector: unit(map[int]float64{9: 1}), quality: clear.quality, det: clear.det},
	}
	guesses := match(faces, []uint64{1, 2, 3}, refs)
	if g := guesses[0]; g.person != 1 || !g.sure {
		t.Errorf("first face: %+v, want Alice surely", g)
	}
	if g := guesses[2]; g.person != 2 || g.sure {
		t.Errorf("turned face: %+v, want Bob, not surely", g)
	}
	if g := guesses[1]; g.person != 0 || g.sure || len(g.alternatives) == 0 || g.alternatives[0].person != 1 {
		t.Errorf("second face: %+v, want no one, Alice the most alike", g)
	}
	if g := guesses[3]; g.person != 0 || len(g.alternatives) != 0 {
		t.Errorf("stranger: %+v, want no one", g)
	}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestStorePortraits(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(ctx, testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	v := unit(map[int]float64{0: 1})
	_, ids, err := s.addPause(ctx, pause{title: "a", scope: "tv:1", season: 1, episode: 2, t: 10, width: 1280, height: 720, model: model}, []sighting{
		{box: [4]float64{1, 2, 3, 4}, vector: v, quality: 80, det: 0.9, person: 7, score: 0.5, sure: true},
		{box: [4]float64{5, 6, 7, 8}, vector: v, quality: 200, det: 0.9, person: 7, score: 0.3},
		{box: [4]float64{5, 6, 7, 8}, quality: 30, det: 0.7},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, more, err := s.addPause(ctx, pause{title: "a", scope: "tv:1", t: 20, model: model}, []sighting{
		{box: [4]float64{1, 2, 3, 4}, vector: v, quality: 100, det: 0.9, person: 7, score: 0.6, sure: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	portraits, err := s.portraits(ctx, "tv:1", []uint64{7, 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(portraits) != 1 || portraits[7] != more[0] {
		t.Fatalf("portraits %v, want the largest sure face %d", portraits, more[0])
	}
	p, sightings, err := s.pause(ctx, 1)
	if err != nil || p.season != 1 || p.episode != 2 || len(sightings) != 3 || sightings[2].vector != nil ||
		sightings[0].box != [4]float64{1, 2, 3, 4} || sightings[0].id != ids[0] {
		t.Fatalf("pause %+v %+v: %v", p, sightings, err)
	}
	kept, err := s.kept(ctx)
	if err != nil || len(kept) != 1 || !kept[more[0]] {
		t.Fatalf("kept %v: %v", kept, err)
	}
}

// segment is a second of red video and one of blue, as MPEG-TS.
func segment(t *testing.T) []byte {
	t.Helper()
	path := ffmpegPath()
	if path == "" {
		t.Skip("no ffmpeg")
	}
	out := filepath.Join(t.TempDir(), "segment.ts")
	command := exec.Command(path, "-loglevel", "error", "-f", "lavfi", "-i",
		"color=red:s=320x180:r=25:d=1[a];color=blue:s=320x180:r=25:d=1[b];[a][b]concat=n=2:v=1",
		"-c:v", "libx264", "-g", "50", "-output_ts_offset", "10", out)
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg can't make a segment: %v: %s", err, output)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPausedFrame(t *testing.T) {
	data := segment(t)
	for _, c := range []struct {
		offset float64
		blue   bool
	}{{0, false}, {0.9, false}, {1.1, true}, {1.95, true}} {
		frame, err := pausedFrame(context.Background(), data, c.offset)
		if err != nil {
			t.Fatalf("at %v: %v", c.offset, err)
		}
		picture, err := jpeg.Decode(bytes.NewReader(frame))
		if err != nil {
			t.Fatal(err)
		}
		r, _, b, _ := picture.At(160, 90).RGBA()
		if blue := b > r; blue != c.blue {
			t.Errorf("at %v: r %d b %d, want blue %v", c.offset, r>>8, b>>8, c.blue)
		}
	}
	if _, err := pausedFrame(context.Background(), data, 5); !errors.Is(err, ErrNoFrame) {
		t.Errorf("past the end: %v, want ErrNoFrame", err)
	}
}

func TestCrop(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 400, 300))
	for y := range 300 {
		for x := range 400 {
			picture.SetRGBA(x, y, color.RGBA{uint8(x / 2), uint8(y), 0, 255})
		}
	}
	c := crop(picture, [4]float64{350, 250, 390, 290})
	if c.Bounds().Dx() != cropSide || c.Bounds().Dy() != cropSide {
		t.Fatalf("crop %v", c.Bounds())
	}
	// Grown past the corner, it is moved in: its far corner is the frame's.
	if p := c.RGBAAt(cropSide-1, cropSide-1); p.R < 195 || p.G < 295-256 {
		t.Errorf("corner %v", p)
	}
	small, scale := scaled(image.NewRGBA(image.Rect(0, 0, 1920, 1080)))
	if small.Bounds().Dx() != 1280 || small.Bounds().Dy() != 720 || math.Abs(scale-2.0/3) > 1e-9 {
		t.Errorf("scaled %v %v", small.Bounds(), scale)
	}
}

type fakeTMDB struct{}

func (fakeTMDB) Match(_ context.Context, title string) (tmdb.Match, bool) {
	return tmdb.Match{ID: 1405, Kind: tmdb.KindTV}, title == "dexter"
}

func (fakeTMDB) Cast(context.Context, string, uint64) ([]tmdb.Person, error) {
	return []tmdb.Person{{ID: 1, Name: "Alice", Character: "A", Profile: "a.jpg"}, {ID: 2, Name: "Bob"}}, nil
}

func (fakeTMDB) Guests(context.Context, uint64, int32, int32) ([]tmdb.Person, error) {
	return []tmdb.Person{{ID: 3, Name: "Carol"}, {ID: 1, Name: "Alice"}}, nil
}

func (fakeTMDB) Photos(_ context.Context, id uint64) (tmdb.Photos, error) {
	if id == 3 {
		return tmdb.Photos{}, nil
	}
	photos := tmdb.Photos{Profiles: []string{"p" + string(rune('0'+id)) + ".jpg"}}
	if id == 1 {
		photos.Tagged = []tmdb.TaggedPhoto{{File: "poster.jpg", Type: "poster"}}
	}
	return photos, nil
}

type fakeImages struct{}

func (fakeImages) Image(_ context.Context, _, name string) ([]byte, string, error) {
	return []byte(name), "image/jpeg", nil
}

// fakeFaces answers a portrait of p1 or p2 with Alice's or Bob's face, and
// any frame with both, Alice's turned a little.
func fakeFaces(t *testing.T) *httptest.Server {
	alice, bob := unit(map[int]float64{0: 1}), unit(map[int]float64{1: 1})
	encode := func(v vector) string { return base64.StdEncoding.EncodeToString(v.bytes()) }
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		type f struct {
			Box       [4]float64 `json:"box"`
			Det       float64    `json:"det"`
			Yaw       float64    `json:"yaw"`
			Embedding string     `json:"embedding"`
		}
		var faces []f
		switch body.String() {
		case "p1.jpg":
			faces = []f{{Box: [4]float64{10, 10, 110, 130}, Det: 0.9, Embedding: encode(alice)}}
		case "p2.jpg":
			faces = []f{{Box: [4]float64{10, 10, 110, 130}, Det: 0.9, Embedding: encode(bob)}}
		// A poster of Alice with a stranger, larger.
		case "poster.jpg":
			faces = []f{
				{Box: [4]float64{0, 0, 200, 200}, Det: 0.9, Embedding: encode(unit(map[int]float64{8: 1}))},
				{Box: [4]float64{300, 0, 400, 100}, Det: 0.9, Embedding: encode(unit(map[int]float64{0: 1, 6: 0.5}))},
			}
		default:
			faces = []f{
				{Box: [4]float64{100, 50, 200, 170}, Det: 0.9, Yaw: 0.1, Embedding: encode(unit(map[int]float64{0: 1, 4: 0.7}))},
				{Box: [4]float64{240, 60, 290, 120}, Det: 0.8, Embedding: encode(unit(map[int]float64{1: 1, 5: 0.9}))},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "width": 320, "height": 180, "faces": faces})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRecognize(t *testing.T) {
	data := segment(t)
	ctx := context.Background()
	dir := t.TempDir()
	s, err := New(ctx, testDB(t), fakeFaces(t).URL, dir, fakeTMDB{}, fakeImages{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recognize(ctx, Request{Title: "other", Segment: data}); err != ErrNotMatched {
		t.Fatalf("unmatched title: %v", err)
	}
	r := Request{Title: "dexter", Season: 1, Episode: 2, Time: 125.3, Offset: 0.5, Segment: data}
	paused, err := s.Recognize(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	// The frame at once, the faces yet to be found.
	if !paused.Finding || len(paused.Faces) != 0 || paused.Frame == "" || paused.Width != 320 {
		t.Fatalf("paused: %+v", paused)
	}
	first, err := s.Again(ctx, paused.Pause)
	if err != nil {
		t.Fatal(err)
	}
	if first.Finding || !first.Preparing || len(first.Faces) != 2 || first.Faces[0].Person != nil {
		t.Fatalf("before the references: %+v", first)
	}
	if first.Width != 320 || first.Height != 180 || first.Faces[0].Box != [4]float64{100, 50, 200, 170} {
		t.Fatalf("frame %dx%d, box %v", first.Width, first.Height, first.Faces[0].Box)
	}
	for _, person := range s.queue {
		s.prepare(ctx, person)
	}
	s.queue = nil
	refs, err := s.store.refs(ctx, "tv:1405", model, []uint64{1})
	if err != nil || len(refs[1]) != 2 || refs[1][1].source != sourceTagged || refs[1][1].vector[0] < 0.8 {
		t.Fatalf("Alice's references %+v: %v, want her portrait and her face of the poster", refs[1], err)
	}
	again, err := s.Again(ctx, first.Pause)
	if err != nil {
		t.Fatal(err)
	}
	if again.Preparing {
		t.Errorf("still preparing: %+v", again)
	}
	alice, bob := again.Faces[0], again.Faces[1]
	if alice.Person == nil || alice.Person.Name != "Alice" || alice.Person.Character != "A" || !alice.Sure ||
		alice.Person.Portrait != alice.Crop {
		t.Errorf("first face: %+v %+v, want Alice surely, her portrait this face", alice, alice.Person)
	}
	// 50 pixels: too small to be sure.
	if bob.Person == nil || bob.Person.Name != "Bob" || bob.Sure || len(bob.Alternatives) == 0 {
		t.Errorf("second face: %+v %+v, want Bob, not surely", bob, bob.Person)
	}
	for _, path := range []string{again.Frame, alice.Crop, bob.Crop} {
		parts := strings.Split(path, "/")
		id, _ := strconv.ParseInt(strings.TrimSuffix(parts[len(parts)-1], ".jpg"), 10, 64)
		data, err := s.Picture(parts[len(parts)-2], id)
		if err != nil || len(data) == 0 {
			t.Errorf("%s: %v", path, err)
		}
		if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	portrait, err := s.Portrait(ctx, "tv:1405", 1)
	if err != nil || len(portrait) == 0 {
		t.Errorf("portrait: %v", err)
	}
	if _, err := s.Portrait(ctx, "tv:1405", 2); !os.IsNotExist(err) {
		t.Errorf("Bob's portrait: %v, want none", err)
	}
}
