package tmdb

import (
	"cmp"
	"context"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// personCredit is a title someone is in or made, as combined_credits list
// them: once for the cast, once for each job of the crew.
type personCredit struct {
	related
	VoteCount    int    `json:"vote_count"`
	GenreIDs     []int  `json:"genre_ids"`
	Character    string `json:"character"`
	EpisodeCount uint32 `json:"episode_count"`
	Job          string `json:"job"`
}

type personTranslation struct {
	Language string `json:"iso_639_1"`
	Data     struct {
		Name      string `json:"name"`
		Biography string `json:"biography"`
	} `json:"data"`
}

// rawPerson is someone as /person/{id} has them, with their external IDs,
// credits and translations.
type rawPerson struct {
	ID           uint64   `json:"id"`
	Name         string   `json:"name"`
	AlsoKnownAs  []string `json:"also_known_as"`
	Biography    string   `json:"biography"`
	Birthday     string   `json:"birthday"`
	Deathday     string   `json:"deathday"`
	PlaceOfBirth string   `json:"place_of_birth"`
	ProfilePath  string   `json:"profile_path"`
	KnownFor     string   `json:"known_for_department"`
	Gender       int      `json:"gender"`
	ExternalIDs  struct {
		IMDb      string `json:"imdb_id"`
		Wikidata  string `json:"wikidata_id"`
		Instagram string `json:"instagram_id"`
	} `json:"external_ids"`
	CombinedCredits struct {
		Cast []personCredit `json:"cast"`
		Crew []personCredit `json:"crew"`
	} `json:"combined_credits"`
	Translations struct {
		Translations []personTranslation `json:"translations"`
	} `json:"translations"`
}

// person is someone on TMDB in Russian, with their external IDs, credits
// and translations, in one request.
func (c *client) person(ctx context.Context, id uint64) (rawPerson, error) {
	var result rawPerson
	err := c.get(ctx, "/person/"+strconv.FormatUint(id, 10), url.Values{
		"language":           {"ru-RU"},
		"append_to_response": {"external_ids,combined_credits,translations"},
	}, &result)
	return result, err
}

// The genders of a person, as TMDB numbers them.
const (
	GenderFemale    = 1
	GenderMale      = 2
	GenderNonBinary = 3
)

// PersonDetails is what a person's page shows of someone on TMDB, chosen
// from what TMDB has; what it lacks is empty.
type PersonDetails struct {
	ID           uint64 `json:"id"`
	Name         string `json:"name"`
	OriginalName string `json:"original_name,omitempty"`
	Profile      string `json:"profile,omitempty"`
	Profession   string `json:"profession,omitempty"`
	Birthday     string `json:"birthday,omitempty"`
	Deathday     string `json:"deathday,omitempty"`
	PlaceOfBirth string `json:"place_of_birth,omitempty"`
	Biography    string `json:"biography,omitempty"`
	Gender       int    `json:"gender,omitempty"`
	IMDb         string `json:"imdb,omitempty"`
	Instagram    string `json:"instagram,omitempty"`
	// From Wikidata, by the ID TMDB has.
	Wikidata  string `json:"wikidata,omitempty"`
	Wikipedia string `json:"wikipedia,omitempty"`
	Kinopoisk string `json:"kinopoisk,omitempty"`
	// Wikidata failed: kept for a day, not a month.
	Partial bool     `json:"partial,omitempty"`
	Credits []Credit `json:"credits"`
}

// Credit is a title someone is in or made, with all they were in it.
type Credit struct {
	Related
	Character string   `json:"character,omitempty"`
	Jobs      []string `json:"jobs,omitempty"`
	Episodes  uint32   `json:"episodes,omitempty"`
	Acting    bool     `json:"acting,omitempty"`
}

// Link is a page of a person elsewhere.
type Link struct {
	Site string
	URL  string
}

// Links are the person's pages: TMDB's, then those TMDB and Wikidata know.
func (p PersonDetails) Links() []Link {
	links := []Link{{"tmdb", "https://www.themoviedb.org/person/" + strconv.FormatUint(p.ID, 10)}}
	if p.Kinopoisk != "" {
		links = append(links, Link{"kinopoisk", "https://www.kinopoisk.ru/name/" + url.PathEscape(p.Kinopoisk) + "/"})
	}
	if p.IMDb != "" {
		links = append(links, Link{"imdb", "https://www.imdb.com/name/" + url.PathEscape(p.IMDb) + "/"})
	}
	if p.Wikipedia != "" {
		links = append(links, Link{"wikipedia", p.Wikipedia})
	}
	if p.Instagram != "" {
		links = append(links, Link{"instagram", "https://www.instagram.com/" + url.PathEscape(p.Instagram) + "/"})
	}
	return links
}

// How many titles of a person are kept, the best known.
const creditsKept = 300

// What a person is known for, in Russian, by TMDB's department.
var professions = map[string]string{
	"Acting": "Актёр", "Directing": "Режиссёр", "Writing": "Сценарист", "Production": "Продюсер",
	"Sound": "Композитор", "Camera": "Оператор", "Editing": "Монтажёр", "Art": "Художник",
	"Creator": "Создатель",
}

type job struct{ tmdb, name string }

// The jobs of the crew a person's page tells, in Russian, in this order;
// the others are left out.
var jobs = []job{
	{"Director", "Режиссёр"}, {"Creator", "Создатель"},
	{"Screenplay", "Сценарий"}, {"Writer", "Сценарий"}, {"Teleplay", "Сценарий"}, {"Story", "Сценарий"},
	{"Novel", "Книга"}, {"Producer", "Продюсер"},
	{"Original Music Composer", "Музыка"}, {"Music", "Музыка"},
	{"Director of Photography", "Оператор"}, {"Editor", "Монтаж"},
}

// The genres of TV shows people are in as themselves: news, reality, talk.
var selfGenres = []int{10763, 10764, 10767}

// asSelf is whom one plays in a title they are in as themselves ("Self -
// Host"); \b would not end a Cyrillic word.
var asSelf = regexp.MustCompile(`(?i)^(self|himself|herself|themselves|сам|сама|сами|себя)(\P{L}|$)`)

// Letters of Cyrillic alphabets other than Russian.
const notRussian = "іїєґўђјљњћџѓќѕ"

// russian tells how surely a name is written in Russian: 0 when it is not,
// its letters not all Cyrillic, of another language's or with ъ as
// Bulgarian has it (before a consonant); 3 with letters Russian alone of
// them has (э, ы, ё); 2 with those Bulgarian has too; else 1, as Serbian
// could be written.
func russian(name string) int {
	letters := []rune(strings.ToLower(name))
	sure := 0
	for i, r := range letters {
		if !unicode.IsLetter(r) {
			continue
		}
		if !unicode.Is(unicode.Cyrillic, r) || strings.ContainsRune(notRussian, r) ||
			r == 'ъ' && (i+1 == len(letters) || !strings.ContainsRune("еёюя", letters[i+1])) {
			return 0
		}
		switch {
		case strings.ContainsRune("эыё", r):
			sure = 3
		case strings.ContainsRune("йьюящ", r):
			sure = max(sure, 2)
		default:
			sure = max(sure, 1)
		}
	}
	return sure
}

func pickPerson(raw rawPerson) PersonDetails {
	texts := map[string]personTranslation{}
	for _, t := range raw.Translations.Translations {
		if _, ok := texts[t.Language]; !ok {
			texts[t.Language] = t
		}
	}
	result := PersonDetails{
		ID: raw.ID, Name: strings.TrimSpace(raw.Name), Profile: file(raw.ProfilePath),
		Birthday: raw.Birthday, Deathday: raw.Deathday, PlaceOfBirth: strings.TrimSpace(raw.PlaceOfBirth),
		Biography: text(cmp.Or(raw.Biography, texts["ru"].Data.Biography, texts["en"].Data.Biography)),
		Gender:    raw.Gender, IMDb: raw.ExternalIDs.IMDb, Instagram: raw.ExternalIDs.Instagram,
		Wikidata: raw.ExternalIDs.Wikidata, Credits: pickCredits(raw),
	}
	// The name in Russian, where TMDB has one, and in English.
	if russian(result.Name) == 0 {
		russianName, sure := strings.TrimSpace(texts["ru"].Data.Name), 0
		if russianName == "" {
			for _, name := range raw.AlsoKnownAs {
				if r := russian(name); r > sure {
					russianName, sure = strings.TrimSpace(name), r
				}
			}
		}
		if russianName != "" {
			result.Name, result.OriginalName = russianName, result.Name
		}
	}
	if english := strings.TrimSpace(texts["en"].Data.Name); result.OriginalName == "" && english != result.Name {
		result.OriginalName = english
	}
	result.Profession = professions[raw.KnownFor]
	if raw.KnownFor == "Acting" && raw.Gender == GenderFemale {
		result.Profession = "Актриса"
	}
	return result
}

// pickCredits are the titles someone is in or made, each once with all
// they did in it, the best known first: those they appear in as
// themselves and those for adults left out, and the crew's jobs other
// than those told.
func pickCredits(raw rawPerson) []Credit {
	type ranked struct {
		credit *Credit
		votes  int
	}
	var list []ranked
	byTitle := map[string]*Credit{}
	add := func(credit personCredit) *Credit {
		r := relatedOf(KindMovie, credit.related)
		key := r.Kind + ":" + strconv.FormatUint(r.ID, 10)
		if byTitle[key] == nil {
			byTitle[key] = &Credit{Related: r}
			list = append(list, ranked{byTitle[key], credit.VoteCount})
		}
		return byTitle[key]
	}
	for _, credit := range raw.CombinedCredits.Cast {
		character := strings.TrimSpace(credit.Character)
		if credit.Adult || asSelf.MatchString(character) ||
			credit.MediaType == KindTV && slices.ContainsFunc(credit.GenreIDs, func(g int) bool { return slices.Contains(selfGenres, g) }) {
			continue
		}
		c := add(credit)
		c.Acting = true
		c.Character = cmp.Or(c.Character, character)
		c.Episodes = max(c.Episodes, credit.EpisodeCount)
	}
	for _, credit := range raw.CombinedCredits.Crew {
		job := jobName(credit.Job)
		if credit.Adult || job == "" {
			continue
		}
		if c := add(credit); !slices.Contains(c.Jobs, job) {
			c.Jobs = append(c.Jobs, job)
		}
	}
	slices.SortStableFunc(list, func(a, b ranked) int {
		return cmp.Or(-cmp.Compare(a.votes, b.votes), -cmp.Compare(a.credit.Date, b.credit.Date))
	})
	result := make([]Credit, 0, min(len(list), creditsKept))
	for _, r := range list[:min(len(list), creditsKept)] {
		slices.SortStableFunc(r.credit.Jobs, func(a, b string) int { return cmp.Compare(jobRank(a), jobRank(b)) })
		result = append(result, *r.credit)
	}
	return result
}

// jobName is a job of the crew in Russian, empty for one not told.
func jobName(tmdb string) string {
	if i := slices.IndexFunc(jobs, func(j job) bool { return j.tmdb == tmdb }); i >= 0 {
		return jobs[i].name
	}
	return ""
}

func jobRank(name string) int {
	return slices.IndexFunc(jobs, func(j job) bool { return j.name == name })
}

func (p PersonDetails) ttl(time.Time) time.Duration {
	if p.Partial {
		return shortTTL
	}
	return longTTL
}
