package main

import (
	"bytes"
	"embed"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
)

//go:embed templates static
var assets embed.FS

// pages maps a page template to the parsed set that renders it: the layout,
// the shared fragments, and the page itself. Parsing the page last lets it
// override the layout's empty `topbar-left`, `topbar-nav` and `content`
// blocks.
var pages = map[string]*template.Template{}

// fragments renders the small pieces htmx swaps in on their own. They are
// parsed into every page set too, since the pages render the same markup on
// first load.
var fragments *template.Template

// The wrapper funcs let a page render the same fragments it will later
// swap, without repeating the markup: inline they are never out-of-band, so
// the flag is always false here. There is none for an episode row, which is
// only ever rendered inside its season's list and so needs no flag at all.
var templateFuncs = template.FuncMap{
	"posterURL":   posterURL,
	"backdropURL": backdropURL,
	"seasonView":  func(s SeasonDetail) seasonFragment { return seasonFragment{SeasonDetail: s} },
	"badge":       func(s Show) badgeFragment { return badgeFragment{Show: s} },
	"notice":      func(m Message) noticeFragment { return noticeFragment{Message: m} },
}

func init() {
	base := template.Must(template.New("base").Funcs(templateFuncs).
		ParseFS(assets, "templates/layout.html", "templates/fragments.html"))
	fragments = base

	for _, page := range []string{"index.html", "show.html", "add.html", "settings.html", "not_found.html"} {
		set := template.Must(base.Clone())
		pages[page] = template.Must(set.ParseFS(assets, "templates/"+page))
	}
}

// render writes a full page. Templates are executed into a buffer first so
// that a failure half-way through doesn't leave a half-written page with a
// 200 already on the wire.
//
// A HEAD request is answered by writing the body as normal: net/http eats
// it on the way out and reports its length, which is what a HEAD is for.
func render(w http.ResponseWriter, status int, page string, data any) {
	set, ok := pages[page]
	if !ok {
		log.Printf("no such page template: %s", page)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Printf("rendering %s: %v", page, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// renderFragments writes one or more named fragments as a single response
// body, which is how an htmx action answers: the piece that was clicked,
// plus whatever else on the page it changed, marked for out-of-band swaps.
func renderFragments(w http.ResponseWriter, parts ...fragment) {
	var buf bytes.Buffer
	for _, part := range parts {
		if err := fragments.ExecuteTemplate(&buf, part.name, part.data); err != nil {
			log.Printf("rendering fragment %s: %v", part.name, err)
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

type fragment struct {
	name string
	data any
}

// ---------- fragment wrappers ----------
//
// Each wrapper carries the same value the page renders plus an OOB flag.
// htmx swaps an element carrying `hx-swap-oob` wherever its id already
// exists on the page, which is how one click updates a season's episode
// list, its progress line and the show's category badge at once.

type seasonFragment struct {
	SeasonDetail
	OOB bool
}

type badgeFragment struct {
	Show
	OOB bool
}

type noticeFragment struct {
	Message
	OOB bool
}

// Message is something to tell the user about the last thing they did.
//
// Info is not a weaker Success: the refresh-all result is reported with it
// because that message can carry failures ("Refreshed 3 shows. 1 failed."),
// so it would be wrong in green.
type Message struct {
	Text string
	Tone string // "error", "success" or "" for neutral info
}

func errorMsg(text string) Message   { return Message{Text: text, Tone: "error"} }
func successMsg(text string) Message { return Message{Text: text, Tone: "success"} }
func infoMsg(text string) Message    { return Message{Text: text} }

// Class is the CSS class the notice renders with, matching the stylesheet's
// `.msg`, `.msg.error` and `.msg.success`.
func (m Message) Class() string {
	if m.Tone == "" {
		return "msg"
	}
	return "msg " + m.Tone
}

func (m Message) Empty() bool { return m.Text == "" }

// ---------- flash messages ----------

const flashCookie = "gotime_flash"

// setFlash stashes a message for the page a redirect is about to land on.
// Only the plain-form actions need it: an htmx action renders its own
// notice into the page it was fired from.
func setFlash(w http.ResponseWriter, m Message) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    url.QueryEscape(m.Tone + "|" + m.Text),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// takeFlash reads and clears the pending message, if any.
func takeFlash(w http.ResponseWriter, r *http.Request) Message {
	cookie, err := r.Cookie(flashCookie)
	if err != nil {
		return Message{}
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1})

	raw, err := url.QueryUnescape(cookie.Value)
	if err != nil {
		return Message{}
	}
	tone, text, ok := strings.Cut(raw, "|")
	if !ok {
		return Message{}
	}
	// The cookie is the user's to edit, and the tone becomes a CSS class.
	// Anything but the two known tones reads as neutral.
	if tone != "error" && tone != "success" {
		tone = ""
	}
	return Message{Text: text, Tone: tone}
}
