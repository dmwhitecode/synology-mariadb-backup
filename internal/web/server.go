// Package web serves the admin UI and the small JSON API behind it.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/scheduler"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/store"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/vault"
)

//go:embed templates static
var assets embed.FS

// Server holds the dependencies for every handler.
type Server struct {
	store      *store.Store
	vault      *vault.Vault
	sched      *scheduler.Scheduler
	backupRoot string
	dataDir    string
	version    string
	log        *slog.Logger
	tmpl       map[string]*template.Template
	limiter    *loginLimiter
}

// New builds the server and parses the embedded templates.
func New(st *store.Store, v *vault.Vault, sc *scheduler.Scheduler, backupRoot, dataDir, version string, log *slog.Logger) (*Server, error) {
	s := &Server{
		store: st, vault: v, sched: sc,
		backupRoot: backupRoot, dataDir: dataDir, version: version,
		log:     log,
		tmpl:    map[string]*template.Template{},
		limiter: newLoginLimiter(),
	}
	pages, err := fs.Glob(assets, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		name := strings.TrimPrefix(p, "templates/pages/")
		t, err := template.New("layout.html").Funcs(funcMap()).ParseFS(assets,
			"templates/layout.html", "templates/partials/*.html", p)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		s.tmpl[name] = t
	}
	return s, nil
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static)))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})

	// Unauthenticated.
	mux.HandleFunc("GET /setup", s.setupForm)
	mux.HandleFunc("POST /setup", s.sameOrigin(s.setupSubmit))
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.sameOrigin(s.loginSubmit))

	// Authenticated.
	auth := func(h http.HandlerFunc) http.HandlerFunc { return s.requireAuth(h) }
	post := func(h http.HandlerFunc) http.HandlerFunc { return s.requireAuth(s.sameOrigin(h)) }

	mux.HandleFunc("GET /{$}", auth(s.connectionsList))
	mux.HandleFunc("POST /logout", post(s.logout))
	mux.HandleFunc("GET /connections/new", auth(s.connectionNew))
	mux.HandleFunc("POST /connections/new", post(s.connectionCreate))
	mux.HandleFunc("GET /connections/{id}/edit", auth(s.connectionEdit))
	mux.HandleFunc("POST /connections/{id}/edit", post(s.connectionUpdate))
	mux.HandleFunc("POST /connections/{id}/delete", post(s.connectionDelete))
	mux.HandleFunc("POST /connections/{id}/toggle", post(s.connectionToggle))
	mux.HandleFunc("POST /connections/{id}/run", post(s.connectionRun))
	mux.HandleFunc("GET /connections/{id}/runs", auth(s.connectionRuns))
	mux.HandleFunc("GET /settings", auth(s.settingsForm))
	mux.HandleFunc("POST /settings", post(s.settingsSave))
	mux.HandleFunc("POST /settings/password", post(s.passwordChange))
	mux.HandleFunc("POST /api/test-connection", post(s.apiTestConnection))
	mux.HandleFunc("POST /api/naming-preview", post(s.apiNamingPreview))

	return securityHeaders(mux)
}

// --- middleware ------------------------------------------------------------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; form-action 'self'")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		w.Header().Set("Cache-Control", "public, max-age=3600")
	})
}

// sameOrigin rejects cross-site state-changing requests. Together with the
// SameSite=Strict session cookie this is the CSRF defence.
func (s *Server) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "origin mismatch", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		has, err := s.store.HasAdmin(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		if !has {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		if !s.sessionOK(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not logged in"})
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// --- rendering -------------------------------------------------------------

type page struct {
	Title    string
	Path     string
	Flash    string
	FlashErr string
	LoggedIn bool
	Version  string
	Data     any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	t, ok := s.tmpl[name]
	if !ok {
		s.fail(w, fmt.Errorf("no template %q", name))
		return
	}
	p := page{
		Title:    title,
		Path:     r.URL.Path,
		Flash:    r.URL.Query().Get("ok"),
		FlashErr: r.URL.Query().Get("err"),
		LoggedIn: s.sessionOK(r),
		Version:  s.version,
		Data:     data,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout.html", p); err != nil {
		s.log.Error("render", "template", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func redirectMsg(w http.ResponseWriter, r *http.Request, path, ok, errMsg string) {
	q := url.Values{}
	if ok != "" {
		q.Set("ok", ok)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "–"
			}
			return t.Local().Format("02 Jan 2006 15:04")
		},
		"fmtTimePtr": func(t *time.Time) string {
			if t == nil {
				return "–"
			}
			return t.Local().Format("02 Jan 2006 15:04:05")
		},
		"fmtDuration": func(d time.Duration) string {
			if d < time.Second {
				return d.Round(time.Millisecond).String()
			}
			return d.Round(time.Second).String()
		},
		"humanSize": humanSize,
		"dayName": func(d int) string {
			return []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}[d]
		},
		"seq": func(n int) []int {
			out := make([]int, n)
			for i := range out {
				out[i] = i
			}
			return out
		},
		"statusClass": func(st model.RunStatus) string { return "status-" + string(st) },
		"modeLabel": func(m string) string {
			switch m {
			case "hourly":
				return "Every hour"
			case "every_n_hours":
				return "Every N hours"
			case "daily":
				return "Daily"
			case "weekly":
				return "Weekly"
			case "cron":
				return "Cron expression"
			}
			return m
		},
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
