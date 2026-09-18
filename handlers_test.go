package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testApp is the whole app over a throwaway database, with TMDB faked at
// the seam newTMDB already provides: a real client pointed at a real
// server. Nothing here is mocked - the JSON is decoded by the same code
// that decodes TMDB's. It serves the show seedFullShow inserts, so the
// stored show and the upstream one start out identical and a test that
// wants them to differ says so.
func testApp(t *testing.T) (*App, http.Handler) {
	t.Helper()
	return appWithTMDB(t, tmdbServer(t, fullShow()))
}

// appWithTMDB is testApp for the tests that need TMDB to answer something
// of their own - a different show, or a failure.
func appWithTMDB(t *testing.T, server *httptest.Server) (*App, http.Handler) {
	t.Helper()
	app := &App{store: testStore(t), tmdb: newTMDB(server.Client(), server.URL)}
	return app, routes(app)
}

// tmdbServer is a fake TMDB that reports one show and nothing else.
func tmdbServer(t *testing.T, show *FetchedShow) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveTMDB(w, r, show)
	}))
	t.Cleanup(server.Close)
	return server
}

// serveTMDB answers the two requests FetchShow makes: the show itself, then
// one season at a time. A season the show does not list gets TMDB's own
// 404 shape rather than a bare status, because that is the answer the app
// has to survive.
func serveTMDB(w http.ResponseWriter, r *http.Request, show *FetchedShow) {
	w.Header().Set("Content-Type", "application/json")

	_, number, isSeason := strings.Cut(r.URL.Path, "/season/")
	if !isSeason {
		json.NewEncoder(w).Encode(showBody(show))
		return
	}
	for _, season := range show.Seasons {
		if strconv.FormatInt(season.Number, 10) == number {
			json.NewEncoder(w).Encode(seasonBody(season))
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"success":false,"status_code":34}`))
}

// showBody and seasonBody are TMDB's wire shapes, written out by hand
// rather than by reusing tmdb.go's structs. Encoding with the same structs
// the app decodes with would agree about a field read under the wrong name
// and prove nothing; these spell the names TMDB actually sends.
func showBody(show *FetchedShow) map[string]any {
	seasons := make([]map[string]any, len(show.Seasons))
	for i, season := range show.Seasons {
		seasons[i] = map[string]any{
			"season_number": season.Number,
			"name":          season.Name,
			"episode_count": season.EpisodeCount,
		}
	}
	return map[string]any{
		"id":            show.TMDBID,
		"name":          show.Name,
		"overview":      show.Overview,
		"poster_path":   show.PosterPath,
		"backdrop_path": show.BackdropPath,
		"status":        show.TMDBStatus,
		"seasons":       seasons,
	}
}

func seasonBody(season FetchedSeason) map[string]any {
	episodes := make([]map[string]any, len(season.Episodes))
	for i, episode := range season.Episodes {
		episodes[i] = map[string]any{
			"episode_number": episode.Number,
			"name":           episode.Name,
			"air_date":       episode.AirDate,
		}
	}
	return map[string]any{"episodes": episodes}
}

// fullShow is the show the handler tests work with: a poster, a backdrop
// and two seasons. seedFullShow writes it and the fake TMDB reports it, so
// neither can drift from the other.
func fullShow() *FetchedShow {
	return &FetchedShow{
		TMDBID:       1399,
		Name:         "Game of Thrones",
		Overview:     "Nine noble families.",
		PosterPath:   "/poster.jpg",
		BackdropPath: "/backdrop.jpg",
		TMDBStatus:   "Ended",
		Seasons:      []FetchedSeason{fetchedSeason(1, 2), fetchedSeason(2, 3)},
	}
}

// seedFullShow inserts a show with a poster, a backdrop and two seasons.
func seedFullShow(t *testing.T, app *App) Show {
	t.Helper()
	show, err := app.store.InsertShow(ctx(), fullShow())
	if err != nil {
		t.Fatalf("seeding show: %v", err)
	}
	return show
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// post sends a form submission. htmx requests carry the header htmx sets.
func post(t *testing.T, h http.Handler, path string, htmx bool, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// flashPage follows a redirect that set a flash message and returns the
// page the user lands on, with the message on it. A response that set no
// flash cookie simply lands on a page without one, and the assertion that
// wanted the message fails there rather than here.
func flashPage(t *testing.T, h http.Handler, rec *httptest.ResponseRecorder) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, rec.Header().Get("Location"), nil)
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie {
			req.AddCookie(c)
		}
	}
	page := httptest.NewRecorder()
	h.ServeHTTP(page, req)
	return page.Body.String()
}

func TestEveryPageRenders(t *testing.T) {
	app, handler := testApp(t)
	show := seedFullShow(t, app)

	cases := []struct {
		path   string
		status int
		want   string
	}{
		// Nothing is watched yet, so the show sits in the watch list and
		// the default Watching tab is empty.
		{"/", http.StatusOK, "Nothing in Watching yet."},
		{"/?tab=watchlist", http.StatusOK, "Game of Thrones"},
		{"/add", http.StatusOK, "Add a Show"},
		{"/settings", http.StatusOK, "TMDB API Key"},
		{"/show/" + itoa(show.ID), http.StatusOK, "Mark All Watched"},
		{"/show/999", http.StatusNotFound, "doesn't exist"},
		{"/show/not-a-number", http.StatusNotFound, "doesn't exist"},
		{"/nonsense", http.StatusNotFound, "doesn't exist"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := get(t, handler, c.path)
			if rec.Code != c.status {
				t.Errorf("status = %d, want %d", rec.Code, c.status)
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("body does not contain %q", c.want)
			}
		})
	}
}

// A show that has never been refreshed has no images at all, and a template
// that assumed otherwise would render a broken URL.
func TestImagesRenderAsRealURLs(t *testing.T) {
	app, handler := testApp(t)
	show := seedFullShow(t, app)

	body := get(t, handler, "/show/"+itoa(show.ID)).Body.String()
	for _, want := range []string{
		"url('https://image.tmdb.org/t/p/w500/poster.jpg')",
		"url('https://image.tmdb.org/t/p/w780/backdrop.jpg')",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("show page does not contain %q", want)
		}
	}

	// The dashboard writes its posters into a src attribute, which the
	// template escaper treats as a different context from url(...) above.
	dash := get(t, handler, "/?tab=watchlist").Body.String()
	if !strings.Contains(dash, `src="https://image.tmdb.org/t/p/w500/poster.jpg"`) {
		t.Error("dashboard poster is not a plain src URL")
	}

	// html/template writes this in place of a URL it won't vouch for.
	for name, page := range map[string]string{"show": body, "dashboard": dash} {
		if strings.Contains(page, "ZgotmplZ") {
			t.Errorf("an image URL was filtered out by the escaper on the %s page", name)
		}
	}
}

// The dashboard is the one page that can carry a hundred posters at once,
// so they must not all be fetched up front.
func TestDashboardPostersAreLazy(t *testing.T) {
	app, handler := testApp(t)
	seedFullShow(t, app)

	body := get(t, handler, "/?tab=watchlist").Body.String()
	if !strings.Contains(body, `loading="lazy"`) {
		t.Error("dashboard posters are not marked loading=lazy")
	}
	// A background-image is fetched whether or not it is on screen, which
	// is what the img replaced.
	if strings.Contains(body, "background-image") {
		t.Error("a dashboard poster is still a background-image")
	}
}

// A show with no poster still gets the placeholder, not a broken image.
func TestAShowWithNoPosterRendersThePlaceholder(t *testing.T) {
	app, handler := testApp(t)
	if _, err := app.store.InsertShow(ctx(), &FetchedShow{
		TMDBID: 42, Name: "No Art", TMDBStatus: "Ended",
		Seasons: []FetchedSeason{fetchedSeason(1, 1)},
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	body := get(t, handler, "/?tab=watchlist").Body.String()
	if !strings.Contains(body, ">No image<") {
		t.Error("a show with no poster should render the placeholder")
	}
	if strings.Contains(body, `src=""`) {
		t.Error("a show with no poster rendered an empty img src")
	}
}

func TestTheDashboardTabRemembersItselfAndCounts(t *testing.T) {
	app, handler := testApp(t)
	seedFullShow(t, app)

	// The seeded show has nothing watched, so it sits in the watch list -
	// and the default tab is Watching, which is therefore empty.
	rec := get(t, handler, "/")
	if !strings.Contains(rec.Body.String(), "Nothing in Watching yet.") {
		t.Error("the default tab should be Watching, and be empty")
	}
	if !strings.Contains(rec.Body.String(), `href="/?tab=watchlist"`) {
		t.Error("the tabs should link to each category")
	}

	// Choosing a tab is remembered for the next bare visit.
	rec = get(t, handler, "/?tab=watchlist")
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == tabCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != "watchlist" {
		t.Fatalf("tab cookie = %v", cookie)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Game of Thrones") {
		t.Error("the remembered tab should have been reopened")
	}
}

// The three htmx actions answer with out-of-band fragments, so one click
// updates the row, the season's progress line and the category badge.
func TestTogglingAnEpisodeAnswersWithSwappableFragments(t *testing.T) {
	app, handler := testApp(t)
	show := seedFullShow(t, app)
	detail, err := app.store.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	season := detail.Seasons[0]
	episode := season.Episodes[0]

	body := post(t, handler, "/episodes/"+itoa(episode.ID)+"/toggle", true, nil).Body.String()

	for _, want := range []string{
		`id="episodes-` + itoa(season.ID) + `"`,
		`id="season-progress-` + itoa(season.ID) + `"`,
		`id="category-badge"`,
		`id="episode-` + itoa(episode.ID) + `"`,
		`hx-swap-oob="true"`,
		"1 / 2 watched",
		"Watching", // the category badge, recomputed
		"checked",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response does not contain %q\n%s", want, body)
		}
	}

	// The row arrives inside its season's list rather than as a swap of its
	// own - the season is the unit. Both at once would be two fragments
	// claiming the same id in one response.
	if strings.Contains(body, `id="episode-`+itoa(episode.ID)+`" hx-swap-oob`) {
		t.Error("the episode row should not be swapped out-of-band on its own")
	}
	// The other season was not touched, so it is not in the answer.
	if other := detail.Seasons[1]; strings.Contains(body, `id="episodes-`+itoa(other.ID)+`"`) {
		t.Error("a season the toggle did not touch was swapped")
	}
}

func TestMarkingASeasonAnswersWithTheWholeSeason(t *testing.T) {
	app, handler := testApp(t)
	show := seedFullShow(t, app)
	detail, _ := app.store.Detail(ctx(), show.ID)
	season := detail.Seasons[0]

	body := post(t, handler, "/seasons/"+itoa(season.ID)+"/mark-watched", true, nil).Body.String()

	if !strings.Contains(body, "2 / 2 watched") {
		t.Error("the season's progress should be full")
	}
	if got := strings.Count(body, `id="episodes-`+itoa(season.ID)+`"`); got != 1 {
		t.Errorf("episode list swapped %d times, want 1", got)
	}
}

func TestMarkingAShowSwapsEverySeason(t *testing.T) {
	app, handler := testApp(t)
	show := seedFullShow(t, app)

	body := post(t, handler, "/shows/"+itoa(show.ID)+"/mark-watched", true, nil).Body.String()

	if got := strings.Count(body, `class="episode-list"`); got != 2 {
		t.Errorf("swapped %d episode lists, want 2", got)
	}
	if !strings.Contains(body, "Finished") {
		t.Error("an ended show with everything watched should read as Finished")
	}
}

// Without JavaScript the same actions are plain form posts, and have to
// land back on the show page having done the same thing.
func TestTheSameActionsWorkWithoutHTMX(t *testing.T) {
	app, handler := testApp(t)
	show := seedFullShow(t, app)
	detail, _ := app.store.Detail(ctx(), show.ID)
	episode := detail.Seasons[0].Episodes[0]

	rec := post(t, handler, "/episodes/"+itoa(episode.ID)+"/toggle", false, nil)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/show/"+itoa(show.ID) {
		t.Errorf("redirected to %q", got)
	}

	detail, err := app.store.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("re-reading detail: %v", err)
	}
	if updated := detail.Seasons[0].Episodes[0]; !updated.Watched {
		t.Errorf("episode watched = %v, want true", updated.Watched)
	}
}

func TestAddingAShowWithoutAnAPIKeyExplainsItself(t *testing.T) {
	_, handler := testApp(t)

	rec := post(t, handler, "/add", false, url.Values{"tmdb_id": {"1399"}})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "No TMDB API key set") {
		t.Error("the page should say what is missing")
	}
}

func TestAddingAnUnparseableIDIsRejectedBeforeTMDB(t *testing.T) {
	app, handler := testApp(t)
	if err := app.store.SetAPIKey(ctx(), "key"); err != nil {
		t.Fatalf("setting key: %v", err)
	}

	for _, raw := range []string{"", "   ", "0", "abc"} {
		rec := post(t, handler, "/add", false, url.Values{"tmdb_id": {raw}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want %d", raw, rec.Code, http.StatusBadRequest)
		}
		if !strings.Contains(rec.Body.String(), "valid TMDB ID") {
			t.Errorf("%q: the page should ask for a valid id", raw)
		}
	}
}

// ---------- fetching from TMDB ----------

func TestAddingAShowFetchesItAndLandsOnItsPage(t *testing.T) {
	app, handler := testApp(t)
	if err := app.store.SetAPIKey(ctx(), "key"); err != nil {
		t.Fatalf("setting key: %v", err)
	}

	rec := post(t, handler, "/add", false, url.Values{"tmdb_id": {"1399"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d\n%s", rec.Code, http.StatusSeeOther, rec.Body)
	}

	list, err := app.store.ListShows(ctx())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("%d shows tracked, want 1", len(list))
	}
	added := list[0]
	if got := rec.Header().Get("Location"); got != "/show/"+itoa(added.ID) {
		t.Errorf("redirected to %q, want the new show's page", got)
	}
	if added.Name != "Game of Thrones" {
		t.Errorf("name = %q", added.Name)
	}

	// The seasons and episodes came down too. An add that wrote only the
	// show row would still redirect and still flash.
	detail, err := app.store.Detail(ctx(), added.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if len(detail.Seasons) != 2 {
		t.Fatalf("%d seasons, want 2", len(detail.Seasons))
	}
	for i, want := range []int{2, 3} {
		if got := len(detail.Seasons[i].Episodes); got != want {
			t.Errorf("season %d has %d episodes, want %d", i+1, got, want)
		}
	}

	if page := flashPage(t, handler, rec); !strings.Contains(page, "Added") {
		t.Error("the show page should confirm the add")
	}
}

// TestAddingAShowSomeoneElseAddedFirstIsAConflictNotACrash covers the gap
// between the handler's tracked check and its insert. The check happens
// before a TMDB fetch that takes as long as it takes, so two adds of the
// same show can both pass it; until the transaction asked for itself, the
// second one hit the UNIQUE constraint and the user got a 500 for a case
// the add page already has a message for.
func TestAddingAShowSomeoneElseAddedFirstIsAConflictNotACrash(t *testing.T) {
	// Assembled by hand: the fake TMDB needs the store, and the app needs
	// the fake TMDB, so appWithTMDB cannot tie the knot.
	app := &App{store: testStore(t)}
	show := fullShow()
	var raced sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The other add lands while this fetch is in flight - after the
		// handler looked, before it wrote. Holding the fetch open is what
		// makes the race a certainty instead of a coin toss.
		raced.Do(func() {
			if _, err := app.store.InsertShow(ctx(), show); err != nil {
				t.Errorf("inserting behind the fetch: %v", err)
			}
		})
		serveTMDB(w, r, show)
	}))
	t.Cleanup(server.Close)
	app.tmdb = newTMDB(server.Client(), server.URL)
	handler := routes(app)

	if err := app.store.SetAPIKey(ctx(), "key"); err != nil {
		t.Fatalf("setting key: %v", err)
	}

	rec := post(t, handler, "/add", false, url.Values{"tmdb_id": {"1399"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d\n%s", rec.Code, http.StatusConflict, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), alreadyTrackedMsg) {
		t.Errorf("the page should say the show is already tracked:\n%s", rec.Body)
	}

	// The add that lost wrote nothing, so the show the winner stored is
	// still the only one and still whole.
	list, err := app.store.ListShows(ctx())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("%d shows tracked, want 1", len(list))
	}
	detail, err := app.store.Detail(ctx(), list[0].ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if len(detail.Seasons) != 2 {
		t.Errorf("%d seasons, want 2", len(detail.Seasons))
	}
}

// The comment at handlers.go:169 claims a failed fetch leaves no trace.
// Nothing checked it, and the cost of it being wrong is a TMDB id that can
// never be added again.
func TestAFailedFetchLeavesNothingTracked(t *testing.T) {
	// One server, down for the first add and healthy for the second: the
	// claim is about the same store accepting the same id afterwards, which
	// two separate servers could not show.
	var down atomic.Bool
	down.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, `{"success":false,"status_code":11}`, http.StatusInternalServerError)
			return
		}
		serveTMDB(w, r, fullShow())
	}))
	t.Cleanup(server.Close)

	app, handler := appWithTMDB(t, server)
	if err := app.store.SetAPIKey(ctx(), "key"); err != nil {
		t.Fatalf("setting key: %v", err)
	}

	rec := post(t, handler, "/add", false, url.Values{"tmdb_id": {"1399"}})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if !strings.Contains(rec.Body.String(), "TMDB is having trouble") {
		t.Errorf("the form should explain the upstream failure:\n%s", rec.Body)
	}
	// TMDB's error body belongs in the log, not on the page.
	if strings.Contains(rec.Body.String(), "status_code") {
		t.Error("TMDB's error body reached the page")
	}

	list, err := app.store.ListShows(ctx())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("%d shows tracked after a failed fetch, want 0", len(list))
	}

	// The id is still free. A show row written before the fetch would have
	// consumed it here, and IsTracked would answer 409 forever.
	down.Store(false)
	rec = post(t, handler, "/add", false, url.Values{"tmdb_id": {"1399"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("re-adding: status = %d, want %d\n%s", rec.Code, http.StatusSeeOther, rec.Body)
	}
	if list, err = app.store.ListShows(ctx()); err != nil || len(list) != 1 {
		t.Fatalf("%d shows tracked after the retry, want 1 (err %v)", len(list), err)
	}
}

func TestRefreshingAShowPullsNewEpisodes(t *testing.T) {
	// TMDB has gained an episode in season 1 since the show was added.
	upstream := fullShow()
	upstream.Seasons[0] = fetchedSeason(1, 3)

	app, handler := appWithTMDB(t, tmdbServer(t, upstream))
	show := seedFullShow(t, app)
	if err := app.store.SetAPIKey(ctx(), "key"); err != nil {
		t.Fatalf("setting key: %v", err)
	}

	// Marked before the refresh, so the mark has something to survive.
	detail, err := app.store.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("reading detail: %v", err)
	}
	if _, err := app.store.ToggleEpisode(ctx(), detail.Seasons[0].Episodes[0].ID); err != nil {
		t.Fatalf("marking: %v", err)
	}

	rec := post(t, handler, "/shows/"+itoa(show.ID)+"/refresh", false, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/show/"+itoa(show.ID) {
		t.Errorf("redirected to %q", got)
	}
	if page := flashPage(t, handler, rec); !strings.Contains(page, "Metadata refreshed.") {
		t.Error("the show page should confirm the refresh")
	}

	detail, err = app.store.Detail(ctx(), show.ID)
	if err != nil {
		t.Fatalf("re-reading detail: %v", err)
	}
	if got := len(detail.Seasons[0].Episodes); got != 3 {
		t.Fatalf("season 1 has %d episodes after the refresh, want 3", got)
	}
	if !detail.Seasons[0].Episodes[0].Watched {
		t.Error("the watched mark did not survive the refresh")
	}
	if detail.Seasons[0].Episodes[2].Watched {
		t.Error("the new episode arrived already watched")
	}
}

// Refreshing looks a show up by id before it talks to TMDB, so an id that is
// not a show is a 404 and not a failed refresh. This is the only test of that
// lookup: it used to be a store method of its own, and folding it into
// Store.Show left the behaviour resting on nothing.
func TestRefreshingAnUnknownShowIsNotFound(t *testing.T) {
	app, handler := testApp(t)
	if err := app.store.SetAPIKey(ctx(), "key"); err != nil {
		t.Fatalf("setting key: %v", err)
	}

	rec := post(t, handler, "/shows/999/refresh", false, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDeletingAShowRedirectsHomeWithAMessage(t *testing.T) {
	app, handler := testApp(t)
	show := seedFullShow(t, app)

	rec := post(t, handler, "/shows/"+itoa(show.ID)+"/delete", false, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}

	// The message survives the redirect in a cookie and is shown once.
	var flash *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie {
			flash = c
		}
	}
	if flash == nil {
		t.Fatal("no flash cookie was set")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(flash)
	home := httptest.NewRecorder()
	handler.ServeHTTP(home, req)
	if !strings.Contains(home.Body.String(), "Show deleted.") {
		t.Error("the dashboard should show the message")
	}
	if strings.Contains(home.Body.String(), "Game of Thrones") {
		t.Error("the show should be gone")
	}
}

func TestSettingsShowsAMaskedKeyOnly(t *testing.T) {
	app, handler := testApp(t)
	if err := app.store.SetAPIKey(ctx(), "abcdef1234567890"); err != nil {
		t.Fatalf("setting key: %v", err)
	}

	body := get(t, handler, "/settings").Body.String()
	if strings.Contains(body, "abcdef1234567890") {
		t.Error("the settings page must not echo the real key")
	}
	if !strings.Contains(body, "************7890") {
		t.Error("the settings page should show the masked key")
	}
}

func TestTheBackupDownloadIsARealDatabase(t *testing.T) {
	app, handler := testApp(t)
	seedFullShow(t, app)

	rec := get(t, handler, "/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment;") {
		t.Errorf("content-disposition = %q", rec.Header().Get("Content-Disposition"))
	}
	// Every SQLite file starts with this.
	if !strings.HasPrefix(rec.Body.String(), "SQLite format 3") {
		t.Error("the download is not a SQLite database")
	}
}

func TestStaticAssetsAreServedFromTheBinary(t *testing.T) {
	_, handler := testApp(t)

	for _, path := range []string{"/static/style.css", "/static/htmx.min.js"} {
		rec := get(t, handler, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty", path)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestRefreshAllCountsEveryShow covers refresh-all now that it works on
// several shows at once: the counters are written from many goroutines, and
// a show that fails must not be counted as refreshed. Run with -race, this
// is also the check that the shared state is guarded.
func TestRefreshAllCountsEveryShow(t *testing.T) {
	const (
		shows       = 12
		failingName = "/tv/5"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One show is missing from TMDB; the rest answer normally.
		if strings.HasPrefix(r.URL.Path, failingName) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"success":false,"status_code":34}`))
			return
		}
		if strings.Contains(r.URL.Path, "/season/") {
			w.Write([]byte(`{"episodes":[{"episode_number":1,"name":"Pilot","air_date":"2011-04-17"}]}`))
			return
		}
		w.Write([]byte(`{"id":1,"name":"Refreshed","status":"Ended",
			"seasons":[{"season_number":1,"name":"Season 1","episode_count":1}]}`))
	}))
	t.Cleanup(server.Close)

	app := &App{store: testStore(t), tmdb: newTMDB(server.Client(), server.URL)}
	handler := routes(app)
	if err := app.store.SetAPIKey(ctx(), "key"); err != nil {
		t.Fatalf("setting key: %v", err)
	}
	for i := range shows {
		if _, err := app.store.InsertShow(ctx(), &FetchedShow{
			TMDBID: int64(i), Name: "Show " + itoa(int64(i)), TMDBStatus: "Ended",
			Seasons: []FetchedSeason{fetchedSeason(1, 1)},
		}); err != nil {
			t.Fatalf("seeding show %d: %v", i, err)
		}
	}

	rec := post(t, handler, "/shows/refresh-all", false, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}

	var flash *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie {
			flash = c
		}
	}
	if flash == nil {
		t.Fatal("no flash cookie was set")
	}
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(flash)
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, req)

	want := "Refreshed 11 shows. 1 failed."
	if !strings.Contains(page.Body.String(), want) {
		t.Errorf("settings page does not report %q", want)
	}
	// The failure was a 404 from TMDB; its JSON stays in the log.
	if strings.Contains(page.Body.String(), "status_code") {
		t.Error("TMDB's error body reached the page")
	}

	// Every show that succeeded actually has the new metadata.
	list, err := app.store.ListShows(ctx())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	var renamed int
	for _, s := range list {
		if s.Name == "Refreshed" {
			renamed++
		}
	}
	if renamed != shows-1 {
		t.Errorf("%d shows were updated, want %d", renamed, shows-1)
	}
}

// TestRefreshAllStopsWhenTheClientGoesAway covers the path the counting
// test cannot reach. Refresh-all hands out work only while someone is
// waiting for the answer, so a cancelled run stops starting shows - and the
// shows it never started are not successes. Nothing else asserts that
// distinction: an unstarted show has no error recorded against it, which
// reads as "fine" unless the count knows how far the run actually got.
func TestRefreshAllStopsWhenTheClientGoesAway(t *testing.T) {
	const shows = 12

	// Every refresh is held open, so the run cannot finish on its own and
	// the only way out is the cancellation this test is about.
	release := make(chan struct{})
	defer close(release)
	var started atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		<-release
	}))
	t.Cleanup(server.Close)

	app := &App{store: testStore(t), tmdb: newTMDB(server.Client(), server.URL)}
	handler := routes(app)
	if err := app.store.SetAPIKey(ctx(), "key"); err != nil {
		t.Fatalf("setting key: %v", err)
	}
	for i := range shows {
		if _, err := app.store.InsertShow(ctx(), &FetchedShow{
			TMDBID: int64(i), Name: "Show " + itoa(int64(i)), TMDBStatus: "Ended",
			Seasons: []FetchedSeason{fetchedSeason(1, 1)},
		}); err != nil {
			t.Fatalf("seeding show %d: %v", i, err)
		}
	}

	// Built by hand rather than with post: this is the one request whose
	// context the test has to hold on to.
	reqCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/shows/refresh-all", nil).WithContext(reqCtx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	// Wait until every slot is busy. The loop is then parked handing out
	// the next one, which is exactly where cancellation has to be noticed.
	deadline := time.Now().Add(10 * time.Second)
	for started.Load() < refreshConcurrency {
		if time.Now().After(deadline) {
			t.Fatalf("%d refreshes started, want %d", started.Load(), refreshConcurrency)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("refresh-all did not return after the client went away")
	}

	// The shows that were running were all cut off mid-request, so none of
	// them succeeded. The rest were never begun, and must not be reported
	// as refreshed.
	want := fmt.Sprintf("Refreshed 0 shows. %d failed.", refreshConcurrency)
	if page := flashPage(t, handler, rec); !strings.Contains(page, want) {
		t.Errorf("settings page does not report %q", want)
	}
}
