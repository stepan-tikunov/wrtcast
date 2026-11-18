package tmdb

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

// image is a picture of a title as TMDB lists them.
type image struct {
	FilePath    string  `json:"file_path"`
	Language    *string `json:"iso_639_1"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	AspectRatio float64 `json:"aspect_ratio"`
	VoteAverage float64 `json:"vote_average"`
	VoteCount   int     `json:"vote_count"`
}

type video struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Site        string `json:"site"`
	Type        string `json:"type"`
	Language    string `json:"iso_639_1"`
	Official    bool   `json:"official"`
	PublishedAt string `json:"published_at"`
}

type castMember struct {
	ID          uint64 `json:"id"`
	Name        string `json:"name"`
	ProfilePath string `json:"profile_path"`
	// A movie's.
	Character string `json:"character"`
	// A series' (aggregate_credits).
	Roles []struct {
		Character string `json:"character"`
	} `json:"roles"`
}

type crewMember struct {
	ID          uint64 `json:"id"`
	Name        string `json:"name"`
	ProfilePath string `json:"profile_path"`
	Job         string `json:"job"`
}

type episodeRef struct {
	SeasonNumber  int32  `json:"season_number"`
	EpisodeNumber int32  `json:"episode_number"`
	AirDate       string `json:"air_date"`
	Name          string `json:"name"`
}

// related is a title in a list: recommendations, a collection's parts.
type related struct {
	ID           uint64 `json:"id"`
	MediaType    string `json:"media_type"`
	Title        string `json:"title"`
	Name         string `json:"name"`
	ReleaseDate  string `json:"release_date"`
	FirstAirDate string `json:"first_air_date"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
	Adult        bool   `json:"adult"`
}

// rawDetails is a movie or a TV show as /movie/{id} and /tv/{id} have it,
// with what is appended to them.
type rawDetails struct {
	item
	Tagline      string  `json:"tagline"`
	BackdropPath string  `json:"backdrop_path"`
	PosterPath   string  `json:"poster_path"`
	VoteAverage  float64 `json:"vote_average"`
	Runtime      uint32  `json:"runtime"`
	Genres       []struct {
		Name string `json:"name"`
	} `json:"genres"`
	ProductionCountries []struct {
		Code string `json:"iso_3166_1"`
		Name string `json:"name"`
	} `json:"production_countries"`
	BelongsToCollection *struct {
		ID uint64 `json:"id"`
	} `json:"belongs_to_collection"`

	// A series'.
	Status    string      `json:"status"`
	Next      *episodeRef `json:"next_episode_to_air"`
	Last      *episodeRef `json:"last_episode_to_air"`
	Seasons   int32       `json:"number_of_seasons"`
	CreatedBy []struct {
		ID          uint64 `json:"id"`
		Name        string `json:"name"`
		ProfilePath string `json:"profile_path"`
	} `json:"created_by"`
	SeasonList []struct {
		SeasonNumber int32  `json:"season_number"`
		EpisodeCount int32  `json:"episode_count"`
		AirDate      string `json:"air_date"`
		Name         string `json:"name"`
	} `json:"seasons"`

	Images struct {
		Backdrops []image `json:"backdrops"`
		Logos     []image `json:"logos"`
	} `json:"images"`
	Videos struct {
		Results []video `json:"results"`
	} `json:"videos"`
	Credits struct {
		Cast []castMember `json:"cast"`
		Crew []crewMember `json:"crew"`
	} `json:"credits"`
	AggregateCredits struct {
		Cast []castMember `json:"cast"`
	} `json:"aggregate_credits"`
	ReleaseDates struct {
		Results []struct {
			Country      string `json:"iso_3166_1"`
			ReleaseDates []struct {
				Certification string `json:"certification"`
			} `json:"release_dates"`
		} `json:"results"`
	} `json:"release_dates"`
	ContentRatings struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Rating  string `json:"rating"`
		} `json:"results"`
	} `json:"content_ratings"`
	Recommendations struct {
		Results []related `json:"results"`
	} `json:"recommendations"`
}

// details is a title on TMDB in Russian, with its pictures, videos, cast,
// age ratings and recommendations, in one request.
func (c *client) details(ctx context.Context, kind string, id uint64) (rawDetails, error) {
	appended := "images,videos,credits,release_dates,recommendations"
	if kind == KindTV {
		appended = "images,videos,aggregate_credits,content_ratings,recommendations"
	}
	var result rawDetails
	err := c.get(ctx, "/"+kind+"/"+strconv.FormatUint(id, 10), url.Values{
		"language":               {"ru-RU"},
		"append_to_response":     {appended},
		"include_image_language": {"ru,en,null"},
		"include_video_language": {"ru,en"},
	}, &result)
	return result, err
}

type rawEpisode struct {
	EpisodeNumber int32  `json:"episode_number"`
	Name          string `json:"name"`
	Overview      string `json:"overview"`
	StillPath     string `json:"still_path"`
	AirDate       string `json:"air_date"`
	Runtime       uint32 `json:"runtime"`
}

type rawSeason struct {
	Episodes []rawEpisode `json:"episodes"`
}

func (c *client) seasonIn(ctx context.Context, id uint64, number int32, language string) (rawSeason, error) {
	var result rawSeason
	err := c.get(ctx, "/tv/"+strconv.FormatUint(id, 10)+"/season/"+strconv.Itoa(int(number)),
		url.Values{"language": {language}}, &result)
	return result, err
}

// season is a season of a series in Russian, an episode aired with no
// overview in Russian given the English one; TMDB not having the season
// is no season, without an error.
func (c *client) season(ctx context.Context, id uint64, number int32, today string) (rawSeason, error) {
	result, err := c.seasonIn(ctx, id, number, "ru-RU")
	if errors.Is(err, errNotFound) {
		return rawSeason{}, nil
	}
	if err != nil {
		return result, err
	}
	missing := false
	for _, episode := range result.Episodes {
		if episode.Overview == "" && !upcoming(episode.AirDate, today) {
			missing = true
		}
	}
	if !missing {
		return result, nil
	}
	english, err := c.seasonIn(ctx, id, number, "en-US")
	if err != nil {
		return result, err
	}
	overviews := map[int32]string{}
	for _, episode := range english.Episodes {
		overviews[episode.EpisodeNumber] = episode.Overview
	}
	for i, episode := range result.Episodes {
		if episode.Overview == "" {
			result.Episodes[i].Overview = overviews[episode.EpisodeNumber]
		}
	}
	return result, nil
}

type rawCollection struct {
	ID    uint64    `json:"id"`
	Name  string    `json:"name"`
	Parts []related `json:"parts"`
}

func (c *client) collection(ctx context.Context, id uint64) (rawCollection, error) {
	var result rawCollection
	err := c.get(ctx, "/collection/"+strconv.FormatUint(id, 10), url.Values{"language": {"ru-RU"}}, &result)
	return result, err
}

// countries are the names of countries in Russian by their codes.
func (c *client) countries(ctx context.Context) (map[string]string, error) {
	var result []struct {
		Code   string `json:"iso_3166_1"`
		Native string `json:"native_name"`
	}
	if err := c.get(ctx, "/configuration/countries", url.Values{"language": {"ru-RU"}}, &result); err != nil {
		return nil, err
	}
	names := make(map[string]string, len(result))
	for _, country := range result {
		if country.Native != "" {
			names[country.Code] = country.Native
		}
	}
	return names, nil
}
