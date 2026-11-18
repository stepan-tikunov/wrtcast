package tmdb

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The fixtures are TMDB's answers of 2026-11-18, recorded once and trimmed
// to what pick reads.
func fixture[T any](t *testing.T, name string) T {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func language(code string) *string { return &code }

func TestPickDetails(t *testing.T) {
	for _, test := range []struct {
		fixture, kind  string
		backdrop, logo string
		age            string
		videos         int
		firstVideo     string
		check          func(t *testing.T, d Details)
	}{
		{"fight-club", KindMovie, "c6OLXfKAk5BKeR6broC8pYiCquX.jpg", "y9RSpK5PpMYEkfdCRofBp09KpW9.png", "18+", 4, "ru",
			func(t *testing.T, d Details) {
				if strings.Contains(d.Overview, "\r") {
					t.Errorf("overview has \\r: %q", d.Overview)
				}
				if len(d.Directors) != 1 || d.Directors[0].Name != "Дэвид Финчер" || d.Runtime != 139 || d.Year() != "1999" {
					t.Errorf("directors %v, runtime %d, year %s", d.Directors, d.Runtime, d.Year())
				}
				if d.Series != nil || d.CollectionID != 0 {
					t.Errorf("series %v, collection %d", d.Series, d.CollectionID)
				}
			}},
		// No Russian rating: PG-13 is 12+.
		{"dune-part-two", KindMovie, "eZ239CUp1d6OryZEBPnO2n87gMG.jpg", "uo5HZB1p4mvDqJq59wENcOcjvpR.png", "12+", 4, "ru",
			func(t *testing.T, d Details) {
				if d.CollectionID != 726871 {
					t.Errorf("collection %d", d.CollectionID)
				}
				if d.Videos[0].Key != "42RV2e1GyzM" || d.Videos[1].Language != "en" {
					t.Errorf("videos %+v", d.Videos)
				}
			}},
		{"dexter", KindTV, "nS5ZSmrX92lu1GYAlXZye1mkDfd.jpg", "cuqAIuSXmHqHwfHHj8sEB7w0HOy.png", "18+", -1, "ru",
			func(t *testing.T, d Details) {
				if d.Series == nil || d.Series.Status != StatusEnded || d.Series.Last == nil || d.Series.Next != nil {
					t.Fatalf("series %+v", d.Series)
				}
				for _, season := range d.Series.SeasonList {
					if season.Number == 0 {
						t.Errorf("specials listed")
					}
				}
				if d.Series.Seasons != int32(len(d.Series.SeasonList)) {
					t.Errorf("%d seasons, %d listed", d.Series.Seasons, len(d.Series.SeasonList))
				}
				if d.Cast[0].Character != "Dexter Morgan" {
					t.Errorf("cast %+v", d.Cast[0])
				}
			}},
		{"vampires", KindTV, "3werZe0KJhNZbL2Tv4MuIqtg6bh.jpg", "V5bnhMhGIWTsh6izj8eVUvzotg.png", "16+", -1, "ru", nil},
		{"univer", KindTV, "tH3tvJ9JKw2zkQqNGy22cE92WVY.jpg", "zougGt0yteEMvmZTHVEhPwLYaBI.png", "16+", 0, "",
			func(t *testing.T, d Details) {
				if last := d.Series.Last; last == nil || last.Season != 5 || last.Name != "Край" {
					t.Errorf("last %+v", last)
				}
			}},
		{"durak", KindMovie, "c4ewJ2mW1EDf12p8VYV0cZghW3V.jpg", "hVl7qP2ZzaMT1vL5MVshnA891kJ.png", "16+", 3, "ru", nil},
		{"osman", KindTV, "puLaqdcTRwFxwv9S3PO6jA3ep7J.jpg", "duF2UvHYQGd2WKSMG33FLRvW2G1.png", "16+", -1, "",
			func(t *testing.T, d Details) {
				if d.OriginalName != "Kuruluş Osman" || len(d.Countries) != 1 || d.Countries[0] != "TR" {
					t.Errorf("original name %q, countries %v", d.OriginalName, d.Countries)
				}
			}},
		// The logo in English alone, an American rating, no trailers.
		{"prime-time", KindMovie, "2Vc0BTGCKRhbJcZnMD46V5zzjQR.jpg", "k7aWH6Tv6kDmp6hyNEydQvNmqGf.png", "18+", 0, "", nil},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			d := pickDetails(test.kind, fixture[rawDetails](t, test.fixture))
			if d.Kind != test.kind || d.ID == 0 || d.Name == "" || d.Overview == "" || d.Poster == "" {
				t.Errorf("kind %s, id %d, name %q, overview %q, poster %q", d.Kind, d.ID, d.Name, d.Overview, d.Poster)
			}
			if d.Backdrop != test.backdrop || d.Logo != test.logo || d.LogoAspect == 0 || d.Age != test.age {
				t.Errorf("backdrop %s, logo %s (%v), age %s", d.Backdrop, d.Logo, d.LogoAspect, d.Age)
			}
			if test.videos >= 0 && len(d.Videos) != test.videos || len(d.Videos) > videoLimit {
				t.Errorf("%d videos", len(d.Videos))
			}
			if test.firstVideo != "" && (len(d.Videos) == 0 || d.Videos[0].Language != test.firstVideo) {
				t.Errorf("videos %+v", d.Videos)
			}
			if len(d.Cast) <= CastShown {
				t.Errorf("%d cast", len(d.Cast))
			}
			for _, person := range d.Cast {
				if person.Name == "" || strings.HasPrefix(person.Profile, "/") {
					t.Errorf("cast %+v", person)
				}
			}
			if len(d.Recommendations) == 0 || len(d.Recommendations) > relatedLimit {
				t.Errorf("%d recommendations", len(d.Recommendations))
			}
			for _, r := range d.Recommendations {
				if r.Poster == "" || r.Kind != KindMovie && r.Kind != KindTV {
					t.Errorf("recommendation %+v", r)
				}
			}
			if test.check != nil {
				test.check(t, d)
			}
		})
	}
}

func TestPickBackdrop(t *testing.T) {
	for _, test := range []struct {
		name      string
		backdrops []image
		want      string
	}{
		{"none", nil, ""},
		{"no text first", []image{
			{FilePath: "/ru.jpg", Language: language("ru"), Width: 3840, VoteAverage: 9},
			{FilePath: "/plain.jpg", Width: 1920, VoteAverage: 5},
		}, "plain.jpg"},
		{"wide enough first", []image{
			{FilePath: "/small.jpg", Width: 1000, VoteAverage: 9},
			{FilePath: "/wide.jpg", Width: 1280, VoteAverage: 5},
			{FilePath: "/wider.jpg", Width: 3840, VoteAverage: 6},
		}, "wider.jpg"},
		{"any when all have text", []image{
			{FilePath: "/en.jpg", Language: language("en"), Width: 1920, VoteAverage: 5},
			{FilePath: "/ru.jpg", Language: language("ru"), Width: 1920, VoteAverage: 6},
		}, "ru.jpg"},
	} {
		if got := pickBackdrop(test.backdrops); got != test.want {
			t.Errorf("%s: %q, want %q", test.name, got, test.want)
		}
	}
}

func TestPickLogo(t *testing.T) {
	logos := []image{
		{FilePath: "/plain.png", VoteAverage: 9, Width: 400, Height: 100},
		{FilePath: "/en.png", Language: language("en"), VoteAverage: 8, AspectRatio: 3},
		{FilePath: "/ru.svg", Language: language("ru"), VoteAverage: 9},
		{FilePath: "/ru.png", Language: language("ru"), VoteAverage: 1, AspectRatio: 2.5},
		{FilePath: "/ru2.png", Language: language("ru"), VoteAverage: 2, AspectRatio: 2},
	}
	for _, test := range []struct {
		logos  []image
		want   string
		aspect float64
	}{
		{logos, "ru2.png", 2},
		{logos[:3], "en.png", 3},
		{logos[:1], "plain.png", 4},
		{logos[2:3], "", 0},
	} {
		if got, aspect := pickLogo(test.logos); got != test.want || aspect != test.aspect {
			t.Errorf("%v: %q (%v), want %q (%v)", test.logos, got, aspect, test.want, test.aspect)
		}
	}
}

func TestPickAge(t *testing.T) {
	movie := func(ratings map[string]string) rawDetails {
		var raw rawDetails
		for country, rating := range ratings {
			raw.ReleaseDates.Results = append(raw.ReleaseDates.Results, struct {
				Country      string `json:"iso_3166_1"`
				ReleaseDates []struct {
					Certification string `json:"certification"`
				} `json:"release_dates"`
			}{Country: country, ReleaseDates: []struct {
				Certification string `json:"certification"`
			}{{""}, {rating}}})
		}
		return raw
	}
	series := func(ratings map[string]string) rawDetails {
		var raw rawDetails
		for country, rating := range ratings {
			raw.ContentRatings.Results = append(raw.ContentRatings.Results, struct {
				Country string `json:"iso_3166_1"`
				Rating  string `json:"rating"`
			}{country, rating})
		}
		return raw
	}
	for _, test := range []struct {
		kind    string
		ratings map[string]string
		want    string
	}{
		{KindMovie, map[string]string{"RU": "16+", "US": "R"}, "16+"},
		{KindMovie, map[string]string{"RU": "6", "US": "R"}, "6+"},
		{KindMovie, map[string]string{"US": "G"}, "0+"},
		{KindMovie, map[string]string{"US": "PG"}, "6+"},
		{KindMovie, map[string]string{"US": "PG-13"}, "12+"},
		{KindMovie, map[string]string{"US": "R"}, "18+"},
		{KindMovie, map[string]string{"US": "NC-17"}, "18+"},
		{KindMovie, map[string]string{"US": "NR"}, ""},
		{KindMovie, map[string]string{"DE": "16"}, ""},
		{KindTV, map[string]string{"RU": "18+", "US": "TV-PG"}, "18+"},
		{KindTV, map[string]string{"US": "TV-Y"}, "0+"},
		{KindTV, map[string]string{"US": "TV-G"}, "0+"},
		{KindTV, map[string]string{"US": "TV-Y7"}, "6+"},
		{KindTV, map[string]string{"US": "TV-PG"}, "12+"},
		{KindTV, map[string]string{"US": "TV-14"}, "16+"},
		{KindTV, map[string]string{"US": "TV-MA"}, "18+"},
		{KindTV, nil, ""},
	} {
		raw := movie(test.ratings)
		if test.kind == KindTV {
			raw = series(test.ratings)
		}
		if got := pickAge(test.kind, raw); got != test.want {
			t.Errorf("%s %v: %q, want %q", test.kind, test.ratings, got, test.want)
		}
	}
}

func TestPickVideos(t *testing.T) {
	got := pickVideos([]video{
		{Key: "clip", Site: "YouTube", Type: "Clip", Language: "ru", Official: true},
		{Key: "vimeo", Site: "Vimeo", Type: "Trailer", Language: "ru", Official: true},
		{Key: "en-old", Site: "YouTube", Type: "Trailer", Language: "en", Official: true, PublishedAt: "2020"},
		{Key: "en-new", Site: "YouTube", Type: "Trailer", Language: "en", Official: true, PublishedAt: "2021"},
		{Key: "en-teaser", Site: "YouTube", Type: "Teaser", Language: "en", Official: true, PublishedAt: "2022"},
		{Key: "ru-fan", Site: "YouTube", Type: "Trailer", Language: "ru"},
		{Key: "ru", Site: "YouTube", Type: "Teaser", Language: "ru", Official: true},
	})
	var keys []string
	for _, v := range got {
		keys = append(keys, v.Key)
	}
	if strings.Join(keys, " ") != "ru ru-fan en-new en-old" {
		t.Fatalf("videos %v", keys)
	}
}

func TestEpisodeName(t *testing.T) {
	for name, want := range map[string]string{
		"Серия 5":     "",
		"серия 12":    "",
		"Эпизод 3":    "",
		"Episode 1":   "",
		"Bölüm 1":     "",
		"Pilot":       "",
		" Пилот ":     "Пилот",
		"1984":        "1984",
		"Серия 5. Да": "Серия 5. Да",
		"":            "",
	} {
		if got := episodeName(name); got != want {
			t.Errorf("episodeName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestPickSeason(t *testing.T) {
	dexter := pickSeason(fixture[rawSeason](t, "dexter-s1"), "2026-11-18")
	if len(dexter.Episodes) != 12 || dexter.Episodes[0].Name != "Декстер" || dexter.Episodes[0].Still == "" ||
		dexter.Episodes[0].Overview == "" || dexter.Episodes[0].Runtime == 0 || dexter.Episodes[0].Upcoming {
		t.Errorf("dexter %+v", dexter.Episodes[0])
	}
	// "Серия N" and no stills.
	for _, episode := range pickSeason(fixture[rawSeason](t, "univer-s1"), "2026-11-18").Episodes {
		if episode.Name != "" || episode.Still != "" {
			t.Errorf("univer %+v", episode)
		}
	}
	// Names in Turkish.
	for _, episode := range pickSeason(fixture[rawSeason](t, "osman-s1"), "2026-11-18").Episodes {
		if episode.Name != "" {
			t.Errorf("osman %+v", episode)
		}
	}
	vampires := pickSeason(fixture[rawSeason](t, "vampires-s1"), "2021-04-01")
	if !vampires.Episodes[7].Upcoming || vampires.Episodes[0].Upcoming || vampires.ttl(time.Now()) != shortTTL {
		t.Errorf("vampires as of 2021-04-01: %+v", vampires.Episodes)
	}
	if dexter.ttl(time.Now()) != longTTL || (Season{}).ttl(time.Now()) != shortTTL {
		t.Errorf("season TTLs")
	}
}

func TestPickCollection(t *testing.T) {
	raw := fixture[rawCollection](t, "dune-collection")
	raw.Parts[0], raw.Parts[2] = raw.Parts[2], raw.Parts[0]
	raw.Parts = append(raw.Parts, related{ID: 1, Title: "Дюна: Часть четвёртая"})
	collection := pickCollection(raw)
	var ids []uint64
	for _, part := range collection.Parts {
		ids = append(ids, part.ID)
	}
	if len(ids) != 4 || ids[0] != 438631 || ids[1] != 693134 || ids[2] != 1170608 || ids[3] != 1 {
		t.Fatalf("parts %v", ids)
	}
	if collection.Name == "" || collection.Parts[2].Date != "2026-12-15" {
		t.Errorf("collection %+v", collection)
	}
}

func TestDetailsTTL(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		details Details
		want    time.Duration
	}{
		{"old movie", Details{Date: "1999-10-15"}, longTTL},
		{"new movie", Details{Date: "2026-03-01"}, shortTTL},
		{"no date", Details{}, shortTTL},
		{"returning", Details{Date: "2006-10-01", Series: &Series{Status: StatusReturning}}, shortTTL},
		{"ended", Details{Date: "2006-10-01", Series: &Series{Status: StatusEnded}}, longTTL},
	} {
		if got := test.details.ttl(now); got != test.want {
			t.Errorf("%s: %v, want %v", test.name, got, test.want)
		}
	}
	collection := Collection{Parts: []Related{{Date: "2021-09-15"}, {Date: "2026-12-15"}}}
	if collection.ttl(now) != shortTTL || (Collection{Parts: collection.Parts[:1]}).ttl(now) != longTTL {
		t.Errorf("collection TTLs")
	}
}

func TestPickPerson(t *testing.T) {
	p := pickPerson(fixture[rawPerson](t, "person-norton"))
	// TMDB's name is in English: the Russian one of the names they are
	// known by, not the Ukrainian.
	if p.Name != "Эдвард Нортон" || p.OriginalName != "Edward Norton" || p.Profession != "Актёр" {
		t.Errorf("name %q, original %q, profession %q", p.Name, p.OriginalName, p.Profession)
	}
	// No biography in Russian: the English one.
	if p.Biography != "Edward Harrison Norton is an American actor.\n\nHe was born in Boston." ||
		p.PlaceOfBirth != "Boston, Massachusetts, USA" || p.Birthday != "1969-08-18" || p.Deathday != "" {
		t.Errorf("biography %q, born %q in %q, died %q", p.Biography, p.Birthday, p.PlaceOfBirth, p.Deathday)
	}
	// Each title once, the best known first; the talk show, the ceremony,
	// the adult movie and the jobs not told left out.
	var got []string
	for _, c := range p.Credits {
		got = append(got, fmt.Sprintf("%s:%d %q %v %d %t", c.Kind, c.ID, c.Character, c.Jobs, c.Episodes, c.Acting))
	}
	want := []string{
		`movie:550 "Narrator" [] 0 true`,
		`movie:120467 "Henckels" [] 0 true`,
		`movie:9294 "" [Режиссёр Сценарий Продюсер] 0 false`,
		`tv:4567 "" [] 3 true`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("credits\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := p.Credits[0]; c.Name != "Бойцовский клуб" || c.Year() != "1999" || c.Poster != "fc.jpg" {
		t.Errorf("credit %+v", c)
	}
	if c := p.Credits[3]; c.Name != "Сериал" || c.Date != "2020-01-01" {
		t.Errorf("series %+v", c)
	}
}

func TestPersonName(t *testing.T) {
	for _, test := range []struct {
		raw            rawPerson
		ru             string
		name, original string
		profession     string
	}{
		// In Russian on TMDB.
		{rawPerson{Name: "Константин Хабенский", KnownFor: "Acting"}, "", "Константин Хабенский", "", "Актёр"},
		{rawPerson{Name: "Ксения Раппопорт", KnownFor: "Acting", Gender: GenderFemale}, "", "Ксения Раппопорт", "", "Актриса"},
		// None in Russian: as TMDB has it.
		{rawPerson{Name: "Bong Joon-ho", AlsoKnownAs: []string{"봉준호", "Бон Чжун-хо"}, KnownFor: "Directing"}, "", "Бон Чжун-хо", "Bong Joon-ho", "Режиссёр"},
		{rawPerson{Name: "Some One", AlsoKnownAs: []string{"Сом Ўан", "Кристофър Нолан"}, KnownFor: "Lighting"}, "", "Some One", "", ""},
		// The Russian translation first.
		{rawPerson{Name: "Christopher Nolan", AlsoKnownAs: []string{"Кристофер Нолан"}}, "Кристофер Джонатан Нолан", "Кристофер Джонатан Нолан", "Christopher Nolan", ""},
		{rawPerson{Name: "Christopher Nolan", AlsoKnownAs: []string{"Крістофер Нолан", "Кристофер Нолан"}}, "", "Кристофер Нолан", "Christopher Nolan", ""},
	} {
		if test.ru != "" {
			ru := personTranslation{Language: "ru"}
			ru.Data.Name = test.ru
			test.raw.Translations.Translations = []personTranslation{ru}
		}
		p := pickPerson(test.raw)
		if p.Name != test.name || p.OriginalName != test.original || p.Profession != test.profession {
			t.Errorf("%q: %q, %q, %q", test.raw.Name, p.Name, p.OriginalName, p.Profession)
		}
	}
}

func TestPersonLinks(t *testing.T) {
	links := PersonDetails{ID: 819, IMDb: "nm0001570", Kinopoisk: "4434", Wikipedia: "https://ru.wikipedia.org/wiki/x"}.Links()
	var got []string
	for _, l := range links {
		got = append(got, l.Site+" "+l.URL)
	}
	want := "tmdb https://www.themoviedb.org/person/819\nkinopoisk https://www.kinopoisk.ru/name/4434/\n" +
		"imdb https://www.imdb.com/name/nm0001570/\nwikipedia https://ru.wikipedia.org/wiki/x"
	if strings.Join(got, "\n") != want {
		t.Errorf("links\n%s", strings.Join(got, "\n"))
	}
}

func TestAsSelf(t *testing.T) {
	for character, want := range map[string]bool{
		"Self": true, "Self - Host": true, "Himself (archive footage)": true, "Сам себя": true, "Себя": true,
		"Selfie": false, "Самурай": false, "Narrator": false, "": false,
	} {
		if asSelf.MatchString(character) != want {
			t.Errorf("%q: %t", character, !want)
		}
	}
}

func TestBiography(t *testing.T) {
	for in, want := range map[string]string{
		"💥 **Робин Танни** — американская актриса, родившаяся **19 июня 1972 года в Чикаго, США 🇺🇸**. Играет в покер.\r\n\r\n" +
			"Фильмография - :  🎬 Магазин «Империя» (1995)\n\n🎬 Колдовство (1996)\n\nНаграды - : 🏆 Кубок Вольпи": "Робин Танни — американская актриса, родившаяся 19 июня 1972 года в Чикаго, США. Играет в покер.",
		"Edward Harrison Norton is an American actor.\n\nHe was born in Boston.":                "Edward Harrison Norton is an American actor.\n\nHe was born in Boston.",
		"Родилась в 1990-х, № 1 в рейтинге, 25°C.\n\nРоли в кино и на ТВ сделали её известной.": "Родилась в 1990-х, № 1 в рейтинге, 25°C.\n\nРоли в кино и на ТВ сделали её известной.",
		"🎬 Избранная фильмография:\n\n🎬 Фильм":                                                  "",
	} {
		if got := biography(in); got != want {
			t.Errorf("%q:\n%q\nwant\n%q", in, got, want)
		}
	}
}
