package music

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// The tables: the videos audio came of, the latest last; each piece of
// audio received (a segment's) with YAMNet's frames of it, on the video's
// time, and its file while it is kept; Shazam's asks of a video, what each
// told; and the tracks told.
const schema = `
	CREATE TABLE IF NOT EXISTS music_videos (
		video TEXT PRIMARY KEY, title TEXT NOT NULL, season INTEGER NOT NULL, episode INTEGER NOT NULL,
		source TEXT NOT NULL, last INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS music_audio (
		video TEXT NOT NULL, start REAL NOT NULL, stop REAL NOT NULL, frames TEXT NOT NULL,
		file TEXT, at INTEGER NOT NULL,
		PRIMARY KEY (video, start)
	);
	CREATE INDEX IF NOT EXISTS music_audio_file ON music_audio (at) WHERE file IS NOT NULL;
	CREATE TABLE IF NOT EXISTS music_asks (
		video TEXT NOT NULL, start REAL NOT NULL, stop REAL NOT NULL, track TEXT,
		offset REAL, duration REAL, at INTEGER NOT NULL,
		PRIMARY KEY (video, start)
	);
	CREATE TABLE IF NOT EXISTS music_tracks (
		key TEXT PRIMARY KEY, info TEXT NOT NULL, at INTEGER NOT NULL
	);`

type store struct {
	db *sql.DB
}

func openStore(ctx context.Context, db *sql.DB) (*store, error) {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("initialize music state: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) touch(ctx context.Context, v Video) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO music_videos (video, title, season, episode, source, last) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (video) DO UPDATE SET last = excluded.last`,
		v.key(), v.Title, v.Season, v.Episode, v.Source, time.Now().Unix())
	return err
}

// recent are the videos audio came of since, the latest first.
func (s *store) recent(ctx context.Context, since time.Time) ([]Video, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT title, season, episode, source FROM music_videos WHERE last >= ? ORDER BY last DESC`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Video
	for rows.Next() {
		var v Video
		if err := rows.Scan(&v.Title, &v.Season, &v.Episode, &v.Source); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s *store) hasAudio(ctx context.Context, video string, start float64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM music_audio WHERE video = ? AND start = ?`,
		video, start).Scan(&n)
	return n > 0, err
}

func (s *store) putAudio(ctx context.Context, video string, start, stop float64, frames []frame, file string) error {
	// [t, music, speech, song, mfcc…]
	encoded := make([][]float64, len(frames))
	for i, f := range frames {
		encoded[i] = append(make([]float64, 0, 4+len(f.MFCC)), f.T, f.Music, f.Speech, f.Song)
		for _, c := range f.MFCC {
			encoded[i] = append(encoded[i], math.Round(float64(c)*100)/100)
		}
	}
	data, err := json.Marshal(encoded)
	if err != nil {
		return err
	}
	var path any
	if file != "" {
		path = file
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO music_audio (video, start, stop, frames, file, at) VALUES (?, ?, ?, ?, ?, ?)`,
		video, start, stop, string(data), path, time.Now().Unix())
	return err
}

// frames are the frames of a video, in order, and the seconds of audio
// received of it.
func (s *store) frames(ctx context.Context, video string) ([]frame, float64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT start, stop, frames FROM music_audio WHERE video = ? ORDER BY start`, video)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var result []frame
	var seconds float64
	for rows.Next() {
		var start, stop float64
		var data string
		if err := rows.Scan(&start, &stop, &data); err != nil {
			return nil, 0, err
		}
		var encoded [][]float64
		if err := json.Unmarshal([]byte(data), &encoded); err != nil {
			return nil, 0, err
		}
		for _, f := range encoded {
			if len(f) < 4 {
				continue
			}
			g := frame{T: f[0], Music: f[1], Speech: f[2], Song: f[3]}
			if len(f) == 4+mfccs {
				g.MFCC = make([]float32, mfccs)
				for i, c := range f[4:] {
					g.MFCC[i] = float32(c)
				}
			}
			result = append(result, g)
		}
		seconds += stop - start
	}
	return result, seconds, rows.Err()
}

// piece is a piece of audio kept as a file.
type piece struct {
	Start, Stop float64
	File        string
}

// pieces are the pieces of a video's audio from before from up to to that
// are kept, in order.
func (s *store) pieces(ctx context.Context, video string, from, to float64) ([]piece, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT start, stop, file FROM music_audio
		WHERE video = ? AND stop > ? AND start < ? AND file IS NOT NULL ORDER BY start`, video, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []piece
	for rows.Next() {
		var p piece
		if err := rows.Scan(&p.Start, &p.Stop, &p.File); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// files are the files of audio kept and when each was received, the
// oldest first.
func (s *store) files(ctx context.Context) ([]string, []int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file, at FROM music_audio WHERE file IS NOT NULL ORDER BY at`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var files []string
	var at []int64
	for rows.Next() {
		var file string
		var t int64
		if err := rows.Scan(&file, &t); err != nil {
			return nil, nil, err
		}
		files = append(files, file)
		at = append(at, t)
	}
	return files, at, rows.Err()
}

func (s *store) forget(ctx context.Context, file string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE music_audio SET file = NULL WHERE file = ?`, file)
	return err
}

func (s *store) asks(ctx context.Context, video string) ([]ask, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT start, stop, coalesce(track, ''), coalesce(offset, 0), coalesce(duration, 0)
		FROM music_asks WHERE video = ? ORDER BY start`, video)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ask
	for rows.Next() {
		var a ask
		if err := rows.Scan(&a.Start, &a.Stop, &a.Key, &a.Offset, &a.Duration); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// putAsk keeps an ask, and the track it told.
func (s *store) putAsk(ctx context.Context, video string, a ask, m *match) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var track, offset, duration any
	if a.Key != "" {
		track, offset = a.Key, a.Offset
		if a.Duration > 0 {
			duration = a.Duration
		}
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO music_asks (video, start, stop, track, offset, duration, at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, video, a.Start, a.Stop, track, offset, duration, now); err != nil {
		return err
	}
	if m != nil {
		info := *m
		info.Offset = 0
		data, err := json.Marshal(info)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO music_tracks (key, info, at) VALUES (?, ?, ?)`,
			m.Key, string(data), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) track(ctx context.Context, key string) (match, error) {
	var data string
	var m match
	if err := s.db.QueryRowContext(ctx, `SELECT info FROM music_tracks WHERE key = ?`, key).Scan(&data); err != nil {
		return m, err
	}
	err := json.Unmarshal([]byte(data), &m)
	return m, err
}

// forgetMisses forgets the asks of a video ("" all) that found nothing:
// they are made again of the audio kept.
func (s *store) forgetMisses(ctx context.Context, video string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM music_asks WHERE track IS NULL AND (? = '' OR video = ?)`, video, video)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
