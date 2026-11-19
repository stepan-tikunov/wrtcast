package faces

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"
)

// Of someone's photos on TMDB, those made references of at most: their
// portraits (the most voted first) and the pictures they are tagged in,
// stills and backdrops first; mostly they are posters, which are taken
// only by the portraits, as they are drawn over too often.
const (
	profilesUsed = 3
	taggedUsed   = 3
	// The least a face in a picture of several is to be like their
	// portraits to be taken for them, and by how much more than the next.
	taggedScore = 0.35
	taggedGap   = 0.08
)

// preparedKey is what the people prepared are kept by: the model, and how
// their photos are chosen, which made anew changes it (2: posters too).
const preparedKey = model + "/2"

// prepare makes the references of someone of their photos on TMDB.
func (s *Service) prepare(ctx context.Context, person uint64) {
	started := time.Now()
	photos, err := s.tmdb.Photos(ctx, person)
	if err != nil {
		log.Printf("faces person %d: %v", person, err)
		return
	}
	var profiles []reference
	for _, file := range photos.Profiles {
		if len(profiles) == profilesUsed {
			break
		}
		// The largest face's vector alone: a portrait is of one.
		faces, err := s.photoFaces(ctx, person, "w342", file, look{size: 320, minSide: 48, most: 1})
		if err != nil {
			log.Printf("faces person %d: %v", person, err)
			return
		}
		if len(faces) == 0 {
			continue
		}
		// A portrait is of one face; another nearly as large makes it
		// unsure whose.
		f := faces[0]
		if len(faces) > 1 && faces[1].area() > 0.4*f.area() || f.Det < goodDet || math.Abs(f.Yaw) > goodYaw {
			continue
		}
		profiles = append(profiles, reference{person: person, source: sourceProfile, ref: file, vector: f.Embedding, quality: f.side()})
	}
	var centroid vector
	if len(profiles) > 0 {
		vectors := make([]vector, len(profiles))
		for i, r := range profiles {
			vectors[i] = r.vector
		}
		centroid = mean(vectors)
	}
	var tagged []reference
	for _, photo := range photos.Tagged {
		if len(tagged) == taggedUsed {
			break
		}
		size := "w1280"
		if photo.Type == "poster" {
			if centroid == nil {
				continue
			}
			size = "w780"
		}
		faces, err := s.photoFaces(ctx, person, size, photo.File, look{size: 640, minSide: 80, most: 4})
		if err != nil {
			log.Printf("faces person %d: %v", person, err)
			return
		}
		var clear []face
		for _, f := range faces {
			if f.Embedding != nil && f.Det >= goodDet && math.Abs(f.Yaw) <= goodYaw {
				clear = append(clear, f)
			}
		}
		var chosen *face
		switch {
		case centroid != nil:
			best, next := -1.0, -1.0
			for i := range clear {
				s := clear[i].Embedding.dot(centroid)
				if s > best {
					best, next, chosen = s, best, &clear[i]
				} else if s > next {
					next = s
				}
			}
			if best < taggedScore || best-next < taggedGap {
				chosen = nil
			}
		case len(clear) == 1:
			chosen = &clear[0]
		}
		if chosen != nil {
			tagged = append(tagged, reference{person: person, source: sourceTagged, ref: photo.File, vector: chosen.Embedding, quality: chosen.side()})
		}
	}
	refs := append(profiles, tagged...)
	for _, r := range refs {
		if err := s.store.addRef(ctx, "", model, r); err != nil {
			log.Printf("faces person %d: %v", person, err)
			return
		}
	}
	if err := s.store.setPrepared(ctx, preparedKey, person, len(refs)); err != nil {
		log.Printf("faces person %d: %v", person, err)
		return
	}
	log.Printf("faces person %d: %d portraits and %d tagged of %d and %d in %s", person, len(profiles), len(tagged),
		len(photos.Profiles), len(photos.Tagged), time.Since(started).Round(time.Millisecond))
}

// photoFaces are the faces in a picture of TMDB, the largest first; none
// if TMDB fails to give it. An error is the faces service's, which leaves
// the person to be prepared again.
func (s *Service) photoFaces(ctx context.Context, person uint64, size, file string, l look) ([]face, error) {
	picture, _, err := s.images.Image(ctx, size, file)
	if err != nil {
		log.Printf("faces person %d: %s: %v", person, file, err)
		return nil, nil
	}
	found, err := s.detect(ctx, picture, l, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return found.Faces, nil
}
