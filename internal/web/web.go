// Package web serves a small read-mostly dashboard: the flag list, each
// flag's rules and its change history.
//
// It is server-rendered with no build step and no JavaScript framework,
// because a dashboard that needs a toolchain is a dashboard that stops
// working in a year. Toggling a flag is a form POST.
package web

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/MahyarNemati/feature-flags/internal/flag"
	"github.com/MahyarNemati/feature-flags/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Server serves the dashboard.
type Server struct {
	store store.Store
	tmpl  *template.Template
	log   *slog.Logger
	// token guards mutating actions. Empty disables them, leaving a
	// read-only dashboard rather than an open one.
	token string
}

// New builds a dashboard server.
func New(st store.Store, adminToken string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	tmpl, err := template.New("").Funcs(funcs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{store: st, tmpl: tmpl, log: log, token: adminToken}, nil
}

// Mount registers the dashboard routes on mux.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /flags/{key}", s.handleDetail)
	mux.HandleFunc("POST /flags/{key}/toggle", s.handleToggle)
	mux.HandleFunc("GET /history", s.handleHistory)
}

type indexData struct {
	Title    string
	Flags    []*flag.Flag
	ReadOnly bool
	Archived bool
	Changes  []store.Change
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("archived") == "true"
	flags, err := s.store.List(r.Context(), includeArchived)
	if err != nil {
		s.fail(w, "could not list flags", err)
		return
	}
	changes, err := s.store.History(r.Context(), "", 10)
	if err != nil {
		s.fail(w, "could not load history", err)
		return
	}
	s.render(w, "index.html", indexData{
		Title: "Flags", Flags: flags, ReadOnly: s.token == "",
		Archived: includeArchived, Changes: changes,
	})
}

type detailData struct {
	Title    string
	Flag     *flag.Flag
	Changes  []store.Change
	ReadOnly bool
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	f, err := s.store.Get(r.Context(), key)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "could not load flag", err)
		return
	}
	changes, err := s.store.History(r.Context(), key, 50)
	if err != nil {
		s.fail(w, "could not load history", err)
		return
	}
	s.render(w, "detail.html", detailData{Title: f.Key, Flag: f, Changes: changes, ReadOnly: s.token == ""})
}

// handleToggle flips a flag's enabled state. This is the one mutation the
// dashboard offers, because it is the one that matters in an incident: turn
// the feature off without editing its targeting.
func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	if s.token == "" {
		http.Error(w, "the dashboard is read-only: no admin token is configured", http.StatusForbidden)
		return
	}
	// Same-origin form post plus the admin token in a hidden field. Not a
	// substitute for real authentication, which belongs in front of this.
	if r.FormValue("token") != s.token {
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}

	key := r.PathValue("key")
	f, err := s.store.Get(r.Context(), key)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "could not load flag", err)
		return
	}

	version, _ := strconv.Atoi(r.FormValue("version"))
	f.Enabled = !f.Enabled
	comment := r.FormValue("comment")
	if comment == "" {
		comment = "toggled from the dashboard"
	}

	if _, err := s.store.Update(r.Context(), f, version, "dashboard", comment); err != nil {
		if errors.Is(err, store.ErrStaleVersion) {
			http.Error(w, "the flag changed while you were looking at it; reload and try again", http.StatusPreconditionFailed)
			return
		}
		s.fail(w, "could not update flag", err)
		return
	}
	http.Redirect(w, r, "/flags/"+key, http.StatusSeeOther)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	changes, err := s.store.History(r.Context(), "", 200)
	if err != nil {
		s.fail(w, "could not load history", err)
		return
	}
	s.render(w, "history.html", indexData{Title: "History", Changes: changes, ReadOnly: s.token == ""})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render", "template", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, msg string, err error) {
	s.log.Error(msg, "err", err)
	http.Error(w, msg, http.StatusInternalServerError)
}

func funcs() template.FuncMap {
	return template.FuncMap{
		"pct": func(basisPoints int) string {
			return strconv.FormatFloat(float64(basisPoints)/100, 'f', -1, 64) + "%"
		},
		"ago": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return strconv.Itoa(int(d.Minutes())) + "m ago"
			case d < 24*time.Hour:
				return strconv.Itoa(int(d.Hours())) + "h ago"
			default:
				return strconv.Itoa(int(d.Hours()/24)) + "d ago"
			}
		},
		"stamp": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 MST") },
		"value": func(v any) string {
			switch t := v.(type) {
			case string:
				return t
			case bool:
				return strconv.FormatBool(t)
			case float64:
				if t == float64(int64(t)) {
					return strconv.FormatInt(int64(t), 10)
				}
				return strconv.FormatFloat(t, 'f', -1, 64)
			default:
				return "…"
			}
		},
	}
}
