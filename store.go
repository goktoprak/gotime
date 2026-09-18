package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Everything that reads or writes tracked shows, seasons, episodes and
// categories. Nothing here calls TMDB: the handler fetches first and hands
// the result in, which is what makes every operation testable against a
// throwaway database file.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// notFoundError is a missing row, in the store's own terms. The HTTP layer
// turns it into a 404 without matching on message text.
type notFoundError struct{ what string }

func (e notFoundError) Error() string { return e.what }

func notFound(what string) error { return notFoundError{what} }

func isNotFound(err error) bool {
	var e notFoundError
	return errors.As(err, &e)
}

// alreadyTrackedError is a second add of a show that is already tracked.
// InsertShow raises it from inside its own transaction, which is the only
// place the answer cannot go stale before the insert lands.
type alreadyTrackedError struct{}

func (alreadyTrackedError) Error() string { return "show already tracked" }

func isAlreadyTracked(err error) bool {
	var e alreadyTrackedError
	return errors.As(err, &e)
}

const showColumns = `id, tmdb_id, name, overview, poster_path, backdrop_path,
	tmdb_status, category, added_at, last_refreshed_at`

// now is the timestamp format both apps write: RFC 3339 in UTC.
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ---------- settings ----------

// APIKey returns the stored TMDB key, or "" if none is set. A key stored as
// the empty string reads as absent.
func (s *Store) APIKey(ctx context.Context) (string, error) {
	var key sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT tmdb_api_key FROM settings WHERE id = 1`).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		// The settings row is seeded at migration time, so this only
		// happens if it was deleted by hand.
		return "", nil
	}
	return key.String, err
}

func (s *Store) SetAPIKey(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE settings SET tmdb_api_key = ? WHERE id = 1`, key)
	return err
}

func (s *Store) ClearAPIKey(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE settings SET tmdb_api_key = NULL WHERE id = 1`)
	return err
}

// Snapshot writes a consistent copy of the whole database to path via
// `VACUUM INTO`, which is safe even with uncheckpointed WAL writes
// outstanding.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// ---------- reading shows ----------

// queryer is the read half that *sql.DB and *sql.Tx have in common, so a
// read can run either on the pool or inside a transaction that has not
// committed yet. Two methods, not three: reads do not exec, and admitting
// ExecContext here would blur which of these functions can mutate.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) ListShows(ctx context.Context) ([]Show, error) {
	return loadShows(ctx, s.db)
}

// The load* functions are the reads themselves; the Store methods beside
// them are call sites that pass the pool. A mutation that has to end with a
// read passes its own transaction instead, so what it returns is the row it
// just wrote rather than whatever the pool sees a moment later.
func loadShows(ctx context.Context, q queryer) ([]Show, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+showColumns+` FROM shows ORDER BY name COLLATE NOCASE ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var shows []Show
	for rows.Next() {
		show, err := scanShow(rows)
		if err != nil {
			return nil, err
		}
		shows = append(shows, show)
	}
	return shows, rows.Err()
}

// Show loads one show row. Every operation that returns a show goes through
// loadShow, so a missing show behaves identically across all of them.
func (s *Store) Show(ctx context.Context, showID int64) (Show, error) {
	return loadShow(ctx, s.db, showID)
}

func loadShow(ctx context.Context, q queryer, showID int64) (Show, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+showColumns+` FROM shows WHERE id = ?`, showID)
	show, err := scanShow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Show{}, notFound("show not found")
	}
	return show, err
}

// Detail loads the whole show in two queries - one for seasons, one for
// every episode under it - and groups them in memory.
func (s *Store) Detail(ctx context.Context, showID int64) (ShowDetail, error) {
	return loadDetail(ctx, s.db, showID)
}

func loadDetail(ctx context.Context, q queryer, showID int64) (ShowDetail, error) {
	show, err := loadShow(ctx, q, showID)
	if err != nil {
		return ShowDetail{}, err
	}

	seasonRows, err := q.QueryContext(ctx,
		`SELECT id, show_id, tmdb_season_number, name, episode_count
		 FROM seasons WHERE show_id = ? ORDER BY tmdb_season_number ASC`, showID)
	if err != nil {
		return ShowDetail{}, err
	}
	defer seasonRows.Close()

	detail := ShowDetail{Show: show}
	// season id -> position in detail.Seasons, so episodes can be filed in
	// one pass instead of rescanning the slice per episode. The episode
	// query below joins through seasons on the same show_id, so it returns
	// exactly the seasons this map was built from and every lookup hits.
	at := map[int64]int{}
	for seasonRows.Next() {
		var (
			season Season
			name   sql.NullString
		)
		if err := seasonRows.Scan(&season.ID, &season.ShowID, &season.SeasonNumber,
			&name, &season.EpisodeCount); err != nil {
			return ShowDetail{}, err
		}
		season.Name = name.String
		at[season.ID] = len(detail.Seasons)
		detail.Seasons = append(detail.Seasons, SeasonDetail{Season: season})
	}
	if err := seasonRows.Err(); err != nil {
		return ShowDetail{}, err
	}

	episodeRows, err := q.QueryContext(ctx,
		`SELECT e.id, e.season_id, e.tmdb_episode_number, e.name, e.air_date,
		        e.watched, e.watched_at
		 FROM episodes e
		 JOIN seasons s ON e.season_id = s.id
		 WHERE s.show_id = ?
		 ORDER BY s.tmdb_season_number ASC, e.tmdb_episode_number ASC`, showID)
	if err != nil {
		return ShowDetail{}, err
	}
	defer episodeRows.Close()

	for episodeRows.Next() {
		episode, err := scanEpisode(episodeRows)
		if err != nil {
			return ShowDetail{}, err
		}
		i := at[episode.SeasonID]
		detail.Seasons[i].Episodes = append(detail.Seasons[i].Episodes, episode)
	}
	return detail, episodeRows.Err()
}

// loadSeasonDetail loads one season with its episodes. It has no Store
// method beside it: the only caller is a watch mutation building its
// answer, and it reads through that mutation's own transaction.
func loadSeasonDetail(ctx context.Context, q queryer, seasonID int64) (SeasonDetail, error) {
	var (
		season SeasonDetail
		name   sql.NullString
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, show_id, tmdb_season_number, name, episode_count
		 FROM seasons WHERE id = ?`, seasonID).
		Scan(&season.ID, &season.ShowID, &season.SeasonNumber, &name, &season.EpisodeCount)
	if errors.Is(err, sql.ErrNoRows) {
		return SeasonDetail{}, notFound("season not found")
	}
	if err != nil {
		return SeasonDetail{}, err
	}
	season.Name = name.String

	rows, err := q.QueryContext(ctx,
		`SELECT id, season_id, tmdb_episode_number, name, air_date, watched, watched_at
		 FROM episodes WHERE season_id = ? ORDER BY tmdb_episode_number ASC`, seasonID)
	if err != nil {
		return SeasonDetail{}, err
	}
	defer rows.Close()

	for rows.Next() {
		episode, err := scanEpisode(rows)
		if err != nil {
			return SeasonDetail{}, err
		}
		season.Episodes = append(season.Episodes, episode)
	}
	return season, rows.Err()
}

func (s *Store) IsTracked(ctx context.Context, tmdbID int64) (bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM shows WHERE tmdb_id = ?`, tmdbID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeleteShow removes a show; seasons and episodes go with it through the
// schema's ON DELETE CASCADE.
func (s *Store) DeleteShow(ctx context.Context, showID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM shows WHERE id = ?`, showID)
	return err
}

// ---------- writing fetched metadata ----------

// InsertShow writes a brand new show together with every season and
// episode, in one transaction. All-or-nothing is the point: a half-written
// show would be tracked but empty, and re-adding it would then conflict.
func (s *Store) InsertShow(ctx context.Context, fetched *FetchedShow) (Show, error) {
	stamp := now()
	var show Show
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Ask again in here. The handler checked before fetching from
		// TMDB, which takes as long as it takes, so two adds of the same
		// show can both come through that check; this one is inside the
		// transaction doing the insert. Without it the UNIQUE constraint
		// answers instead and the loser gets a 500, for something the add
		// page already has a message for.
		var existing int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM shows WHERE tmdb_id = ?`, fetched.TMDBID).Scan(&existing)
		if err == nil {
			return alreadyTrackedError{}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		res, err := tx.ExecContext(ctx,
			`INSERT INTO shows (tmdb_id, name, overview, poster_path, backdrop_path,
			                    tmdb_status, category, added_at, last_refreshed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fetched.TMDBID, fetched.Name, null(fetched.Overview), null(fetched.PosterPath),
			null(fetched.BackdropPath), null(fetched.TMDBStatus), string(Watchlist), stamp, stamp)
		if err != nil {
			return err
		}
		showID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if err := writeSeasons(ctx, tx, showID, fetched); err != nil {
			return err
		}
		if err := recomputeCategory(ctx, tx, showID); err != nil {
			return err
		}
		show, err = loadShow(ctx, tx, showID)
		return err
	})
	if err != nil {
		return Show{}, err
	}
	return show, nil
}

// ApplyRefresh re-writes an existing show's metadata, seasons and episodes
// in one transaction, leaving the show holding exactly what TMDB reported.
// Watched marks on episodes TMDB still reports survive; new episodes arrive
// unwatched; seasons and episodes TMDB has dropped are removed along with
// any marks they carried.
func (s *Store) ApplyRefresh(ctx context.Context, showID int64, fetched *FetchedShow) (Show, error) {
	var show Show
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE shows SET name = ?, overview = ?, poster_path = ?, backdrop_path = ?,
			                  tmdb_status = ?, last_refreshed_at = ?
			 WHERE id = ?`,
			fetched.Name, null(fetched.Overview), null(fetched.PosterPath),
			null(fetched.BackdropPath), null(fetched.TMDBStatus), now(), showID)
		if err != nil {
			return err
		}
		if err := writeSeasons(ctx, tx, showID, fetched); err != nil {
			return err
		}
		if err := reconcileSeasons(ctx, tx, showID, fetched); err != nil {
			return err
		}
		if err := recomputeCategory(ctx, tx, showID); err != nil {
			return err
		}
		show, err = loadShow(ctx, tx, showID)
		return err
	})
	if err != nil {
		return Show{}, err
	}
	return show, nil
}

// writeSeasons upserts every season and its episodes. Specials (season 0)
// are stored like any other season; they are only excluded when the
// category is derived.
//
// One statement per episode rather than a chunked multi-row insert: this
// driver is in-process, so there is no round trip to save.
func writeSeasons(ctx context.Context, tx *sql.Tx, showID int64, fetched *FetchedShow) error {
	upsertEpisode, err := tx.PrepareContext(ctx,
		`INSERT INTO episodes (season_id, tmdb_episode_number, name, air_date, watched)
		 VALUES (?, ?, ?, ?, 0)
		 ON CONFLICT(season_id, tmdb_episode_number)
		 DO UPDATE SET name = excluded.name, air_date = excluded.air_date`)
	if err != nil {
		return err
	}
	defer upsertEpisode.Close()

	for _, season := range fetched.Seasons {
		var seasonID int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO seasons (show_id, tmdb_season_number, name, episode_count)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(show_id, tmdb_season_number)
			 DO UPDATE SET name = excluded.name, episode_count = excluded.episode_count
			 RETURNING id`,
			showID, season.Number, null(season.Name), season.EpisodeCount).Scan(&seasonID)
		if err != nil {
			return err
		}

		for _, episode := range season.Episodes {
			if _, err := upsertEpisode.ExecContext(ctx,
				seasonID, episode.Number, null(episode.Name), null(episode.AirDate)); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileSeasons deletes the seasons and episodes TMDB no longer reports.
// writeSeasons has already written everything the fetch contained; this is
// the other half of making the stored show match it, and the two together
// are what "refresh" means.
//
// Only a refresh reconciles. A brand new show has nothing to reconcile
// against, so InsertShow simply doesn't call this - rather than passing
// writeSeasons a flag that would make it mean two different things.
//
// Deleting an episode deletes the watched mark it carried, which cannot be
// undone. That is deliberate: an episode TMDB has withdrawn - usually
// because a season was renumbered - otherwise counts toward the category
// forever. Nothing is stranded by that on its own, since marking the
// phantom watched does restore a finished show; what remains is a stored
// row set that disagrees with TMDB permanently, and compounds every time a
// season is reorganised. That is what the irreversible delete buys.
func reconcileSeasons(ctx context.Context, tx *sql.Tx, showID int64, fetched *FetchedShow) error {
	// A fetch that reports no seasons at all is a bad answer, not an
	// instruction to empty the show. Nothing here can tell one from the
	// other, so the reading that keeps data wins.
	if len(fetched.Seasons) == 0 {
		return nil
	}

	// Seasons first: their episodes go with them through ON DELETE CASCADE,
	// so the per-season pass below only ever sees seasons TMDB reported.
	// Specials (season 0) are reconciled by the same rule as any other
	// season - they are excluded from the category, not from the data.
	args := make([]any, 0, len(fetched.Seasons)+1)
	args = append(args, showID)
	for _, season := range fetched.Seasons {
		args = append(args, season.Number)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM seasons
		 WHERE show_id = ? AND tmdb_season_number NOT IN (`+placeholders(len(fetched.Seasons))+`)`,
		args...); err != nil {
		return err
	}

	// One statement per season rather than one big one, so the bound
	// parameters are bounded by the largest season rather than by the whole
	// show. SQLite's ceiling is 32766 per statement; the longest season TMDB
	// lists is three orders of magnitude below that.
	for _, season := range fetched.Seasons {
		query := `DELETE FROM episodes
		          WHERE season_id = (SELECT id FROM seasons
		                             WHERE show_id = ? AND tmdb_season_number = ?)`
		args := []any{showID, season.Number}
		// A season TMDB lists with no episodes yet - announced but unaired -
		// is a real state, so everything stored under it goes. The branch is
		// here because `NOT IN ()` is not valid SQL, not because the empty
		// case means something different.
		if len(season.Episodes) > 0 {
			query += ` AND tmdb_episode_number NOT IN (` + placeholders(len(season.Episodes)) + `)`
			for _, episode := range season.Episodes {
				args = append(args, episode.Number)
			}
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// placeholders builds the "?, ?, ?" of an IN clause holding n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// ---------- marking watched ----------

// ToggleEpisode flips one episode's watched flag. The mutation, the
// recompute and the read that describes them all share a transaction, so
// the counts the recompute reads cannot be stale and the Watch Update
// describes the rows this call wrote rather than whatever the pool sees a
// moment later.
func (s *Store) ToggleEpisode(ctx context.Context, episodeID int64) (WatchUpdate, error) {
	var update WatchUpdate
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// One lookup for all three facts. The episode's season is needed to
		// report the change and the season's show to recompute the
		// category, and `season_id` is a foreign key, so the join can only
		// drop a row the episode table does not have either.
		var (
			watched  bool
			seasonID int64
			showID   int64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT e.watched, e.season_id, s.show_id
			 FROM episodes e JOIN seasons s ON e.season_id = s.id
			 WHERE e.id = ?`, episodeID).Scan(&watched, &seasonID, &showID)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("episode not found")
		}
		if err != nil {
			return err
		}

		// Unwatching clears the timestamp: an unwatched episode has no
		// watch time.
		var watchedAt any
		if !watched {
			watchedAt = now()
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE episodes SET watched = ?, watched_at = ? WHERE id = ?`,
			!watched, watchedAt, episodeID); err != nil {
			return err
		}
		if err := recomputeCategory(ctx, tx, showID); err != nil {
			return err
		}
		update, err = oneSeasonUpdate(ctx, tx, showID, seasonID)
		return err
	})
	if err != nil {
		return WatchUpdate{}, err
	}
	return update, nil
}

// MarkSeason marks every episode of one season watched.
func (s *Store) MarkSeason(ctx context.Context, seasonID int64) (WatchUpdate, error) {
	var update WatchUpdate
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Resolve the season first: an unknown id has to become a not-found
		// here, because nothing below reports one. The UPDATE matches no
		// rows without complaining, and the recompute is a no-op on a show
		// with no episodes.
		var showID int64
		err := tx.QueryRowContext(ctx,
			`SELECT show_id FROM seasons WHERE id = ?`, seasonID).Scan(&showID)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("season not found")
		}
		if err != nil {
			return err
		}

		// `AND watched = 0` keeps the original watch time on episodes that
		// were already marked: re-marking a season must not rewrite history.
		if _, err := tx.ExecContext(ctx,
			`UPDATE episodes SET watched = 1, watched_at = ?
			 WHERE season_id = ? AND watched = 0`, now(), seasonID); err != nil {
			return err
		}
		if err := recomputeCategory(ctx, tx, showID); err != nil {
			return err
		}
		update, err = oneSeasonUpdate(ctx, tx, showID, seasonID)
		return err
	})
	if err != nil {
		return WatchUpdate{}, err
	}
	return update, nil
}

// MarkShow marks every episode of every season watched, specials included -
// the user asked for all of them.
func (s *Store) MarkShow(ctx context.Context, showID int64) (WatchUpdate, error) {
	var update WatchUpdate
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Same reason as MarkSeason: not-found before mutating anything.
		// The check now reads through the transaction that does the
		// mutating, so it cannot be answered by a row the update no longer
		// sees.
		if _, err := loadShow(ctx, tx, showID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE episodes SET watched = 1, watched_at = ?
			 WHERE season_id IN (SELECT id FROM seasons WHERE show_id = ?)
			   AND watched = 0`, now(), showID); err != nil {
			return err
		}
		if err := recomputeCategory(ctx, tx, showID); err != nil {
			return err
		}
		// Every season was touched, so the whole show is the answer, and
		// loadDetail is already exactly that shape. Read again rather than
		// reusing the pre-flight row: the recompute above may have moved
		// the category.
		detail, err := loadDetail(ctx, tx, showID)
		if err != nil {
			return err
		}
		update = WatchUpdate{Show: detail.Show, Seasons: detail.Seasons}
		return nil
	})
	if err != nil {
		return WatchUpdate{}, err
	}
	return update, nil
}

// oneSeasonUpdate is the answer for a mutation that touched a single
// season, which is two of the three. Runs on the mutation's transaction.
func oneSeasonUpdate(ctx context.Context, q queryer, showID, seasonID int64) (WatchUpdate, error) {
	show, err := loadShow(ctx, q, showID)
	if err != nil {
		return WatchUpdate{}, err
	}
	season, err := loadSeasonDetail(ctx, q, seasonID)
	if err != nil {
		return WatchUpdate{}, err
	}
	return WatchUpdate{Show: show, Seasons: []SeasonDetail{season}}, nil
}

// recomputeCategory recomputes and persists a show's category. Called after
// toggling an episode and after refreshing metadata, where new episodes may
// appear.
//
// Specials (season 0) are excluded from the counts entirely. TMDB files a
// large and unstable set of shorts, recaps and clips under season 0, so
// counting them would hold shows in Watching indefinitely and reclassify
// them whenever TMDB adds one. They are still stored, shown and
// individually markable - they just don't decide the category.
//
// Takes the transaction rather than the pool so it runs alongside the
// mutation that triggered it; otherwise the counts it reads could be stale
// by the time it writes.
func recomputeCategory(ctx context.Context, tx *sql.Tx, showID int64) error {
	// One pass for both counts: they differ only by the watched filter.
	var total, watched int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(e.watched), 0) FROM episodes e
		 JOIN seasons s ON e.season_id = s.id
		 WHERE s.show_id = ? AND s.tmdb_season_number != 0`, showID).
		Scan(&total, &watched); err != nil {
		return err
	}

	// No episode data at all is not a category. A refresh that hasn't
	// repopulated seasons yet must leave the show where it was, and leaving
	// it alone here costs neither a read of the column nor a write of the
	// value back over itself.
	if total == 0 {
		return nil
	}

	var tmdbStatus sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT tmdb_status FROM shows WHERE id = ?`, showID).
		Scan(&tmdbStatus); err != nil {
		return err
	}

	category := deriveCategory(total, watched, tmdbStatus.String)
	_, err := tx.ExecContext(ctx,
		`UPDATE shows SET category = ? WHERE id = ?`, string(category), showID)
	return err
}

// ---------- plumbing ----------

// inTx runs fn in a transaction, committing on success and rolling back on
// any error or panic.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// scanner is satisfied by both *sql.Row and *sql.Rows, so a row can be
// decoded the same way whether it came from a single lookup or a loop.
type scanner interface {
	Scan(dest ...any) error
}

func scanShow(src scanner) (Show, error) {
	var (
		show                                                    Show
		overview, poster, backdrop, tmdbStatus, lastRefreshedAt sql.NullString
		category                                                string
	)
	err := src.Scan(&show.ID, &show.TMDBID, &show.Name, &overview, &poster, &backdrop,
		&tmdbStatus, &category, &show.AddedAt, &lastRefreshedAt)
	if err != nil {
		return Show{}, err
	}
	show.Overview = overview.String
	show.PosterPath = poster.String
	show.BackdropPath = backdrop.String
	show.TMDBStatus = tmdbStatus.String
	show.LastRefreshedAt = lastRefreshedAt.String

	// A category outside the four is only possible in a hand-edited
	// database. Reading it as Watchlist keeps the show visible on the
	// dashboard instead of hiding it under a tab that doesn't exist.
	parsed, ok := ParseCategory(category)
	if !ok {
		parsed = Watchlist
	}
	show.Category = parsed
	return show, nil
}

func scanEpisode(src scanner) (Episode, error) {
	var (
		episode            Episode
		name, air, watched sql.NullString
	)
	err := src.Scan(&episode.ID, &episode.SeasonID, &episode.EpisodeNumber,
		&name, &air, &episode.Watched, &watched)
	if err != nil {
		return Episode{}, err
	}
	episode.Name = name.String
	episode.AirDate = air.String
	episode.WatchedAt = watched.String
	return episode, nil
}

// null stores "" as SQL NULL, so a field TMDB omitted reads back as absent
// rather than as an empty string.
func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}
