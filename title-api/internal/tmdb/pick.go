package tmdb

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// The statuses of a series.
const (
	StatusReturning = "returning"
	StatusEnded     = "ended"
	StatusCanceled  = "canceled"
	StatusOther     = "other"
)

// How many people, videos and recommendations a title's page shows at most.
const (
	castLimit    = 12
	videoLimit   = 4
	relatedLimit = 12
)

// Details is what a title's page shows of a title on TMDB, chosen from
// what TMDB has; what it lacks is empty. Pictures are files of
// /v1/tmdb/images, without a leading slash.
type Details struct {
	ID           uint64  `json:"id"`
	Kind         string  `json:"kind"`
	Name         string  `json:"name"`
	OriginalName string  `json:"original_name"`
	Date         string  `json:"date,omitempty"`
	Tagline      string  `json:"tagline,omitempty"`
	Overview     string  `json:"overview,omitempty"`
	Backdrop     string  `json:"backdrop,omitempty"`
	Logo         string  `json:"logo,omitempty"`
	LogoAspect   float64 `json:"logo_aspect,omitempty"`
	Poster       string  `json:"poster,omitempty"`
	// In the Russian scale: 0+, 6+, 12+, 16+, 18+.
	Age         string   `json:"age,omitempty"`
	VoteAverage float64  `json:"vote_average,omitempty"`
	Runtime     uint32   `json:"runtime,omitempty"`
	Genres      []string `json:"genres,omitempty"`
	// Codes as kept, their Russian names as served.
	Countries       []string  `json:"countries,omitempty"`
	Directors       []string  `json:"directors,omitempty"`
	Cast            []Person  `json:"cast,omitempty"`
	Videos          []Video   `json:"videos,omitempty"`
	CollectionID    uint64    `json:"collection_id,omitempty"`
	Recommendations []Related `json:"recommendations,omitempty"`
	Series          *Series   `json:"series,omitempty"`

	// As served: the collection's parts, and title-api's title, if known.
	Collection *Collection `json:"-"`
	TitleID    string      `json:"-"`
}

// Year is the year the title came out, if TMDB knows.
func (d Details) Year() string { return year(d.Date) }

// Series is what Details has of a series alone.
type Series struct {
	Status   string      `json:"status"`
	Next     *EpisodeRef `json:"next,omitempty"`
	Last     *EpisodeRef `json:"last,omitempty"`
	Seasons  int32       `json:"seasons"`
	Creators []string    `json:"creators,omitempty"`
	// The seasons, the specials (season 0) left out.
	SeasonList []SeasonRef `json:"season_list,omitempty"`
}

type EpisodeRef struct {
	Season  int32  `json:"season"`
	Episode int32  `json:"episode"`
	AirDate string `json:"air_date,omitempty"`
	Name    string `json:"name,omitempty"`
}

type SeasonRef struct {
	Number   int32  `json:"number"`
	Episodes int32  `json:"episodes"`
	AirDate  string `json:"air_date,omitempty"`
	Name     string `json:"name,omitempty"`
}

type Person struct {
	Name      string `json:"name"`
	Character string `json:"character,omitempty"`
	Profile   string `json:"profile"`
}

// Video is a trailer or a teaser on YouTube.
type Video struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Language string `json:"language"`
	// Trailer or Teaser.
	Kind string `json:"kind"`
}

// Related is a title in a list: recommendations, a collection's parts.
type Related struct {
	ID       uint64 `json:"id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Date     string `json:"date,omitempty"`
	Poster   string `json:"poster,omitempty"`
	Backdrop string `json:"backdrop,omitempty"`
	// As served: title-api's title, if known.
	TitleID string `json:"-"`
}

// Year is the year the title came out, if TMDB knows.
func (r Related) Year() string { return year(r.Date) }

// Collection is a collection's parts in the order they came out, those
// with no date yet last.
type Collection struct {
	ID    uint64    `json:"id"`
	Name  string    `json:"name"`
	Parts []Related `json:"parts"`
}

// Season is a season's episodes; one TMDB doesn't have has none.
type Season struct {
	Episodes []Episode `json:"episodes"`
}

type Episode struct {
	Number   int32  `json:"number"`
	Name     string `json:"name,omitempty"`
	Overview string `json:"overview,omitempty"`
	Still    string `json:"still,omitempty"`
	AirDate  string `json:"air_date,omitempty"`
	Runtime  uint32 `json:"runtime,omitempty"`
	// Not aired yet: its date is to come or unknown.
	Upcoming bool `json:"upcoming,omitempty"`
}

func year(date string) string {
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}

// text is a text of TMDB with its lines ending in \n alone.
func text(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(value))
}

// file is a picture's path as /v1/tmdb/images takes it.
func file(path string) string { return strings.TrimPrefix(path, "/") }

// upcoming tells whether an episode or a title is yet to come out, by its
// date and today's, both as TMDB writes them (2006-01-02).
func upcoming(date, today string) bool { return date == "" || date > today }

func pickDetails(kind string, raw rawDetails) Details {
	result := Details{
		ID: raw.ID, Kind: kind, Name: raw.Title, OriginalName: raw.OriginalTitle, Date: raw.ReleaseDate,
		Tagline: text(raw.Tagline), Overview: text(raw.Overview),
		Backdrop: pickBackdrop(raw.Images.Backdrops), Poster: file(raw.PosterPath),
		Age: pickAge(kind, raw), VoteAverage: raw.VoteAverage, Runtime: raw.Runtime,
		Cast: pickCast(kind, raw), Videos: pickVideos(raw.Videos.Results),
		Recommendations: pickRelated(kind, raw.Recommendations.Results),
	}
	if kind == KindTV {
		result.Name, result.OriginalName, result.Date = raw.Name, raw.OriginalName, raw.FirstAirDate
	}
	result.Logo, result.LogoAspect = pickLogo(raw.Images.Logos)
	for _, genre := range raw.Genres {
		result.Genres = append(result.Genres, genre.Name)
	}
	for _, country := range raw.ProductionCountries {
		result.Countries = append(result.Countries, country.Code)
	}
	if len(result.Countries) == 0 {
		result.Countries = raw.OriginCountry
	}
	for _, member := range raw.Credits.Crew {
		if member.Job == "Director" && !slices.Contains(result.Directors, member.Name) {
			result.Directors = append(result.Directors, member.Name)
		}
	}
	if raw.BelongsToCollection != nil {
		result.CollectionID = raw.BelongsToCollection.ID
	}
	if kind == KindTV {
		result.Series = pickSeries(raw)
	}
	return result
}

func pickSeries(raw rawDetails) *Series {
	series := &Series{Status: seriesStatus(raw.Status), Seasons: raw.Seasons}
	ref := func(raw *episodeRef) *EpisodeRef {
		if raw == nil {
			return nil
		}
		return &EpisodeRef{Season: raw.SeasonNumber, Episode: raw.EpisodeNumber, AirDate: raw.AirDate, Name: episodeName(raw.Name)}
	}
	series.Next, series.Last = ref(raw.Next), ref(raw.Last)
	for _, creator := range raw.CreatedBy {
		series.Creators = append(series.Creators, creator.Name)
	}
	for _, season := range raw.SeasonList {
		if season.SeasonNumber > 0 {
			series.SeasonList = append(series.SeasonList, SeasonRef{
				Number: season.SeasonNumber, Episodes: season.EpisodeCount, AirDate: season.AirDate, Name: season.Name,
			})
		}
	}
	return series
}

func seriesStatus(status string) string {
	switch status {
	case "Returning Series":
		return StatusReturning
	case "Ended":
		return StatusEnded
	case "Canceled":
		return StatusCanceled
	default:
		return StatusOther
	}
}

// pickBackdrop is the best backdrop without text: of those in no language,
// at least 1280 wide first, then the best voted; else any.
func pickBackdrop(backdrops []image) string {
	var plain []image
	for _, backdrop := range backdrops {
		if backdrop.Language == nil || *backdrop.Language == "" {
			plain = append(plain, backdrop)
		}
	}
	if len(plain) == 0 {
		plain = backdrops
	}
	if len(plain) == 0 {
		return ""
	}
	best := slices.MinFunc(plain, func(a, b image) int {
		return cmp.Or(
			-cmp.Compare(boolInt(a.Width >= 1280), boolInt(b.Width >= 1280)),
			-cmp.Compare(a.VoteAverage, b.VoteAverage),
			-cmp.Compare(a.VoteCount, b.VoteCount),
		)
	})
	return file(best.FilePath)
}

// pickLogo is the best voted PNG logo, in Russian, else English, else in
// no language, and its width to its height.
func pickLogo(logos []image) (string, float64) {
	var best *image
	rank := func(logo image) int {
		switch {
		case logo.Language == nil || *logo.Language == "":
			return 2
		case *logo.Language == "ru":
			return 0
		case *logo.Language == "en":
			return 1
		default:
			return 3
		}
	}
	for i, logo := range logos {
		if !strings.HasSuffix(logo.FilePath, ".png") || rank(logo) == 3 {
			continue
		}
		if best == nil || cmp.Or(cmp.Compare(rank(logo), rank(*best)), -cmp.Compare(logo.VoteAverage, best.VoteAverage)) < 0 {
			best = &logos[i]
		}
	}
	if best == nil {
		return "", 0
	}
	aspect := best.AspectRatio
	if aspect == 0 && best.Height > 0 {
		aspect = float64(best.Width) / float64(best.Height)
	}
	return file(best.FilePath), aspect
}

// usAges are the American ratings in the Russian scale.
var usAges = map[string]string{
	"G": "0+", "TV-Y": "0+", "TV-G": "0+",
	"PG": "6+", "TV-Y7": "6+",
	"PG-13": "12+", "TV-PG": "12+",
	"TV-14": "16+",
	"R":     "18+", "NC-17": "18+", "TV-MA": "18+",
}

var ruAge = regexp.MustCompile(`^(0|6|12|16|18)\+?$`)

// pickAge is the title's Russian age rating, else its American one in the
// Russian scale.
func pickAge(kind string, raw rawDetails) string {
	ratings := map[string]string{}
	if kind == KindTV {
		for _, rating := range raw.ContentRatings.Results {
			if rating.Rating != "" && ratings[rating.Country] == "" {
				ratings[rating.Country] = strings.TrimSpace(rating.Rating)
			}
		}
	} else {
		for _, country := range raw.ReleaseDates.Results {
			for _, release := range country.ReleaseDates {
				if release.Certification != "" && ratings[country.Country] == "" {
					ratings[country.Country] = strings.TrimSpace(release.Certification)
				}
			}
		}
	}
	if match := ruAge.FindStringSubmatch(ratings["RU"]); match != nil {
		return match[1] + "+"
	}
	return usAges[ratings["US"]]
}

// pickVideos are the trailers and teasers on YouTube: Russian first, the
// official ones first, trailers before teasers, the latest first.
func pickVideos(videos []video) []Video {
	var found []video
	for _, v := range videos {
		if v.Site == "YouTube" && v.Key != "" && (v.Type == "Trailer" || v.Type == "Teaser") {
			found = append(found, v)
		}
	}
	slices.SortStableFunc(found, func(a, b video) int {
		return cmp.Or(
			-cmp.Compare(boolInt(a.Language == "ru"), boolInt(b.Language == "ru")),
			-cmp.Compare(boolInt(a.Official), boolInt(b.Official)),
			-cmp.Compare(boolInt(a.Type == "Trailer"), boolInt(b.Type == "Trailer")),
			-cmp.Compare(a.PublishedAt, b.PublishedAt),
		)
	})
	var result []Video
	for _, v := range found[:min(len(found), videoLimit)] {
		result = append(result, Video{Key: v.Key, Name: v.Name, Language: v.Language, Kind: v.Type})
	}
	return result
}

// pickCast are the first people billed who have a photo.
func pickCast(kind string, raw rawDetails) []Person {
	cast := raw.Credits.Cast
	if kind == KindTV {
		cast = raw.AggregateCredits.Cast
	}
	var result []Person
	for _, member := range cast {
		if member.ProfilePath == "" {
			continue
		}
		character := member.Character
		if len(member.Roles) > 0 {
			character = member.Roles[0].Character
		}
		result = append(result, Person{Name: member.Name, Character: strings.TrimSpace(character), Profile: file(member.ProfilePath)})
		if len(result) == castLimit {
			break
		}
	}
	return result
}

func relatedOf(kind string, raw related) Related {
	if raw.MediaType == KindMovie || raw.MediaType == KindTV {
		kind = raw.MediaType
	}
	result := Related{
		ID: raw.ID, Kind: kind, Name: raw.Title, Date: raw.ReleaseDate,
		Poster: file(raw.PosterPath), Backdrop: file(raw.BackdropPath),
	}
	if kind == KindTV {
		result.Name, result.Date = raw.Name, raw.FirstAirDate
	}
	return result
}

// pickRelated are the first recommendations with a poster.
func pickRelated(kind string, recommendations []related) []Related {
	var result []Related
	for _, raw := range recommendations {
		if raw.Adult || raw.PosterPath == "" {
			continue
		}
		result = append(result, relatedOf(kind, raw))
		if len(result) == relatedLimit {
			break
		}
	}
	return result
}

func pickCollection(raw rawCollection) Collection {
	result := Collection{ID: raw.ID, Name: raw.Name, Parts: []Related{}}
	for _, part := range raw.Parts {
		if !part.Adult {
			result.Parts = append(result.Parts, relatedOf(KindMovie, part))
		}
	}
	slices.SortStableFunc(result.Parts, func(a, b Related) int {
		return cmp.Or(cmp.Compare(boolInt(a.Date == ""), boolInt(b.Date == "")), cmp.Compare(a.Date, b.Date))
	})
	return result
}

func pickSeason(raw rawSeason, today string) Season {
	result := Season{Episodes: []Episode{}}
	for _, episode := range raw.Episodes {
		result.Episodes = append(result.Episodes, Episode{
			Number: episode.EpisodeNumber, Name: episodeName(episode.Name), Overview: text(episode.Overview),
			Still: file(episode.StillPath), AirDate: episode.AirDate, Runtime: episode.Runtime,
			Upcoming: upcoming(episode.AirDate, today),
		})
	}
	return result
}

var numbered = regexp.MustCompile(`(?i)^(серия|эпизод|episode)\s*\d+$`)

// episodeName is an episode's name, unless it is but its number ("Серия 3")
// or TMDB has it in no Russian: its letters are none of them Cyrillic.
func episodeName(name string) string {
	name = strings.TrimSpace(name)
	if numbered.MatchString(name) {
		return ""
	}
	letters, cyrillic := false, false
	for _, r := range name {
		if unicode.IsLetter(r) {
			letters = true
			cyrillic = cyrillic || unicode.Is(unicode.Cyrillic, r)
		}
	}
	if letters && !cyrillic {
		return ""
	}
	return name
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// How long what TMDB has of a title is kept before it is asked again: a
// day for what changes, a running series or a title out within a year,
// a month for the rest.
const (
	shortTTL = 24 * time.Hour
	longTTL  = 30 * 24 * time.Hour
)

// recent tells whether a date is within a year of now, or to come, or
// unknown.
func recent(date string, now time.Time) bool {
	return date == "" || date > now.AddDate(-1, 0, 0).Format(time.DateOnly)
}

func (d Details) ttl(now time.Time) time.Duration {
	if d.Series != nil && d.Series.Status == StatusReturning || recent(d.Date, now) {
		return shortTTL
	}
	return longTTL
}

func (s Season) ttl(now time.Time) time.Duration {
	// TMDB may add a season it doesn't have yet.
	if len(s.Episodes) == 0 {
		return shortTTL
	}
	for _, episode := range s.Episodes {
		if episode.Upcoming {
			return shortTTL
		}
	}
	return longTTL
}

func (c Collection) ttl(now time.Time) time.Duration {
	for _, part := range c.Parts {
		if recent(part.Date, now) {
			return shortTTL
		}
	}
	return longTTL
}
