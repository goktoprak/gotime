package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTMDB serves the shape of the real API for one show with `seasons`
// seasons of two episodes each.
func fakeTMDB(t *testing.T, seasons int, failSeason int) (*TMDB, *int32) {
	t.Helper()
	var requests int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.URL.Query().Get("api_key") != "good-key" {
			http.Error(w, `{"status_message":"Invalid API key"}`, http.StatusUnauthorized)
			return
		}

		var number int
		if n, _ := fmt.Sscanf(r.URL.Path, "/tv/1399/season/%d", &number); n == 1 {
			if number == failSeason {
				http.Error(w, `{"status_message":"Not found"}`, http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"episodes":[
				{"episode_number":1,"name":"S%dE1","air_date":"2011-04-17"},
				{"episode_number":2,"name":"S%dE2","air_date":null}
			]}`, number, number)
			return
		}

		if r.URL.Path == "/tv/1399" {
			var list []string
			for i := range seasons {
				list = append(list, fmt.Sprintf(
					`{"season_number":%d,"name":"Season %d","episode_count":2}`, i, i))
			}
			fmt.Fprintf(w, `{"id":1399,"name":"Game of Thrones","overview":"Nine noble families.",
				"poster_path":"/poster.jpg","backdrop_path":null,"status":"Ended","seasons":[%s]}`,
				strings.Join(list, ","))
			return
		}

		w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	return newTMDB(server.Client(), server.URL), &requests
}

func TestFetchShowReadsEverySeasonInOrder(t *testing.T) {
	// More seasons than may be in flight at once, so the ordering below is
	// a real claim about the concurrency and not an accident.
	tmdb, _ := fakeTMDB(t, maxInFlight*3, -1)

	show, err := tmdb.FetchShow(context.Background(), 1399, "good-key")
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}

	if show.Name != "Game of Thrones" || show.TMDBStatus != "Ended" {
		t.Errorf("show = %q / %q", show.Name, show.TMDBStatus)
	}
	if show.PosterPath != "/poster.jpg" {
		t.Errorf("poster = %q", show.PosterPath)
	}
	// A null in the JSON is an absent value, not the string "null".
	if show.BackdropPath != "" {
		t.Errorf("backdrop = %q, want empty", show.BackdropPath)
	}
	if len(show.Seasons) != maxInFlight*3 {
		t.Fatalf("got %d seasons, want %d", len(show.Seasons), maxInFlight*3)
	}
	for i, season := range show.Seasons {
		if season.Number != int64(i) {
			t.Errorf("season %d is numbered %d - order was not preserved", i, season.Number)
		}
		if len(season.Episodes) != 2 {
			t.Errorf("season %d has %d episodes, want 2", i, len(season.Episodes))
		}
		if season.Episodes[0].Name != fmt.Sprintf("S%dE1", i) {
			t.Errorf("season %d episode 1 is %q", i, season.Episodes[0].Name)
		}
	}
	if show.Seasons[0].Episodes[1].AirDate != "" {
		t.Error("an unaired episode should have no air date")
	}
}

// One failed season fails the whole fetch: the caller writes all of it or
// none of it, so a partial result would be worse than an error.
func TestOneFailedSeasonFailsTheFetch(t *testing.T) {
	tmdb, _ := fakeTMDB(t, 4, 2)

	_, err := tmdb.FetchShow(context.Background(), 1399, "good-key")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to mention the status", err)
	}
}

func TestARejectedKeyIsReported(t *testing.T) {
	tmdb, requests := fakeTMDB(t, 2, -1)

	err := tmdb.ValidateKey(context.Background(), "bad-key")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("error = %v, want TMDB's own message", err)
	}

	if _, err := tmdb.FetchShow(context.Background(), 1399, "bad-key"); err == nil {
		t.Fatal("expected the fetch to fail too")
	}
	// The show request failed, so no season request should have followed.
	if got := atomic.LoadInt32(requests); got != 2 {
		t.Errorf("made %d requests, want 2", got)
	}
}

func TestAGoodKeyValidates(t *testing.T) {
	tmdb, _ := fakeTMDB(t, 1, -1)
	if err := tmdb.ValidateKey(context.Background(), "good-key"); err != nil {
		t.Errorf("validating: %v", err)
	}
}

// TestTMDBErrorsBecomeSentences is the guard against showing a person
// TMDB's raw error JSON, which is what a bad show ID used to produce.
func TestTMDBErrorsBecomeSentences(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not found", &tmdbError{Status: "404 Not Found", StatusCode: 404,
			Body: `{"success":false,"status_code":34,"status_message":"The resource you requested could not be found."}`},
			"TMDB has no show with that ID."},
		{"bad key", &tmdbError{Status: "401 Unauthorized", StatusCode: 401, Body: `{"status_code":7}`},
			"TMDB did not accept the API key."},
		{"rate limited", &tmdbError{Status: "429 Too Many Requests", StatusCode: 429},
			"TMDB is rate-limiting requests. Try again in a moment."},
		{"tmdb down", &tmdbError{Status: "503 Service Unavailable", StatusCode: 503},
			"TMDB is having trouble right now. Try again in a moment."},
		{"unreachable", fmt.Errorf("%w: request failed: %w", errTMDB, errors.New("dial tcp: no route to host")),
			"Could not reach TMDB. Check the connection and try again."},
		{"timed out", fmt.Errorf("%w: %w", errTMDB, context.DeadlineExceeded),
			"TMDB took too long to answer. Try again in a moment."},
		// Not a TMDB failure at all: refreshOne also reads and writes the
		// database, and a SQL error must not be reported as a TMDB one.
		{"not tmdb", errors.New("no such column: bogus"),
			"Something went wrong. The details are in the server log."},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tmdbMessage(c.err)
			if got != c.want {
				t.Errorf("tmdbMessage() = %q, want %q", got, c.want)
			}
			// Whatever the case, none of TMDB's own wire text leaks.
			for _, leak := range []string{"status_code", "{", "}", "dial tcp", "no such column"} {
				if strings.Contains(got, leak) {
					t.Errorf("message %q leaks %q", got, leak)
				}
			}
		})
	}
}

// TestRequestsAreCappedInFlight covers the promise refresh-all relies on:
// running several shows at once cannot multiply into a burst, because the
// cap is counted across the whole process.
func TestRequestsAreCappedInFlight(t *testing.T) {
	var (
		mu      sync.Mutex
		inParts int
		peak    int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inParts++
		if inParts > peak {
			peak = inParts
		}
		mu.Unlock()

		// Hold each request open for longer than a full cap's worth of
		// pacing (maxInFlight * minRequestInterval), so that without the
		// cap more than maxInFlight would pile up here.
		time.Sleep(2 * maxInFlight * minRequestInterval)

		mu.Lock()
		inParts--
		mu.Unlock()

		if strings.Contains(r.URL.Path, "/season/") {
			w.Write([]byte(`{"episodes":[]}`))
			return
		}
		seasons := make([]string, 0, 10)
		for i := range 10 {
			seasons = append(seasons, fmt.Sprintf(`{"season_number":%d,"name":"S%d","episode_count":0}`, i, i))
		}
		fmt.Fprintf(w, `{"id":1,"name":"X","status":"Ended","seasons":[%s]}`, strings.Join(seasons, ","))
	}))
	t.Cleanup(server.Close)

	tmdb := newTMDB(server.Client(), server.URL)

	// Four shows at once, eleven requests each: without a shared cap the
	// peak would be far above maxInFlight.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tmdb.FetchShow(context.Background(), 1, "key"); err != nil {
				t.Errorf("fetching: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if peak > maxInFlight {
		t.Errorf("peak of %d requests in flight, cap is %d", peak, maxInFlight)
	}
	if peak <= maxInFlight/2 {
		t.Errorf("peak of only %d means the cap was never under pressure - the test proves nothing", peak)
	}
}

// TestRequestsArePaced is the other half of the cap: concurrency bounds the
// burst, this bounds the average. Without it a fast network turns eight
// slots into well over a hundred requests a second.
func TestRequestsArePaced(t *testing.T) {
	var (
		mu     sync.Mutex
		starts []time.Time
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	tmdb := newTMDB(server.Client(), server.URL)

	const n = 20
	var wg sync.WaitGroup
	begin := time.Now()
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tmdb.ValidateKey(context.Background(), "key"); err != nil {
				t.Errorf("request: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(begin)

	// n requests cannot be crammed into less than (n-1) intervals.
	if floor := time.Duration(n-1) * minRequestInterval; elapsed < floor {
		t.Errorf("%d requests took %v, faster than the pacing floor of %v", n, elapsed, floor)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(starts) != n {
		t.Fatalf("server saw %d requests, want %d", len(starts), n)
	}
	// And the rate that implies stays under what TMDB tolerates.
	if rate := float64(n) / elapsed.Seconds(); rate > 50 {
		t.Errorf("issued %.0f requests/second, TMDB refuses around 50", rate)
	}
}
