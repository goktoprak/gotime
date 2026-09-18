// GoTime is a personal TV tracker: one person records which episodes of
// which shows they have watched, and TMDB supplies the metadata. Everything
// lives in one SQLite file.
//
// Everything is server-rendered HTML. The only client-side code is htmx,
// served from this binary, and it is used for exactly one thing: ticking
// episodes off without reloading the page.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run holds what main does so that every failure leaves through a return.
// log.Fatalf ends the process with os.Exit, which does not run deferred
// calls - including the database close below, which is what gives SQLite a
// clean WAL. Only the exit code is main's job.
func run() error {
	dbPath := flag.String("db", env("GOTIME_DB", "gotime.db"), "path to the SQLite database")
	bind := flag.String("bind", env("GOTIME_BIND", "0.0.0.0:3000"), "address to listen on")
	flag.Parse()

	db, err := openDB(*dbPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", *dbPath, err)
	}
	defer db.Close()

	app := &App{store: NewStore(db), tmdb: NewTMDB()}

	server := &http.Server{
		Addr:    *bind,
		Handler: logRequests(routes(app)),
		// Only the header timeout is set. A refresh of every tracked show
		// is one long request by design, so capping the write side would
		// cut it off part-way.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Buffered, so the goroutine can report a failed listen and finish
	// even if nobody is left to receive it.
	errCh := make(chan error, 1)
	go func() {
		log.Printf("GoTime running at http://%s (database: %s)", *bind, *dbPath)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Shut down on Ctrl-C rather than dying mid-write: SQLite is happier
	// closing its WAL cleanly.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		// Nothing to shut down - the server never came up. Returning is
		// still the way out, because the database is open either way.
		return fmt.Errorf("server: %w", err)
	case <-stop:
	}

	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	return nil
}

func routes(app *App) http.Handler {
	mux := http.NewServeMux()

	// Pages.
	mux.HandleFunc("GET /{$}", app.index)
	mux.HandleFunc("GET /show/{id}", app.showPage)
	mux.HandleFunc("GET /add", app.addPage)
	mux.HandleFunc("GET /settings", app.settingsPage)
	mux.HandleFunc("GET /export", app.export)

	// Actions. Everything that changes something is a POST, and answers
	// either an htmx fragment or a redirect.
	mux.HandleFunc("POST /add", app.addShow)
	mux.HandleFunc("POST /settings/apikey", app.setAPIKey)
	mux.HandleFunc("POST /settings/apikey/delete", app.deleteAPIKey)
	mux.HandleFunc("POST /shows/refresh-all", app.refreshAll)
	mux.HandleFunc("POST /shows/{id}/refresh", app.refreshShow)
	mux.HandleFunc("POST /shows/{id}/delete", app.deleteShow)
	mux.HandleFunc("POST /shows/{id}/mark-watched", app.markShow)
	mux.HandleFunc("POST /seasons/{id}/mark-watched", app.markSeason)
	mux.HandleFunc("POST /episodes/{id}/toggle", app.toggleEpisode)

	// The stylesheet and htmx are compiled into the binary, so a build is
	// one file with no directory to deploy beside it.
	mux.Handle("GET /static/", cacheStatic(http.FileServerFS(assets)))

	// Anything unclaimed. `GET /{$}` above matches only the bare path, so
	// this is the catch-all rather than a second route to the dashboard.
	mux.HandleFunc("GET /", app.notFoundPage)

	return mux
}

// cacheStatic lets the browser hold onto the stylesheet and htmx for an
// hour. They aren't content-hashed, so this is deliberately short - long
// enough to skip refetching them on every navigation, short enough that an
// upgrade shows up without a hard reload.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
