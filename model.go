package main

import (
	"strconv"
	"strings"
)

// Category is which shelf a Show currently sits on. Always derived from
// watched progress and the show's production status, never chosen by the
// user, and only ever computed on the server.
//
// The values are the strings stored in the `category` column, and they
// double as the CSS modifier on `.status-badge` and `.tab-btn`.
type Category string

const (
	Watchlist Category = "watchlist"
	Watching  Category = "watching"
	Ongoing   Category = "ongoing"
	Finished  Category = "finished"
)

// AllCategories lists every category, in the order the dashboard shows its
// tabs.
var AllCategories = []Category{Watching, Ongoing, Watchlist, Finished}

// Label is how the category is written in the UI.
func (c Category) Label() string {
	switch c {
	case Watchlist:
		return "Watch List"
	case Watching:
		return "Watching"
	case Ongoing:
		return "Ongoing"
	case Finished:
		return "Finished"
	}
	return string(c)
}

// String is the stored and serialised form, used as a CSS class and in the
// ?tab= query parameter.
func (c Category) String() string { return string(c) }

// ParseCategory reads the stored form back. Anything else - a hand-edited
// database, a mangled URL, a stale cookie - is not a category.
func ParseCategory(s string) (Category, bool) {
	for _, c := range AllCategories {
		if string(c) == s {
			return c, true
		}
	}
	return "", false
}

// Show is one tracked TV series, mirroring the `shows` table.
type Show struct {
	ID       int64
	TMDBID   int64
	Name     string
	Overview string
	// PosterPath and BackdropPath are TMDB's bare path fragments; the size
	// segment is chosen per use site by PosterURL / BackdropURL.
	PosterPath   string
	BackdropPath string
	// TMDBStatus is TMDB's own production status: "Returning Series",
	// "Ended", and so on. Mirrored verbatim; only interpreted to split
	// Ongoing from Finished.
	TMDBStatus      string
	Category        Category
	AddedAt         string
	LastRefreshedAt string
}

// Season is one numbered run of episodes, mirroring the `seasons` table.
type Season struct {
	ID           int64
	ShowID       int64
	SeasonNumber int64
	Name         string
	EpisodeCount int64
}

// Episode mirrors the `episodes` table. Watched is the only fact in the
// system the user authors directly.
type Episode struct {
	ID            int64
	SeasonID      int64
	EpisodeNumber int64
	Name          string
	AirDate       string
	Watched       bool
	WatchedAt     string
}

// SeasonDetail is a season together with its episodes, as the show page
// renders it.
type SeasonDetail struct {
	Season
	Episodes []Episode
}

// ShowDetail is everything the show page needs in one value.
type ShowDetail struct {
	Show
	Seasons []SeasonDetail
}

// WatchUpdate is what a watch mutation reports back: the Show, whose
// Category the mutation may have moved, and the Seasons it touched.
//
// The Season is the unit. Ticking one Episode reports its whole Season
// rather than the single row, so the three watch actions differ only in how
// many Seasons they carry and one render path serves all of them.
//
// Show is a named field rather than embedded, unlike ShowDetail: the two
// look alike but mean different things, and Seasons here is what changed,
// not what exists.
type WatchUpdate struct {
	Show    Show
	Seasons []SeasonDetail
}

// DisplayName falls back to the season number: TMDB doesn't always name a
// season.
func (s SeasonDetail) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return "Season " + strconv.FormatInt(s.SeasonNumber, 10)
}

// WatchedCount and TotalCount are the season's progress, as the season
// header reads it: "3 / 10 watched".
func (s SeasonDetail) WatchedCount() int {
	watched := 0
	for _, e := range s.Episodes {
		if e.Watched {
			watched++
		}
	}
	return watched
}

func (s SeasonDetail) TotalCount() int { return len(s.Episodes) }

// DisplayName gives an untitled episode something to render.
func (e Episode) DisplayName() string {
	if e.Name != "" {
		return e.Name
	}
	return "Untitled"
}

// tmdbStatusIsAiring maps a raw TMDB show status into whether the show is
// still actively producing new content ("airing") or is done. An unknown or
// missing status is not airing: a show can only be Ongoing on a positive
// signal.
func tmdbStatusIsAiring(tmdbStatus string) bool {
	switch tmdbStatus {
	case "Returning Series", "In Production", "Planned", "Pilot":
		return true
	}
	return false
}

// deriveCategory applies the category rules to one show's counts.
//
//   - no episodes watched yet -> Watchlist
//   - some episodes watched, but not all -> Watching
//   - all episodes watched:
//   - TMDB production status still airing/upcoming -> Ongoing
//   - TMDB production status ended/canceled        -> Finished
//
// A show previously Ongoing or Finished that gains new unwatched episodes
// (a new season dropped) falls back to Watching on its own, since the rules
// above already produce that result.
//
// `total` and `watched` exclude specials - see recomputeCategory, which is
// also where a show with no episode data at all is dealt with. That case
// stops there and never reaches this function, so there is no answer here
// meaning "decide nothing".
func deriveCategory(total, watched int64, tmdbStatus string) Category {
	switch {
	case watched == 0:
		return Watchlist
	case watched < total:
		return Watching
	case tmdbStatusIsAiring(tmdbStatus):
		return Ongoing
	default:
		return Finished
	}
}

// maskAPIKey builds the display version of an API key: all but the last four
// characters replaced with asterisks. Very short keys (<= 4 characters,
// which shouldn't happen with a real TMDB key) are masked entirely.
//
// Counts runes, not bytes, so a multi-byte key isn't sliced mid-character.
func maskAPIKey(key string) string {
	runes := []rune(key)
	if len(runes) <= 4 {
		return strings.Repeat("*", len(runes))
	}
	return strings.Repeat("*", len(runes)-4) + string(runes[len(runes)-4:])
}

// posterURL and backdropURL build TMDB image URLs from the bare path
// fragments TMDB returns. Empty means the show has no image, and the
// templates render a placeholder instead.
func posterURL(path string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w500" + path
}

func backdropURL(path string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w780" + path
}
