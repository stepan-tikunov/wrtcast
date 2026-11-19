package faces

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The sources of references, by how much a face's likeness to one counts.
const (
	sourceProfile = "tmdb_profile"
	sourceTagged  = "tmdb_tagged"
	// Of a series' clusters of faces, and the faces told by hand, to come.
	sourceCluster   = "cluster"
	sourceConfirmed = "confirmed"
)

var sourceWeights = map[string]float64{
	sourceProfile: 0.95, sourceTagged: 1, sourceCluster: 1.1, sourceConfirmed: 1.2,
}

// The tables: the references, a vector per person, source and picture,
// scoped to a series or a movie (cluster, confirmed) or not (TMDB's
// photos, which are of the person); whom of the people references were
// made for (prepared); the pauses looked at and the faces seen in them,
// with their vectors and what they were taken for, kept for the clusters.
const schema = `
	CREATE TABLE IF NOT EXISTS face_refs (
		scope TEXT NOT NULL, person INTEGER NOT NULL, source TEXT NOT NULL, ref TEXT NOT NULL,
		model TEXT NOT NULL, embedding BLOB NOT NULL, quality REAL NOT NULL, added INTEGER NOT NULL,
		PRIMARY KEY (scope, person, source, ref, model)
	);
	CREATE INDEX IF NOT EXISTS face_refs_person ON face_refs (person, model);
	CREATE TABLE IF NOT EXISTS face_prepared (
		person INTEGER NOT NULL, model TEXT NOT NULL, refs INTEGER NOT NULL, at INTEGER NOT NULL,
		PRIMARY KEY (person, model)
	);
	CREATE TABLE IF NOT EXISTS face_pauses (
		id INTEGER PRIMARY KEY, title TEXT NOT NULL, scope TEXT NOT NULL,
		season INTEGER, episode INTEGER, t REAL NOT NULL,
		width INTEGER NOT NULL, height INTEGER NOT NULL, model TEXT NOT NULL, at INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS face_sightings (
		id INTEGER PRIMARY KEY, pause INTEGER NOT NULL, scope TEXT NOT NULL,
		box TEXT NOT NULL, embedding BLOB, quality REAL NOT NULL, det REAL NOT NULL, yaw REAL NOT NULL,
		blur REAL NOT NULL, model TEXT NOT NULL, person INTEGER, score REAL, sure INTEGER NOT NULL,
		confirmed INTEGER, at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS face_sightings_pause ON face_sightings (pause);
	CREATE INDEX IF NOT EXISTS face_sightings_portrait ON face_sightings (scope, person, sure, quality);`

type store struct {
	db *sql.DB
}

func openStore(ctx context.Context, db *sql.DB) (*store, error) {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("initialize faces state: %w", err)
	}
	return &store{db: db}, nil
}

// reference is a vector of someone's face.
type reference struct {
	person  uint64
	source  string
	ref     string
	vector  vector
	quality float64
}

func (s *store) addRef(ctx context.Context, scope, model string, r reference) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO face_refs (scope, person, source, ref, model, embedding, quality, added)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, scope, r.person, r.source, r.ref, model, r.vector.bytes(), r.quality, time.Now().Unix())
	return err
}

// refs are the references of people in a scope: TMDB's of them, and the
// scope's own.
func (s *store) refs(ctx context.Context, scope, model string, people []uint64) (map[uint64][]reference, error) {
	result := map[uint64][]reference{}
	if len(people) == 0 {
		return result, nil
	}
	ids, err := json.Marshal(people)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT person, source, ref, embedding, quality FROM face_refs
		WHERE model = ? AND scope IN ('', ?) AND person IN (SELECT value FROM json_each(?))`, model, scope, string(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r reference
		var raw []byte
		if err := rows.Scan(&r.person, &r.source, &r.ref, &raw, &r.quality); err != nil {
			return nil, err
		}
		if err := r.vector.decode(raw); err != nil {
			return nil, err
		}
		result[r.person] = append(result[r.person], r)
	}
	return result, rows.Err()
}

// prepared tells of the people which had references made, and when.
func (s *store) prepared(ctx context.Context, model string, people []uint64) (map[uint64]time.Time, error) {
	ids, err := json.Marshal(people)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT person, refs, at FROM face_prepared
		WHERE model = ? AND person IN (SELECT value FROM json_each(?))`, model, string(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[uint64]time.Time{}
	for rows.Next() {
		var person uint64
		var refs int
		var at int64
		if err := rows.Scan(&person, &refs, &at); err != nil {
			return nil, err
		}
		// One with no photo to tell them by is tried again a week later.
		if refs > 0 || time.Since(time.Unix(at, 0)) < 7*24*time.Hour {
			result[person] = time.Unix(at, 0)
		}
	}
	return result, rows.Err()
}

func (s *store) setPrepared(ctx context.Context, model string, person uint64, refs int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO face_prepared (person, model, refs, at) VALUES (?, ?, ?, ?)`,
		person, model, refs, time.Now().Unix())
	return err
}

// pause is a pause looked at.
type pause struct {
	id            int64
	title, scope  string
	season        int32
	episode       int32
	t             float64
	width, height int
	model         string
	at            time.Time
}

// sighting is a face seen in a pause: its box in the pause's frame, as
// kept, and what it was taken for.
type sighting struct {
	id     int64
	box    [4]float64
	vector vector
	// The shorter side of the face in the segment's pixels, how sure the
	// detector was, how far it is turned and how sharp.
	quality, det, yaw, blur float64
	person                  uint64
	score                   float64
	sure                    bool
	// The two most alike, if not surely someone; not kept.
	alternatives []candidateScore
}

func (s *store) addPause(ctx context.Context, p pause, sightings []sighting) (int64, []int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO face_pauses (title, scope, season, episode, t, width, height, model, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.title, p.scope, nullable(p.season), nullable(p.episode), p.t, p.width, p.height, p.model, p.at.Unix())
	if err != nil {
		return 0, nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, nil, err
	}
	ids := make([]int64, len(sightings))
	for i, f := range sightings {
		box, _ := json.Marshal(f.box)
		var raw []byte
		if f.vector != nil {
			raw = f.vector.bytes()
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO face_sightings (pause, scope, box, embedding, quality, det, yaw, blur, model, person, score, sure, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, p.scope, string(box), raw, f.quality, f.det, f.yaw, f.blur, p.model,
			nullable(f.person), nullable(f.score), f.sure, p.at.Unix())
		if err != nil {
			return 0, nil, err
		}
		if ids[i], err = result.LastInsertId(); err != nil {
			return 0, nil, err
		}
	}
	return id, ids, tx.Commit()
}

var errNoPause = errors.New("no such pause")

func (s *store) pause(ctx context.Context, id int64) (pause, []sighting, error) {
	p := pause{id: id}
	var season, episode sql.NullInt32
	var at int64
	err := s.db.QueryRowContext(ctx, `
		SELECT title, scope, season, episode, t, width, height, model, at FROM face_pauses WHERE id = ?`, id).
		Scan(&p.title, &p.scope, &season, &episode, &p.t, &p.width, &p.height, &p.model, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil, errNoPause
	}
	if err != nil {
		return p, nil, err
	}
	p.season, p.episode, p.at = season.Int32, episode.Int32, time.Unix(at, 0)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, box, embedding, quality, det, yaw, blur FROM face_sightings WHERE pause = ? ORDER BY id`, id)
	if err != nil {
		return p, nil, err
	}
	defer rows.Close()
	var sightings []sighting
	for rows.Next() {
		var f sighting
		var box string
		var raw []byte
		if err := rows.Scan(&f.id, &box, &raw, &f.quality, &f.det, &f.yaw, &f.blur); err != nil {
			return p, nil, err
		}
		if err := json.Unmarshal([]byte(box), &f.box); err != nil {
			return p, nil, err
		}
		if raw != nil {
			if err := f.vector.decode(raw); err != nil {
				return p, nil, err
			}
		}
		sightings = append(sightings, f)
	}
	return p, sightings, rows.Err()
}

// rematched keeps what the faces of a pause were taken for anew.
func (s *store) rematched(ctx context.Context, sightings []sighting) error {
	for _, f := range sightings {
		if _, err := s.db.ExecContext(ctx, `UPDATE face_sightings SET person = ?, score = ?, sure = ? WHERE id = ?`,
			nullable(f.person), nullable(f.score), f.sure, f.id); err != nil {
			return err
		}
	}
	return nil
}

// portraits are, of people in a scope, the faces of theirs seen there that
// were surely them, the best of each: the largest, the latest of those.
func (s *store) portraits(ctx context.Context, scope string, people []uint64) (map[uint64]int64, error) {
	ids, err := json.Marshal(people)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT person, id FROM (
			SELECT coalesce(confirmed, person) AS person, id, row_number() OVER (
				PARTITION BY coalesce(confirmed, person) ORDER BY confirmed IS NULL, quality DESC, id DESC) AS n
			FROM face_sightings
			WHERE scope = ? AND (sure = 1 OR confirmed IS NOT NULL)
				AND coalesce(confirmed, person) IN (SELECT value FROM json_each(?)))
		WHERE n = 1`, scope, string(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[uint64]int64{}
	for rows.Next() {
		var person uint64
		var id int64
		if err := rows.Scan(&person, &id); err != nil {
			return nil, err
		}
		result[person] = id
	}
	return result, rows.Err()
}

// kept are the faces whose pictures are to stay: the portraits.
func (s *store) kept(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM (
			SELECT id, row_number() OVER (
				PARTITION BY scope, coalesce(confirmed, person) ORDER BY confirmed IS NULL, quality DESC, id DESC) AS n
			FROM face_sightings WHERE sure = 1 OR confirmed IS NOT NULL)
		WHERE n = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result[id] = true
	}
	return result, rows.Err()
}

func nullable[T int32 | uint64 | float64](value T) any {
	if value == 0 {
		return nil
	}
	return value
}
