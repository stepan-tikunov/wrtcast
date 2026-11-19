package faces

import (
	"cmp"
	"math"
	"slices"
	"sort"
)

// How alike a face and someone's references are to be (the cosine of
// their vectors, see score) for the face to be taken for them: surely, at
// least sureScore and by sureGap more than for anyone else; maybe, at
// least maybeScore. Started from what ArcFace's vectors of a portrait and
// of a frame of the same person tend to be, to be set by the faces seen.
const (
	sureScore  = 0.40
	sureGap    = 0.06
	maybeScore = 0.28
)

// A face too small, too turned or too blurred is never taken for anyone
// surely.
const (
	goodSide = 64
	goodYaw  = 0.5
	goodDet  = 0.6
)

// score is how alike a face is to someone by their references: of each
// source, the mean of the two most alike, weighed by the source; the most
// of the sources.
func score(v vector, refs []reference) float64 {
	bySource := map[string][]float64{}
	for _, r := range refs {
		bySource[r.source] = append(bySource[r.source], v.dot(r.vector))
	}
	best := math.Inf(-1)
	for source, scores := range bySource {
		sort.Sort(sort.Reverse(sort.Float64Slice(scores)))
		value := scores[0]
		if len(scores) > 1 {
			value = (scores[0] + scores[1]) / 2
		}
		weight, ok := sourceWeights[source]
		if !ok {
			weight = 1
		}
		best = math.Max(best, value*weight)
	}
	return best
}

// guess is what a face was taken for: someone, surely or maybe, and the
// two most alike of the people, if it was not sure.
type guess struct {
	person       uint64
	score        float64
	sure         bool
	alternatives []candidateScore
}

type candidateScore struct {
	person uint64
	score  float64
}

// pair is how alike a face of a frame is to someone.
type pair struct {
	face int
	candidateScore
}

// good tells whether a face is clear enough to be told surely.
func good(f sighting) bool {
	return f.quality >= goodSide && math.Abs(f.yaw) <= goodYaw && f.det >= goodDet
}

// match takes the faces of a frame for the people of the title, each
// someone else: the most alike pair first, and so on.
func match(faces []sighting, people []uint64, refs map[uint64][]reference) []guess {
	guesses := make([]guess, len(faces))
	ranked := make([][]candidateScore, len(faces))
	var pairs []pair
	for i, f := range faces {
		if f.vector == nil {
			continue
		}
		for _, person := range people {
			if len(refs[person]) == 0 {
				continue
			}
			s := score(f.vector, refs[person])
			ranked[i] = append(ranked[i], candidateScore{person, s})
			pairs = append(pairs, pair{i, candidateScore{person, s}})
		}
		slices.SortStableFunc(ranked[i], func(a, b candidateScore) int {
			return cmp.Compare(b.score, a.score)
		})
	}
	slices.SortStableFunc(pairs, func(a, b pair) int { return cmp.Compare(b.score, a.score) })
	taken, told := map[uint64]bool{}, make([]bool, len(faces))
	for _, p := range pairs {
		if p.score < maybeScore {
			break
		}
		if told[p.face] || taken[p.person] {
			continue
		}
		told[p.face], taken[p.person] = true, true
		g := guess{person: p.person, score: p.score}
		// The gap is to the next most alike of anyone, taken or not.
		next := 0.0
		for _, c := range ranked[p.face] {
			if c.person != p.person {
				next = c.score
				break
			}
		}
		g.sure = good(faces[p.face]) && p.score >= sureScore && p.score-next >= sureGap
		guesses[p.face] = g
	}
	for i := range guesses {
		if guesses[i].sure {
			continue
		}
		for _, c := range ranked[i] {
			if len(guesses[i].alternatives) == 2 || c.score < maybeScore {
				break
			}
			guesses[i].alternatives = append(guesses[i].alternatives, c)
		}
	}
	return guesses
}
