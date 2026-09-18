package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const tmdbBaseURL = "https://api.themoviedb.org/3"

// maxInFlight bounds how many TMDB requests this process has open at once,
// counted across every show being refreshed rather than per show. One
// global cap is what lets refresh-all fetch several shows at a time without
// two separate limits multiplying into a burst TMDB would throttle.
const maxInFlight = 8

// minRequestInterval spaces out the starts of TMDB requests. The
// concurrency cap alone does not bound the rate - eight slots against fast
// answers measured over a hundred requests a second, and TMDB starts
// refusing around fifty - so the two work together: maxInFlight keeps the
// bursts small, this keeps the average under TMDB's limit whatever the
// network is doing that day.
const minRequestInterval = 25 * time.Millisecond

// TMDB is a client for the handful of TMDB endpoints this app uses. The key
// is passed per call because it lives in the database and can change while
// the process is running.
type TMDB struct {
	client *http.Client
	// baseURL is TMDB's API root. Only a test ever changes it.
	baseURL string
	// sem holds one token per request allowed in flight. Always build a
	// TMDB through newTMDB, or this is nil and every request blocks.
	sem chan struct{}

	// mu guards next, the earliest moment another request may start.
	mu   sync.Mutex
	next time.Time
}

func NewTMDB() *TMDB {
	return newTMDB(&http.Client{Timeout: 20 * time.Second}, tmdbBaseURL)
}

func newTMDB(client *http.Client, baseURL string) *TMDB {
	return &TMDB{
		client:  client,
		baseURL: baseURL,
		sem:     make(chan struct{}, maxInFlight),
	}
}

// FetchedShow is show metadata already read from TMDB, in the shape the
// store wants to write. Keeping it separate from the JSON structs below is
// what stops the store depending on a third party's wire format.
type FetchedShow struct {
	TMDBID       int64
	Name         string
	Overview     string
	PosterPath   string
	BackdropPath string
	TMDBStatus   string
	Seasons      []FetchedSeason
}

type FetchedSeason struct {
	Number       int64
	Name         string
	EpisodeCount int64
	Episodes     []FetchedEpisode
}

type FetchedEpisode struct {
	Number  int64
	Name    string
	AirDate string
}

// The subset of TMDB's JSON this app reads.
type tmdbShow struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
	Status       string `json:"status"`
	Seasons      []struct {
		SeasonNumber int64  `json:"season_number"`
		Name         string `json:"name"`
		EpisodeCount int64  `json:"episode_count"`
	} `json:"seasons"`
}

type tmdbSeason struct {
	Episodes []struct {
		EpisodeNumber int64  `json:"episode_number"`
		Name          string `json:"name"`
		AirDate       string `json:"air_date"`
	} `json:"episodes"`
}

// get performs one authenticated TMDB request and decodes it into out.
// Every request in the process passes through here, which is where the
// in-flight cap is applied.
func (t *TMDB) get(ctx context.Context, path, apiKey string, out any) error {
	endpoint := t.baseURL + path + "?" + url.Values{"api_key": {apiKey}}.Encode()

	// Waiting for a slot is itself cancellable, so a caller that gives up
	// doesn't leave goroutines queued behind a cap they no longer need.
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	// Pace after taking a slot, not before, so a reserved moment is always
	// used by a request that is about to run rather than by one still
	// queued behind the cap.
	if err := t.pace(ctx); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: request failed: %w", errTMDB, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Bounded read: an error body is a short JSON object, and it is
		// only ever logged.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &tmdbError{Status: resp.Status, StatusCode: resp.StatusCode, Body: string(body)}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: response could not be read: %w", errTMDB, err)
	}
	return nil
}

// pace blocks until this request's turn in the schedule. Each caller
// claims the next free moment and moves the marker on, so requests leave at
// a steady rate however many goroutines are waiting.
func (t *TMDB) pace(ctx context.Context) error {
	t.mu.Lock()
	now := time.Now()
	if t.next.Before(now) {
		t.next = now
	}
	wait := t.next.Sub(now)
	t.next = t.next.Add(minRequestInterval)
	t.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ValidateKey is a cheap call to check whether TMDB accepts an API key,
// without needing to know any show or season id.
func (t *TMDB) ValidateKey(ctx context.Context, apiKey string) error {
	var discard json.RawMessage
	return t.get(ctx, "/authentication", apiKey, &discard)
}

// FetchShow reads a show and every one of its seasons. Nothing is written
// here: the caller hands the whole result to the store, which applies it in
// one transaction. Fetching everything up front is what makes that
// atomicity possible - a failure part-way through leaves no trace in the
// database.
func (t *TMDB) FetchShow(ctx context.Context, tmdbID int64, apiKey string) (*FetchedShow, error) {
	var show tmdbShow
	if err := t.get(ctx, fmt.Sprintf("/tv/%d", tmdbID), apiKey, &show); err != nil {
		return nil, err
	}

	fetched := &FetchedShow{
		TMDBID:       show.ID,
		Name:         show.Name,
		Overview:     show.Overview,
		PosterPath:   show.PosterPath,
		BackdropPath: show.BackdropPath,
		TMDBStatus:   show.Status,
		Seasons:      make([]FetchedSeason, len(show.Seasons)),
	}

	// Results are written into their own slot rather than appended, so the
	// seasons keep TMDB's order however the requests interleave.
	// No limit here: get bounds the requests process-wide, so a
	// forty-season show queues forty goroutines rather than firing forty
	// requests.
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i, summary := range show.Seasons {
		wg.Add(1)
		go func(i int, number int64, name string, episodeCount int64) {
			defer wg.Done()

			var season tmdbSeason
			err := t.get(ctx, fmt.Sprintf("/tv/%d/season/%d", tmdbID, number), apiKey, &season)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					// Nothing partial is usable, so stop the rest early.
					cancel()
				}
				return
			}
			episodes := make([]FetchedEpisode, len(season.Episodes))
			for j, e := range season.Episodes {
				episodes[j] = FetchedEpisode{Number: e.EpisodeNumber, Name: e.Name, AirDate: e.AirDate}
			}
			fetched.Seasons[i] = FetchedSeason{
				Number:       number,
				Name:         name,
				EpisodeCount: episodeCount,
				Episodes:     episodes,
			}
		}(i, summary.SeasonNumber, summary.Name, summary.EpisodeCount)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return fetched, nil
}

// errTMDB marks every failure that came from talking to TMDB. refreshOne
// reads the database, calls TMDB, then writes the database, so its caller
// needs to know which half of that failed before it picks a message.
var errTMDB = errors.New("tmdb")

// tmdbError is a non-2xx answer from TMDB. The status is kept apart from
// the body so a caller can tell "no such show" from "bad key" by code
// rather than by matching on message text. The body is for the log:
// userMessage is what a person should see.
type tmdbError struct {
	Status     string
	StatusCode int
	Body       string
}

func (e *tmdbError) Error() string {
	return fmt.Sprintf("TMDB returned %s: %s", e.Status, e.Body)
}

func (e *tmdbError) Unwrap() error { return errTMDB }

// tmdbMessage turns a failure into one plain sentence fit for the page.
// TMDB's own error JSON is not it: that body goes to the log, and the user
// gets a line that says what to do about it.
//
// An error that did not come from TMDB gets a generic line rather than a
// wrong one, since the detail there would be a SQL string.
func tmdbMessage(err error) string {
	if !errors.Is(err, errTMDB) {
		return "Something went wrong. The details are in the server log."
	}

	var apiErr *tmdbError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusNotFound:
			return "TMDB has no show with that ID."
		case apiErr.StatusCode == http.StatusUnauthorized,
			apiErr.StatusCode == http.StatusForbidden:
			return "TMDB did not accept the API key."
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return "TMDB is rate-limiting requests. Try again in a moment."
		case apiErr.StatusCode >= 500:
			return "TMDB is having trouble right now. Try again in a moment."
		}
		return "TMDB refused the request."
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "TMDB took too long to answer. Try again in a moment."
	case errors.Is(err, context.Canceled):
		return "The request was cancelled before TMDB answered."
	}
	return "Could not reach TMDB. Check the connection and try again."
}
