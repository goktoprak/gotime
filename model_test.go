package main

import "testing"

func TestMaskingKeepsOnlyTheLastFourCharacters(t *testing.T) {
	if got := maskAPIKey("abcdef1234567890"); got != "************7890" {
		t.Errorf("mask = %q", got)
	}
}

func TestMaskingHidesEverythingForShortKeys(t *testing.T) {
	for key, want := range map[string]string{"": "", "a": "*", "abcd": "****"} {
		if got := maskAPIKey(key); got != want {
			t.Errorf("mask(%q) = %q, want %q", key, got, want)
		}
	}
}

// The masking arithmetic counts runes, not bytes: a multi-byte key must not
// be sliced mid-character.
func TestMaskingCountsRunesNotBytes(t *testing.T) {
	for key, want := range map[string]string{
		"áéíóúab": "***óúab",
		"🔑🔑🔑🔑🔑":   "*🔑🔑🔑🔑",
		// Exactly four characters but sixteen bytes - masked entirely.
		"🔑🔑🔑🔑": "****",
	} {
		if got := maskAPIKey(key); got != want {
			t.Errorf("mask(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestAiringStatusesAreRecognised(t *testing.T) {
	for _, status := range []string{"Returning Series", "In Production", "Planned", "Pilot"} {
		if !tmdbStatusIsAiring(status) {
			t.Errorf("%q should count as airing", status)
		}
	}
}

func TestFinishedAndUnknownStatusesAreNotAiring(t *testing.T) {
	for _, status := range []string{"Ended", "Canceled", "Cancelled", "", "returning series"} {
		if tmdbStatusIsAiring(status) {
			t.Errorf("%q should not count as airing", status)
		}
	}
}

func TestDeriveCategory(t *testing.T) {
	cases := []struct {
		name           string
		total, watched int64
		tmdbStatus     string
		want           Category
	}{
		{"nothing watched is the watch list", 5, 0, "Ended", Watchlist},
		{"part way through is watching", 5, 2, "Ended", Watching},
		{"all watched and still airing is ongoing", 5, 5, "Returning Series", Ongoing},
		{"all watched and ended is finished", 5, 5, "Ended", Finished},
		{"all watched with no status is finished", 5, 5, "", Finished},
		{"a new episode reopens a finished show", 6, 5, "Ended", Watching},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveCategory(c.total, c.watched, c.tmdbStatus)
			if got != c.want {
				t.Errorf("category = %q, want %q", got, c.want)
			}
		})
	}
}

// The stored form is load-bearing in three places: the database column, the
// ?tab= query parameter, and the CSS class on the badge.
func TestCategoriesKeepTheirStoredSpelling(t *testing.T) {
	want := []string{"watching", "ongoing", "watchlist", "finished"}
	for i, category := range AllCategories {
		if category.String() != want[i] {
			t.Errorf("tab %d is %q, want %q", i, category, want[i])
		}
	}
}

func TestEveryCategoryRoundTripsThroughItsStoredForm(t *testing.T) {
	for _, category := range AllCategories {
		got, ok := ParseCategory(category.String())
		if !ok || got != category {
			t.Errorf("parse(%q) = %q, %v", category, got, ok)
		}
	}
}

func TestAnUnrecognisedCategoryDoesNotParse(t *testing.T) {
	for _, s := range []string{"", "Watching", "abandoned"} {
		if _, ok := ParseCategory(s); ok {
			t.Errorf("%q should not parse as a category", s)
		}
	}
}

func TestSeasonAndEpisodeFallBackToSomethingRenderable(t *testing.T) {
	unnamed := SeasonDetail{Season: Season{SeasonNumber: 2}}
	if got := unnamed.DisplayName(); got != "Season 2" {
		t.Errorf("season name = %q, want %q", got, "Season 2")
	}
	named := SeasonDetail{Season: Season{SeasonNumber: 2, Name: "The Second"}}
	if got := named.DisplayName(); got != "The Second" {
		t.Errorf("season name = %q", got)
	}
	if got := (Episode{}).DisplayName(); got != "Untitled" {
		t.Errorf("episode name = %q, want %q", got, "Untitled")
	}
}

func TestImageURLsAreEmptyWhenTMDBHasNoImage(t *testing.T) {
	if got := posterURL(""); got != "" {
		t.Errorf("poster = %q, want empty", got)
	}
	if got := posterURL("/abc.jpg"); got != "https://image.tmdb.org/t/p/w500/abc.jpg" {
		t.Errorf("poster = %q", got)
	}
	if got := backdropURL("/abc.jpg"); got != "https://image.tmdb.org/t/p/w780/abc.jpg" {
		t.Errorf("backdrop = %q", got)
	}
}
