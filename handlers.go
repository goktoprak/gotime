package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// refreshConcurrency is how many shows refresh-all works on at once. The
// TMDB requests underneath are capped separately and process-wide, so this
// number decides only how many shows are in flight, not how many requests.
// Refreshing writes as well as reads and the database takes one writer at a
// time, so raising this much further buys little.
const refreshConcurrency = 4

// App is the HTTP layer: it extracts arguments, calls one store operation,
// and renders. Deciding anything about categories or watched marks is the
// store's job; talking to TMDB is the client's.
type App struct {
	store *Store
	tmdb  *TMDB
}

// tabCookie remembers which dashboard tab was last open. A cookie rather
// than local storage, so the server renders the right tab on first paint.
const tabCookie = "gotime_tab"

// ---------- dashboard ----------

type tab struct {
	Category Category
	Count    int
	Active   bool
}

type indexData struct {
	Tabs     []tab
	Active   Category
	Shows    []Show
	HasShows bool
	Notice   Message
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	shows, err := a.store.ListShows(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}

	active := a.activeTab(r)
	http.SetCookie(w, &http.Cookie{
		Name: tabCookie, Value: string(active), Path: "/",
		MaxAge: int((365 * 24 * time.Hour).Seconds()), SameSite: http.SameSiteLaxMode,
	})

	data := indexData{Active: active, HasShows: len(shows) > 0, Notice: takeFlash(w, r)}
	counts := map[Category]int{}
	for _, show := range shows {
		counts[show.Category]++
		if show.Category == active {
			data.Shows = append(data.Shows, show)
		}
	}
	for _, category := range AllCategories {
		data.Tabs = append(data.Tabs, tab{
			Category: category,
			Count:    counts[category],
			Active:   category == active,
		})
	}

	render(w, http.StatusOK, "index.html", data)
}

// activeTab reads the tab from the query string, falling back to the
// remembered one and then to Watching. Anything unparseable falls back
// rather than throwing the dashboard off.
func (a *App) activeTab(r *http.Request) Category {
	if c, ok := ParseCategory(r.URL.Query().Get("tab")); ok {
		return c
	}
	if cookie, err := r.Cookie(tabCookie); err == nil {
		if c, ok := ParseCategory(cookie.Value); ok {
			return c
		}
	}
	return Watching
}

// ---------- show detail ----------

type showData struct {
	Detail ShowDetail
	Notice Message
}

func (a *App) showPage(w http.ResponseWriter, r *http.Request) {
	showID, ok := pathID(w, r, "id")
	if !ok {
		return
	}

	detail, err := a.store.Detail(r.Context(), showID)
	if err != nil {
		a.storeError(w, r, err)
		return
	}

	render(w, http.StatusOK, "show.html", showData{Detail: detail, Notice: takeFlash(w, r)})
}

// ---------- adding a show ----------

type addData struct {
	TMDBID string
	Notice Message
}

// The answer to an add of a show already on the list, from either of the
// two places that can find out: the check before the fetch, and the
// transaction that does the insert.
const alreadyTrackedMsg = "That show is already tracked."

func (a *App) addPage(w http.ResponseWriter, r *http.Request) {
	render(w, http.StatusOK, "add.html", addData{Notice: takeFlash(w, r)})
}

func (a *App) addShow(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.FormValue("tmdb_id"))

	// A blank field, a non-number, and 0 are all rejected.
	tmdbID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || tmdbID == 0 {
		a.addFailed(w, http.StatusBadRequest, raw, "Please enter a valid TMDB ID.")
		return
	}

	apiKey, err := a.store.APIKey(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if apiKey == "" {
		a.addFailed(w, http.StatusBadRequest, raw,
			"No TMDB API key set - add one in Settings first.")
		return
	}

	tracked, err := a.store.IsTracked(r.Context(), tmdbID)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if tracked {
		a.addFailed(w, http.StatusConflict, raw, alreadyTrackedMsg)
		return
	}

	// Fetch everything before writing anything: a TMDB failure here must
	// not leave a tracked-but-empty show that can never be re-added.
	fetched, err := a.tmdb.FetchShow(r.Context(), tmdbID, apiKey)
	if err != nil {
		log.Printf("adding tmdb id %d: %v", tmdbID, err)
		a.addFailed(w, http.StatusBadGateway, raw, tmdbMessage(err))
		return
	}

	show, err := a.store.InsertShow(r.Context(), fetched)
	if err != nil {
		// Someone else added it while the fetch above was in flight.
		if isAlreadyTracked(err) {
			a.addFailed(w, http.StatusConflict, raw, alreadyTrackedMsg)
			return
		}
		a.serverError(w, r, err)
		return
	}

	setFlash(w, successMsg(fmt.Sprintf("Added %q.", show.Name)))
	http.Redirect(w, r, fmt.Sprintf("/show/%d", show.ID), http.StatusSeeOther)
}

// addFailed re-renders the add form with the entered id still in the field.
func (a *App) addFailed(w http.ResponseWriter, status int, raw, msg string) {
	render(w, status, "add.html", addData{TMDBID: raw, Notice: errorMsg(msg)})
}

// ---------- settings ----------

type settingsData struct {
	HasKey    bool
	MaskedKey string
	Notice    Message
}

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	key, err := a.store.APIKey(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	render(w, http.StatusOK, "settings.html", settingsData{
		HasKey:    key != "",
		MaskedKey: maskAPIKey(key),
		Notice:    takeFlash(w, r),
	})
}

func (a *App) setAPIKey(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.FormValue("api_key"))
	if key == "" {
		setFlash(w, errorMsg("Please enter an API key."))
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	// A key TMDB won't accept is the user's problem, not a bad gateway, so
	// it's reported as a plain message rather than an upstream failure.
	if err := a.tmdb.ValidateKey(r.Context(), key); err != nil {
		log.Printf("validating api key: %v", err)
		setFlash(w, errorMsg(tmdbMessage(err)))
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	if err := a.store.SetAPIKey(r.Context(), key); err != nil {
		a.serverError(w, r, err)
		return
	}
	setFlash(w, successMsg("API key saved."))
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (a *App) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := a.store.ClearAPIKey(r.Context()); err != nil {
		a.serverError(w, r, err)
		return
	}
	setFlash(w, successMsg("API key removed."))
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// export serves a consistent snapshot of the database as a downloadable
// file, for use as a manual backup.
func (a *App) export(w http.ResponseWriter, r *http.Request) {
	// VACUUM INTO refuses to overwrite, so it needs a path that does not
	// exist yet - hence a fresh directory rather than a temp file.
	dir, err := os.MkdirTemp("", "gotime-export-")
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "gotime.db")
	if err := a.store.Snapshot(r.Context(), path); err != nil {
		a.serverError(w, r, err)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	defer file.Close()

	filename := "gotime-backup-" + time.Now().Format("20060102-150405") + ".db"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filename, time.Now(), file)
}

// ---------- refreshing ----------

func (a *App) refreshShow(w http.ResponseWriter, r *http.Request) {
	showID, ok := pathID(w, r, "id")
	if !ok {
		return
	}

	apiKey, err := a.store.APIKey(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if apiKey == "" {
		setFlash(w, errorMsg("No TMDB API key set."))
		http.Redirect(w, r, fmt.Sprintf("/show/%d", showID), http.StatusSeeOther)
		return
	}

	if err := a.refreshOne(r.Context(), showID, apiKey); err != nil {
		if isNotFound(err) {
			a.storeError(w, r, err)
			return
		}
		log.Printf("refreshing show %d: %v", showID, err)
		setFlash(w, errorMsg("Refresh failed. "+tmdbMessage(err)))
	} else {
		setFlash(w, successMsg("Metadata refreshed."))
	}
	http.Redirect(w, r, fmt.Sprintf("/show/%d", showID), http.StatusSeeOther)
}

func (a *App) refreshAll(w http.ResponseWriter, r *http.Request) {
	apiKey, err := a.store.APIKey(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if apiKey == "" {
		setFlash(w, errorMsg("No TMDB API key set."))
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	shows, err := a.store.ListShows(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}

	// Several shows at a time. The requests they make are already capped
	// process-wide inside the TMDB client, so this widens the pipe without
	// changing how hard TMDB is hit - it just stops each show waiting for
	// the one before it. Whoever clicked is holding a connection open for
	// the whole run, and a reverse proxy will not wait forever.
	//
	// The slot is taken before the goroutine is spawned, so a long list
	// queues in this loop rather than in memory: refreshConcurrency
	// goroutines at a time, not one per show. Each one writes a single
	// element of errs and reads nothing, so there is no shared state to
	// guard - the counts are a pass over the results once every goroutine
	// has stopped.
	var (
		wg        sync.WaitGroup
		errs      = make([]error, len(shows))
		slots     = make(chan struct{}, refreshConcurrency)
		attempted int
	)
queue:
	for i, show := range shows {
		select {
		case slots <- struct{}{}:
		case <-r.Context().Done():
			// The client is gone; nothing left to report to, so stop
			// handing out work. What is already running still finishes.
			break queue
		}
		attempted++
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			errs[i] = a.refreshOne(r.Context(), show.ID, apiKey)
		}()
	}
	wg.Wait()

	// Only as far as attempted: past it, errs holds untouched zero values
	// for shows that were never started, which are not successes.
	var refreshed, failed int
	for i, err := range errs[:attempted] {
		if err != nil {
			log.Printf("refreshing %q: %v", shows[i].Name, err)
			failed++
			continue
		}
		refreshed++
	}

	msg := fmt.Sprintf("Refreshed %d show%s.", refreshed, plural(refreshed))
	if failed != 0 {
		msg += fmt.Sprintf(" %d failed.", failed)
	}
	// Info, not success: this line can carry failures.
	setFlash(w, infoMsg(msg))
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// refreshOne re-reads one show from TMDB and applies it, preserving watched
// marks. Shared by the single-show and refresh-all actions.
//
// Takes a context rather than the request it came from. Nothing here reads a
// header, a path or a body, and refresh-all runs it in a goroutine, where a
// live *http.Request is a hazard rather than a convenience.
//
// The show is read whole for one field. Store.Show is the only lookup a show
// id needs, so an id that isn't a show fails here the same way it fails
// everywhere else, and there is no second not-found path to keep in step.
func (a *App) refreshOne(ctx context.Context, showID int64, apiKey string) error {
	show, err := a.store.Show(ctx, showID)
	if err != nil {
		return err
	}
	fetched, err := a.tmdb.FetchShow(ctx, show.TMDBID, apiKey)
	if err != nil {
		return err
	}
	_, err = a.store.ApplyRefresh(ctx, showID, fetched)
	return err
}

func (a *App) deleteShow(w http.ResponseWriter, r *http.Request) {
	showID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := a.store.DeleteShow(r.Context(), showID); err != nil {
		a.serverError(w, r, err)
		return
	}
	setFlash(w, successMsg("Show deleted."))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- marking watched ----------
//
// The three actions htmx drives. Each answers with the seasons its change
// touched, marked for out-of-band swaps, so the open season panels and the
// scroll position survive a click. Submitted as a plain form instead - no
// JavaScript - each falls back to a redirect and a full re-render, which
// lands on exactly the same state.

// watchAction is all three: extract the id, mutate, and answer with what
// the mutation says it touched. The store decides how much that is - one
// season for a toggle or a season mark, every season for a show mark - so
// there is nothing here to vary per action.
func (a *App) watchAction(w http.ResponseWriter, r *http.Request,
	mutate func(context.Context, int64) (WatchUpdate, error)) {

	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}

	update, err := mutate(r.Context(), id)
	if err != nil {
		a.actionError(w, r, err)
		return
	}
	if !isHTMX(r) {
		a.redirectToShow(w, r, update.Show.ID)
		return
	}

	parts := []fragment{
		{"category-badge", badgeFragment{Show: update.Show, OOB: true}},
		{"notice", noticeFragment{OOB: true}},
	}
	// Progress line and episode list per touched season: the list carries
	// the checkboxes, the line the "3 / 10 watched" above them.
	for _, season := range update.Seasons {
		parts = append(parts,
			fragment{"season-progress", seasonFragment{SeasonDetail: season, OOB: true}},
			fragment{"episode-list", seasonFragment{SeasonDetail: season, OOB: true}},
		)
	}
	renderFragments(w, parts...)
}

func (a *App) toggleEpisode(w http.ResponseWriter, r *http.Request) {
	a.watchAction(w, r, a.store.ToggleEpisode)
}

func (a *App) markSeason(w http.ResponseWriter, r *http.Request) {
	a.watchAction(w, r, a.store.MarkSeason)
}

func (a *App) markShow(w http.ResponseWriter, r *http.Request) {
	a.watchAction(w, r, a.store.MarkShow)
}

// ---------- failure paths ----------

// actionError reports a failed mutation. An htmx caller is told to reload
// the page: whatever it optimistically showed is now wrong, and a reload
// both restores the truth and picks up the flash message.
func (a *App) actionError(w http.ResponseWriter, r *http.Request, err error) {
	if isNotFound(err) {
		a.storeError(w, r, err)
		return
	}
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	setFlash(w, errorMsg(err.Error()))

	if isHTMX(r) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	a.redirectBack(w, r)
}

// storeError renders a store failure: a missing row as the 404 page,
// anything else as a server error.
func (a *App) storeError(w http.ResponseWriter, r *http.Request, err error) {
	if isNotFound(err) {
		var e notFoundError
		errors.As(err, &e)
		if isHTMX(r) {
			http.Error(w, e.what, http.StatusNotFound)
			return
		}
		render(w, http.StatusNotFound, "not_found.html", nil)
		return
	}
	a.serverError(w, r, err)
}

func (a *App) serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	http.Error(w, "Something went wrong. Check the server log.", http.StatusInternalServerError)
}

func (a *App) notFoundPage(w http.ResponseWriter, r *http.Request) {
	render(w, http.StatusNotFound, "not_found.html", nil)
}

// ---------- small helpers ----------

// pathID reads a numeric path segment, answering 404 for anything else.
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		render(w, http.StatusNotFound, "not_found.html", nil)
		return 0, false
	}
	return id, true
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func (a *App) redirectToShow(w http.ResponseWriter, r *http.Request, showID int64) {
	http.Redirect(w, r, fmt.Sprintf("/show/%d", showID), http.StatusSeeOther)
}

// redirectBack returns to the page the request came from, for the no-script
// path where there is no show id to hand. Only a same-host referer is
// followed, so a link from elsewhere can't aim the redirect off-site.
func (a *App) redirectBack(w http.ResponseWriter, r *http.Request) {
	target := "/"
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host && ref.Path != "" {
		target = ref.RequestURI()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
