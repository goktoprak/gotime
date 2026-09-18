package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// Every one of these runs against a real database file, which is the
// point: the rules live in SQL as much as in Go.

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func ctx() context.Context { return context.Background() }

// seedShow inserts a show directly, bypassing TMDB.
func seedShow(t *testing.T, s *Store, tmdbStatus string, category Category) int64 {
	t.Helper()
	res, err := s.db.Exec(
		`INSERT INTO shows (tmdb_id, name, tmdb_status, category, added_at)
		 VALUES (abs(random()) % 1000000, 'Test Show', ?, ?, '2024-01-01T00:00:00Z')`,
		null(tmdbStatus), string(category))
	if err != nil {
		t.Fatalf("seeding show: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("seeding show: %v", err)
	}
	return id
}

// addSeason adds a season with `episodes` unwatched episodes, numbered from 1.
func addSeason(t *testing.T, s *Store, showID, number int64, episodes int) int64 {
	t.Helper()
	res, err := s.db.Exec(
		`INSERT INTO seasons (show_id, tmdb_season_number, name, episode_count)
		 VALUES (?, ?, ?, ?)`, showID, number, "Season", episodes)
	if err != nil {
		t.Fatalf("seeding season: %v", err)
	}
	seasonID, _ := res.LastInsertId()
	for i := 1; i <= episodes; i++ {
		if _, err := s.db.Exec(
			`INSERT INTO episodes (season_id, tmdb_episode_number, name, watched)
			 VALUES (?, ?, ?, 0)`, seasonID, i, "Episode"); err != nil {
			t.Fatalf("seeding episode: %v", err)
		}
	}
	return seasonID
}

// watch marks the first `count` episodes of a season watched.
func watch(t *testing.T, s *Store, seasonID int64, count int) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE episodes SET watched = 1, watched_at = '2024-01-01T00:00:00Z'
		 WHERE id IN (SELECT id FROM episodes WHERE season_id = ?
		              ORDER BY tmdb_episode_number LIMIT ?)`, seasonID, count); err != nil {
		t.Fatalf("marking watched: %v", err)
	}
}

// recompute runs the category rules outside any mutation, the way a
// mutation would.
func recompute(t *testing.T, s *Store, showID int64) {
	t.Helper()
	if err := s.inTx(ctx(), func(tx *sql.Tx) error {
		return recomputeCategory(ctx(), tx, showID)
	}); err != nil {
		t.Fatalf("recomputing: %v", err)
	}
}

func categoryOf(t *testing.T, s *Store, showID int64) Category {
	t.Helper()
	show, err := s.Show(ctx(), showID)
	if err != nil {
		t.Fatalf("reading show: %v", err)
	}
	return show.Category
}

func watchedAtOf(t *testing.T, s *Store, episodeID int64) string {
	t.Helper()
	var watchedAt sql.NullString
	if err := s.db.QueryRow(
		`SELECT watched_at FROM episodes WHERE id = ?`, episodeID).Scan(&watchedAt); err != nil {
		t.Fatalf("reading episode: %v", err)
	}
	return watchedAt.String
}

func episodeIDs(t *testing.T, s *Store, seasonID int64) []int64 {
	t.Helper()
	season, err := loadSeasonDetail(ctx(), s.db, seasonID)
	if err != nil {
		t.Fatalf("reading season: %v", err)
	}
	var ids []int64
	for _, e := range season.Episodes {
		ids = append(ids, e.ID)
	}
	return ids
}

// countEpisodes counts every episode row under a show, including ones no
// season of the show still points at - which is the whole point of asking.
func countEpisodes(t *testing.T, s *Store, showID int64) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM episodes e JOIN seasons s ON e.season_id = s.id
		 WHERE s.show_id = ?`, showID).Scan(&count); err != nil {
		t.Fatalf("counting episodes: %v", err)
	}
	return count
}

func fetched(tmdbStatus string, seasons ...FetchedSeason) *FetchedShow {
	return &FetchedShow{
		TMDBID:     42,
		Name:       "Fetched Show",
		Overview:   "An overview.",
		TMDBStatus: tmdbStatus,
		Seasons:    seasons,
	}
}

func fetchedSeason(number int64, episodes int) FetchedSeason {
	season := FetchedSeason{
		Number:       number,
		Name:         "Season",
		EpisodeCount: int64(episodes),
	}
	for i := 1; i <= episodes; i++ {
		season.Episodes = append(season.Episodes, FetchedEpisode{
			Number:  int64(i),
			Name:    "Episode",
			AirDate: "2024-01-01",
		})
	}
	return season
}

// numberedSeason is fetchedSeason for the cases where the episode numbers
// themselves matter - a season TMDB renumbered does not start at 1.
func numberedSeason(number int64, episodeNumbers ...int64) FetchedSeason {
	season := FetchedSeason{
		Number:       number,
		Name:         "Season",
		EpisodeCount: int64(len(episodeNumbers)),
	}
	for _, n := range episodeNumbers {
		season.Episodes = append(season.Episodes, FetchedEpisode{
			Number:  n,
			Name:    "Episode",
			AirDate: "2024-01-01",
		})
	}
	return season
}

// ---------- category derivation ----------

func TestNoEpisodesAtAllLeavesTheCategoryAlone(t *testing.T) {
	// Guards the deliberate no-op: a refresh that hasn't repopulated
	// seasons yet must not reclassify the show off missing data. The guard
	// lives in recomputeCategory, and this is the only test of it -
	// deriveCategory has no say in the matter and never sees the case.
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Finished)

	recompute(t, s, showID)

	if got := categoryOf(t, s, showID); got != Finished {
		t.Errorf("category = %q, want %q", got, Finished)
	}
}

func TestEpisodesButNoneWatchedIsWatchlist(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Returning Series", Watching)
	addSeason(t, s, showID, 1, 3)

	recompute(t, s, showID)

	if got := categoryOf(t, s, showID); got != Watchlist {
		t.Errorf("category = %q, want %q", got, Watchlist)
	}
}

func TestSomeButNotAllWatchedIsWatching(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Returning Series", Watchlist)
	seasonID := addSeason(t, s, showID, 1, 3)
	watch(t, s, seasonID, 1)

	recompute(t, s, showID)

	if got := categoryOf(t, s, showID); got != Watching {
		t.Errorf("category = %q, want %q", got, Watching)
	}
}

func TestAllWatchedWhileStillAiringIsOngoing(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Returning Series", Watching)
	seasonID := addSeason(t, s, showID, 1, 2)
	watch(t, s, seasonID, 2)

	recompute(t, s, showID)

	if got := categoryOf(t, s, showID); got != Ongoing {
		t.Errorf("category = %q, want %q", got, Ongoing)
	}
}

func TestAllWatchedOnceEndedIsFinished(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watching)
	seasonID := addSeason(t, s, showID, 1, 2)
	watch(t, s, seasonID, 2)

	recompute(t, s, showID)

	if got := categoryOf(t, s, showID); got != Finished {
		t.Errorf("category = %q, want %q", got, Finished)
	}
}

func TestAllWatchedWithUnknownTMDBStatusIsFinished(t *testing.T) {
	// tmdb_status is nullable; a missing status must not be treated as
	// still airing.
	s := testStore(t)
	showID := seedShow(t, s, "", Watching)
	seasonID := addSeason(t, s, showID, 1, 1)
	watch(t, s, seasonID, 1)

	recompute(t, s, showID)

	if got := categoryOf(t, s, showID); got != Finished {
		t.Errorf("category = %q, want %q", got, Finished)
	}
}

func TestUnwatchedSpecialsDoNotHoldAShowBack(t *testing.T) {
	// Season 0 is excluded from the counts, so every regular episode being
	// watched is enough to finish the show.
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watching)
	addSeason(t, s, showID, 0, 5)
	seasonID := addSeason(t, s, showID, 1, 2)
	watch(t, s, seasonID, 2)

	recompute(t, s, showID)

	if got := categoryOf(t, s, showID); got != Finished {
		t.Errorf("category = %q, want %q", got, Finished)
	}
}

func TestWatchingOnlySpecialsDoesNotStartTheShow(t *testing.T) {
	// The mirror image: specials count for nothing in either direction.
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	specials := addSeason(t, s, showID, 0, 3)
	addSeason(t, s, showID, 1, 2)
	watch(t, s, specials, 3)

	recompute(t, s, showID)

	if got := categoryOf(t, s, showID); got != Watchlist {
		t.Errorf("category = %q, want %q", got, Watchlist)
	}
}

func TestUnwatchingEverythingReturnsToWatchlist(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	seasonID := addSeason(t, s, showID, 1, 1)
	episodeID := episodeIDs(t, s, seasonID)[0]

	if _, err := s.ToggleEpisode(ctx(), episodeID); err != nil {
		t.Fatalf("toggling on: %v", err)
	}
	if got := categoryOf(t, s, showID); got != Finished {
		t.Fatalf("after watching, category = %q, want %q", got, Finished)
	}

	if _, err := s.ToggleEpisode(ctx(), episodeID); err != nil {
		t.Fatalf("toggling off: %v", err)
	}
	if got := categoryOf(t, s, showID); got != Watchlist {
		t.Errorf("after unwatching, category = %q, want %q", got, Watchlist)
	}
}

func TestANewEpisodeDropsAnOngoingShowBackToWatching(t *testing.T) {
	// The behaviour the README promises after a metadata refresh.
	s := testStore(t)
	showID := seedShow(t, s, "Returning Series", Watching)
	seasonID := addSeason(t, s, showID, 1, 2)
	watch(t, s, seasonID, 2)
	recompute(t, s, showID)
	if got := categoryOf(t, s, showID); got != Ongoing {
		t.Fatalf("category = %q, want %q", got, Ongoing)
	}

	if _, err := s.db.Exec(
		`INSERT INTO episodes (season_id, tmdb_episode_number, watched) VALUES (?, 3, 0)`,
		seasonID); err != nil {
		t.Fatalf("adding episode: %v", err)
	}
	recompute(t, s, showID)

	if got := categoryOf(t, s, showID); got != Watching {
		t.Errorf("category = %q, want %q", got, Watching)
	}
}

// ---------- marking watched ----------

func TestTogglingAnEpisodeFlipsItAndRecomputesTheCategory(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	seasonID := addSeason(t, s, showID, 1, 2)
	episodeID := episodeIDs(t, s, seasonID)[0]

	update, err := s.ToggleEpisode(ctx(), episodeID)
	if err != nil {
		t.Fatalf("toggling: %v", err)
	}
	if update.Show.Category != Watching {
		t.Errorf("returned category = %q, want %q", update.Show.Category, Watching)
	}
	if watchedAtOf(t, s, episodeID) == "" {
		t.Error("watching an episode should record when")
	}

	// Unwatching clears the timestamp too.
	if _, err := s.ToggleEpisode(ctx(), episodeID); err != nil {
		t.Fatalf("toggling back: %v", err)
	}
	if got := watchedAtOf(t, s, episodeID); got != "" {
		t.Errorf("watched_at = %q after unwatching, want empty", got)
	}
}

func TestTogglingAnUnknownEpisodeIsNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.ToggleEpisode(ctx(), 999)
	if !isNotFound(err) {
		t.Errorf("err = %v, want not found", err)
	}
}

func TestMarkingASeasonAgainKeepsTheOriginalWatchTimes(t *testing.T) {
	// Regression: the update must not rewrite watched_at for episodes that
	// were already marked, which would destroy when they were watched.
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	seasonID := addSeason(t, s, showID, 1, 3)
	ids := episodeIDs(t, s, seasonID)

	if _, err := s.ToggleEpisode(ctx(), ids[0]); err != nil {
		t.Fatalf("toggling: %v", err)
	}
	original := watchedAtOf(t, s, ids[0])

	if _, err := s.MarkSeason(ctx(), seasonID); err != nil {
		t.Fatalf("marking season: %v", err)
	}

	if got := watchedAtOf(t, s, ids[0]); got != original {
		t.Errorf("watched_at = %q, want the original %q", got, original)
	}
	if got := watchedAtOf(t, s, ids[2]); got == "" {
		t.Error("an episode marked by the season should have a watch time")
	}
	if got := categoryOf(t, s, showID); got != Finished {
		t.Errorf("category = %q, want %q", got, Finished)
	}
}

func TestMarkingAShowAgainKeepsTheOriginalWatchTimes(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	seasonID := addSeason(t, s, showID, 1, 2)
	ids := episodeIDs(t, s, seasonID)

	if _, err := s.ToggleEpisode(ctx(), ids[0]); err != nil {
		t.Fatalf("toggling: %v", err)
	}
	original := watchedAtOf(t, s, ids[0])

	if _, err := s.MarkShow(ctx(), showID); err != nil {
		t.Fatalf("marking show: %v", err)
	}

	if got := watchedAtOf(t, s, ids[0]); got != original {
		t.Errorf("watched_at = %q, want the original %q", got, original)
	}
	if got := categoryOf(t, s, showID); got != Finished {
		t.Errorf("category = %q, want %q", got, Finished)
	}
}

func TestMarkingASeasonLeavesOtherSeasonsAlone(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	first := addSeason(t, s, showID, 1, 2)
	second := addSeason(t, s, showID, 2, 2)

	if _, err := s.MarkSeason(ctx(), first); err != nil {
		t.Fatalf("marking season: %v", err)
	}

	detail, err := loadSeasonDetail(ctx(), s.db, second)
	if err != nil {
		t.Fatalf("reading second season: %v", err)
	}
	if watched := detail.WatchedCount(); watched != 0 {
		t.Errorf("second season has %d watched episodes, want 0", watched)
	}
	// Season 2 is still unwatched, so the show is only part-way through.
	if got := categoryOf(t, s, showID); got != Watching {
		t.Errorf("category = %q, want %q", got, Watching)
	}
}

func TestMarkingAnUnknownSeasonIsNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.MarkSeason(ctx(), 999)
	if !isNotFound(err) {
		t.Errorf("err = %v, want not found", err)
	}
}

// The third of the trio. MarkShow's check is the one with nothing under it
// to catch the counts, so without it an unknown id falls through to the
// recompute and surfaces as a raw sql.ErrNoRows - which isNotFound does not
// match, and the handler renders as a 500 rather than a 404.
func TestMarkingAnUnknownShowIsNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.MarkShow(ctx(), 999)
	if !isNotFound(err) {
		t.Errorf("err = %v, want not found", err)
	}
}

// ---------- what a watch mutation reports ----------
//
// The Season is the unit, so each of the three has to say which Seasons it
// touched and what they now look like. These read the returned value rather
// than the database on purpose: the value is built inside the transaction
// that mutated, and it is the only thing the handler ever sees.

func TestTogglingAnEpisodeReportsItsWholeSeason(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	first := addSeason(t, s, showID, 1, 3)
	second := addSeason(t, s, showID, 2, 2)

	update, err := s.ToggleEpisode(ctx(), episodeIDs(t, s, first)[0])
	if err != nil {
		t.Fatalf("toggling: %v", err)
	}

	if len(update.Seasons) != 1 {
		t.Fatalf("%d seasons reported, want 1", len(update.Seasons))
	}
	got := update.Seasons[0]
	if got.ID != first {
		t.Errorf("season %d reported, want %d", got.ID, first)
	}
	// The whole season, not the one row that changed.
	if got.TotalCount() != 3 || got.WatchedCount() != 1 {
		t.Errorf("season reads %d / %d watched, want 1 / 3",
			got.WatchedCount(), got.TotalCount())
	}
	// The season the click did not touch has nothing to swap.
	if got.ID == second {
		t.Error("the untouched season was reported")
	}
	if update.Show.Category != Watching {
		t.Errorf("category = %q, want %q", update.Show.Category, Watching)
	}
}

func TestMarkingASeasonReportsThatSeasonFull(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	first := addSeason(t, s, showID, 1, 2)
	addSeason(t, s, showID, 2, 2)

	update, err := s.MarkSeason(ctx(), first)
	if err != nil {
		t.Fatalf("marking season: %v", err)
	}

	if len(update.Seasons) != 1 {
		t.Fatalf("%d seasons reported, want 1", len(update.Seasons))
	}
	got := update.Seasons[0]
	if got.ID != first || got.WatchedCount() != got.TotalCount() {
		t.Errorf("season %d reads %d / %d watched, want %d full",
			got.ID, got.WatchedCount(), got.TotalCount(), first)
	}
	// Season 2 is still unwatched, so the show is only part-way through -
	// and the value says so without a second read.
	if update.Show.Category != Watching {
		t.Errorf("category = %q, want %q", update.Show.Category, Watching)
	}
}

func TestMarkingAShowReportsEverySeason(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	addSeason(t, s, showID, 1, 2)
	addSeason(t, s, showID, 2, 3)

	update, err := s.MarkShow(ctx(), showID)
	if err != nil {
		t.Fatalf("marking show: %v", err)
	}

	if len(update.Seasons) != 2 {
		t.Fatalf("%d seasons reported, want 2", len(update.Seasons))
	}
	for _, season := range update.Seasons {
		if season.TotalCount() == 0 || season.WatchedCount() != season.TotalCount() {
			t.Errorf("season %d reads %d / %d watched, want full",
				season.SeasonNumber, season.WatchedCount(), season.TotalCount())
		}
	}
	if update.Show.Category != Finished {
		t.Errorf("category = %q, want %q", update.Show.Category, Finished)
	}
}

// ---------- reading ----------

func TestDetailGroupsEveryEpisodeUnderItsOwnSeason(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)
	addSeason(t, s, showID, 1, 2)
	addSeason(t, s, showID, 2, 3)

	detail, err := s.Detail(ctx(), showID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if len(detail.Seasons) != 2 {
		t.Fatalf("got %d seasons, want 2", len(detail.Seasons))
	}
	if got := len(detail.Seasons[0].Episodes); got != 2 {
		t.Errorf("season 1 has %d episodes, want 2", got)
	}
	if got := len(detail.Seasons[1].Episodes); got != 3 {
		t.Errorf("season 2 has %d episodes, want 3", got)
	}
	// Episodes stay in broadcast order within a season.
	for i, episode := range detail.Seasons[1].Episodes {
		if episode.EpisodeNumber != int64(i+1) {
			t.Errorf("episode %d is numbered %d", i, episode.EpisodeNumber)
		}
	}
}

func TestDetailOfAShowWithNoSeasonsIsEmptyNotAnError(t *testing.T) {
	s := testStore(t)
	showID := seedShow(t, s, "Ended", Watchlist)

	detail, err := s.Detail(ctx(), showID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if len(detail.Seasons) != 0 {
		t.Errorf("got %d seasons, want none", len(detail.Seasons))
	}
}

func TestDetailOfAnUnknownShowIsNotFound(t *testing.T) {
	s := testStore(t)
	_, err := s.Detail(ctx(), 999)
	if !isNotFound(err) {
		t.Errorf("err = %v, want not found", err)
	}
}

// ---------- writing fetched metadata ----------

func TestInsertingAFetchedShowWritesEverySeasonAndEpisode(t *testing.T) {
	s := testStore(t)

	show, err := s.InsertShow(ctx(), fetched("Returning Series",
		fetchedSeason(1, 3), fetchedSeason(2, 2)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if show.Category != Watchlist {
		t.Errorf("category = %q, want %q", show.Category, Watchlist)
	}
	if show.Name != "Fetched Show" {
		t.Errorf("name = %q", show.Name)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if len(detail.Seasons) != 2 {
		t.Fatalf("got %d seasons, want 2", len(detail.Seasons))
	}
	if got := detail.Seasons[0].TotalCount(); got != 3 {
		t.Errorf("season 1 has %d episodes, want 3", got)
	}
	if got := detail.Seasons[1].TotalCount(); got != 2 {
		t.Errorf("season 2 has %d episodes, want 2", got)
	}
}

func TestARefreshPreservesWatchedMarksAndAddsNewEpisodes(t *testing.T) {
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Returning Series", fetchedSeason(1, 2)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if _, err := s.MarkShow(ctx(), show.ID); err != nil {
		t.Fatalf("marking watched: %v", err)
	}
	if got := categoryOf(t, s, show.ID); got != Ongoing {
		t.Fatalf("category = %q, want %q", got, Ongoing)
	}

	// A new episode appears in the season the user had finished.
	refreshed, err := s.ApplyRefresh(ctx(), show.ID,
		fetched("Returning Series", fetchedSeason(1, 3)))
	if err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	if refreshed.Category != Watching {
		t.Errorf("category = %q, want %q", refreshed.Category, Watching)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	watched, total := detail.Seasons[0].WatchedCount(), detail.Seasons[0].TotalCount()
	if watched != 2 || total != 3 {
		t.Errorf("progress = %d/%d, want 2/3", watched, total)
	}
}

func TestARefreshWritesALargeSeason(t *testing.T) {
	// Nothing chunks this insert to stay under SQLite's bound variable
	// limit, so the size is worth a test of its own.
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Ended", fetchedSeason(1, 250)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if got := detail.Seasons[0].TotalCount(); got != 250 {
		t.Errorf("season has %d episodes, want 250", got)
	}
}

// ---------- reconciling a refresh ----------

// A refresh leaves the show holding what TMDB reported and nothing else.
// Before this, writeSeasons only upserted, so a season TMDB renumbered or
// withdrew left rows behind that no later refresh could ever clear.

func TestARefreshRemovesEpisodesTMDBNoLongerReports(t *testing.T) {
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Ended", fetchedSeason(1, 3)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}

	if _, err := s.ApplyRefresh(ctx(), show.ID, fetched("Ended", fetchedSeason(1, 2))); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if got := detail.Seasons[0].TotalCount(); got != 2 {
		t.Errorf("season has %d episodes, want 2", got)
	}
}

func TestARefreshRemovesSeasonsTMDBNoLongerReports(t *testing.T) {
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Ended", fetchedSeason(1, 2), fetchedSeason(2, 2)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}

	if _, err := s.ApplyRefresh(ctx(), show.ID, fetched("Ended", fetchedSeason(1, 2))); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if len(detail.Seasons) != 1 {
		t.Fatalf("got %d seasons, want 1", len(detail.Seasons))
	}
	// The dropped season's episodes go with it rather than being orphaned,
	// which the schema's ON DELETE CASCADE is what guarantees.
	if got := countEpisodes(t, s, show.ID); got != 2 {
		t.Errorf("%d episode rows survive, want 2", got)
	}
}

// A special is reconciled by the same rule as any other season. Specials are
// excluded from the category, not from the data.
func TestARefreshRemovesASpecialTMDBNoLongerReports(t *testing.T) {
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Ended", fetchedSeason(0, 3), fetchedSeason(1, 2)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}

	if _, err := s.ApplyRefresh(ctx(), show.ID, fetched("Ended", fetchedSeason(1, 2))); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if len(detail.Seasons) != 1 || detail.Seasons[0].SeasonNumber != 1 {
		t.Fatalf("got %d seasons, first numbered %d - want only season 1",
			len(detail.Seasons), detail.Seasons[0].SeasonNumber)
	}
}

// TestARenumberedSeasonLeavesNoPhantomEpisode is the reproduction that
// prompted this, inverted. TMDB renumbering a season's episodes from (1,2)
// to (2,3) used to leave three episodes stored where TMDB reports two: the
// old episode 1 stayed forever, inflating the season's progress line and
// making "every episode watched" mean one more episode than exists.
func TestARenumberedSeasonLeavesNoPhantomEpisode(t *testing.T) {
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Ended", numberedSeason(1, 1, 2)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if _, err := s.MarkShow(ctx(), show.ID); err != nil {
		t.Fatalf("marking watched: %v", err)
	}

	if _, err := s.ApplyRefresh(ctx(), show.ID,
		fetched("Ended", numberedSeason(1, 2, 3))); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	season := detail.Seasons[0]
	if season.TotalCount() != 2 {
		t.Fatalf("season has %d episodes, want 2 - TMDB reports two", season.TotalCount())
	}
	for i, want := range []int64{2, 3} {
		if got := season.Episodes[i].EpisodeNumber; got != want {
			t.Errorf("episode %d is numbered %d, want %d", i, got, want)
		}
	}
	// Episode 2 was watched before the renumber and TMDB still reports it,
	// so its mark survives; episode 3 is new and arrives unwatched.
	if !season.Episodes[0].Watched || season.Episodes[1].Watched {
		t.Errorf("watched = %v, %v; want true, false",
			season.Episodes[0].Watched, season.Episodes[1].Watched)
	}
	if detail.Category != Watching {
		t.Errorf("category = %q, want %q - one of two is watched", detail.Category, Watching)
	}
}

// A fetch reporting no seasons at all is a bad answer, not an instruction to
// empty the show. This is the one case reconciling deliberately skips.
func TestARefreshReportingNoSeasonsChangesNothing(t *testing.T) {
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Ended", fetchedSeason(1, 2), fetchedSeason(2, 2)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if _, err := s.MarkShow(ctx(), show.ID); err != nil {
		t.Fatalf("marking watched: %v", err)
	}

	if _, err := s.ApplyRefresh(ctx(), show.ID, fetched("Ended")); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if len(detail.Seasons) != 2 {
		t.Errorf("got %d seasons, want 2 - nothing should have been removed", len(detail.Seasons))
	}
	if got := countEpisodes(t, s, show.ID); got != 4 {
		t.Errorf("%d episode rows survive, want 4", got)
	}
	if detail.Category != Finished {
		t.Errorf("category = %q, want %q", detail.Category, Finished)
	}
}

// A season TMDB lists with no episodes - announced but unaired - is a real
// state, so anything stored under it goes. Distinct from the whole-show case
// above, where an empty answer means the fetch failed to say anything.
func TestASeasonReportedWithNoEpisodesLosesItsStoredOnes(t *testing.T) {
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Returning Series", fetchedSeason(1, 2)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}

	if _, err := s.ApplyRefresh(ctx(), show.ID,
		fetched("Returning Series", fetchedSeason(1, 0))); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if len(detail.Seasons) != 1 {
		t.Fatalf("got %d seasons, want 1 - the season itself was reported", len(detail.Seasons))
	}
	if got := detail.Seasons[0].TotalCount(); got != 0 {
		t.Errorf("season has %d episodes, want 0", got)
	}
}

// Reconciling names every reported episode as a bound parameter, so the
// widest season has to stay under SQLite's per-statement ceiling of 32766.
// The longest season TMDB lists is orders of magnitude below that; this pins
// that the mechanism holds well past any real one.
func TestARefreshReconcilesALargeSeason(t *testing.T) {
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Ended", fetchedSeason(1, 250)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}

	if _, err := s.ApplyRefresh(ctx(), show.ID, fetched("Ended", fetchedSeason(1, 249))); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	detail, err := s.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if got := detail.Seasons[0].TotalCount(); got != 249 {
		t.Errorf("season has %d episodes, want 249", got)
	}
}

// ---------- settings ----------

func TestAnAPIKeyRoundTripsAndClears(t *testing.T) {
	s := testStore(t)

	if key, err := s.APIKey(ctx()); err != nil || key != "" {
		t.Fatalf("fresh database: key = %q, err = %v", key, err)
	}
	if err := s.SetAPIKey(ctx(), "secret"); err != nil {
		t.Fatalf("setting key: %v", err)
	}
	if key, _ := s.APIKey(ctx()); key != "secret" {
		t.Errorf("key = %q, want %q", key, "secret")
	}
	if err := s.ClearAPIKey(ctx()); err != nil {
		t.Fatalf("clearing key: %v", err)
	}
	if key, _ := s.APIKey(ctx()); key != "" {
		t.Errorf("key = %q after clearing, want empty", key)
	}
}

func TestAnEmptyStoredKeyReadsAsNoKey(t *testing.T) {
	s := testStore(t)
	if err := s.SetAPIKey(ctx(), ""); err != nil {
		t.Fatalf("setting key: %v", err)
	}
	if key, _ := s.APIKey(ctx()); key != "" {
		t.Errorf("key = %q, want empty", key)
	}
}

func TestDeletingAShowTakesItsSeasonsAndEpisodesWithIt(t *testing.T) {
	s := testStore(t)
	show, err := s.InsertShow(ctx(), fetched("Ended", fetchedSeason(1, 2)))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if err := s.DeleteShow(ctx(), show.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	var episodes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM episodes`).Scan(&episodes); err != nil {
		t.Fatalf("counting episodes: %v", err)
	}
	if episodes != 0 {
		t.Errorf("%d episodes left behind, want 0", episodes)
	}
}
