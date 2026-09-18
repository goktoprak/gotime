package main

import (
	"path/filepath"
	"testing"
)

// TestReopeningADatabaseKeepsEverything is the guarantee that matters on
// every restart: openDB runs the schema again over a file that already has
// data, and nothing it does may disturb what is stored.
func TestReopeningADatabaseKeepsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gotime.db")

	seed, err := openDB(path)
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	if _, err := seed.Exec(`
		UPDATE settings SET tmdb_api_key = 'stored-key' WHERE id = 1;
		INSERT INTO shows (id, tmdb_id, name, overview, tmdb_status, category, added_at)
		VALUES (7, 1399, 'Game of Thrones', 'Nine noble families.', 'Ended', 'watching', '2024-05-01T10:00:00Z');
		INSERT INTO seasons (id, show_id, tmdb_season_number, name, episode_count)
		VALUES (3, 7, 1, 'Season 1', 2);
		INSERT INTO episodes (season_id, tmdb_episode_number, name, air_date, watched, watched_at)
		VALUES (3, 1, 'Winter Is Coming', '2011-04-17', 1, '2024-05-02T20:00:00Z'),
		       (3, 2, 'The Kingsroad', '2011-04-24', 0, NULL);
	`); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	seed.Close()

	db, err := openDB(path)
	if err != nil {
		t.Fatalf("reopening database: %v", err)
	}
	defer db.Close()
	store := NewStore(db)

	key, err := store.APIKey(ctx())
	if err != nil || key != "stored-key" {
		t.Errorf("api key = %q, err = %v", key, err)
	}

	detail, err := store.Detail(ctx(), 7)
	if err != nil {
		t.Fatalf("reading show: %v", err)
	}
	if detail.Name != "Game of Thrones" || detail.Category != Watching {
		t.Errorf("show = %q / %q", detail.Name, detail.Category)
	}
	if detail.TMDBStatus != "Ended" {
		t.Errorf("tmdb status = %q, want %q", detail.TMDBStatus, "Ended")
	}
	if len(detail.Seasons) != 1 {
		t.Fatalf("got %d seasons, want 1", len(detail.Seasons))
	}
	watched, total := detail.Seasons[0].WatchedCount(), detail.Seasons[0].TotalCount()
	if watched != 1 || total != 2 {
		t.Errorf("progress = %d/%d, want 1/2", watched, total)
	}
	if detail.Seasons[0].Episodes[0].WatchedAt != "2024-05-02T20:00:00Z" {
		t.Errorf("watch time = %q", detail.Seasons[0].Episodes[0].WatchedAt)
	}
}

// TestMigrationIsIdempotent - the app restarts against its own database far
// more often than it creates one.
func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gotime.db")

	for i := range 3 {
		db, err := openDB(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		var settings int
		if err := db.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&settings); err != nil {
			t.Fatalf("counting settings rows: %v", err)
		}
		if settings != 1 {
			t.Errorf("open %d: %d settings rows, want 1", i, settings)
		}
		db.Close()
	}
}
