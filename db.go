package main

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// schema is the whole database as this app expects to find it. Every
// statement is idempotent, so it runs unchanged against a fresh file and
// against one the app has already been using: startup is the same code path
// either way, and there is no migration ordering to get wrong.
//
// Any table this app did not create is left alone.
const schema = `
CREATE TABLE IF NOT EXISTS settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    tmdb_api_key TEXT
);
INSERT OR IGNORE INTO settings (id, tmdb_api_key) VALUES (1, NULL);

CREATE TABLE IF NOT EXISTS shows (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    tmdb_id INTEGER NOT NULL UNIQUE,
    name TEXT NOT NULL,
    overview TEXT,
    poster_path TEXT,
    backdrop_path TEXT,
    tmdb_status TEXT,
    category TEXT NOT NULL DEFAULT 'watchlist',
    added_at TEXT NOT NULL DEFAULT (datetime('now')),
    last_refreshed_at TEXT
);

CREATE TABLE IF NOT EXISTS seasons (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    show_id INTEGER NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    tmdb_season_number INTEGER NOT NULL,
    name TEXT,
    episode_count INTEGER NOT NULL DEFAULT 0,
    UNIQUE(show_id, tmdb_season_number)
);

CREATE TABLE IF NOT EXISTS episodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    season_id INTEGER NOT NULL REFERENCES seasons(id) ON DELETE CASCADE,
    tmdb_episode_number INTEGER NOT NULL,
    name TEXT,
    air_date TEXT,
    watched INTEGER NOT NULL DEFAULT 0,
    watched_at TEXT,
    UNIQUE(season_id, tmdb_episode_number)
);

CREATE INDEX IF NOT EXISTS idx_seasons_show_id ON seasons(show_id);
CREATE INDEX IF NOT EXISTS idx_episodes_season_id ON episodes(season_id);
`

// openDB opens (creating if missing) the SQLite database at path and brings
// its schema up to date.
//
// One connection, deliberately. This is a single-user app whose writes are
// all short, and a pool of one means a write can never meet "database is
// locked" from another request mid-transaction.
func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)",
		url.PathEscape(path),
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate brings the file up to the schema above, creating whatever is
// missing. On a database that already has it, every statement is a no-op.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("applying schema: %w", err)
	}
	return nil
}
