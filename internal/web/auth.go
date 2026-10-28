package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie   = "mbk_session"
	sessionLifetime = 30 * 24 * time.Hour
	minPasswordLen  = 10
	maxFailures     = 5
	lockout         = 60 * time.Second
)

// dummyHash keeps bcrypt timing constant when the username is wrong.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)

// --- login rate limiting ---------------------------------------------------

type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	count int
	until time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{fails: map[string]*failRecord{}} }

func (l *loginLimiter) blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.fails[key]
	if !ok || time.Now().After(rec.until) {
		return false, 0
	}
	return true, time.Until(rec.until)
}

func (l *loginLimiter) failure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.fails[key]
	if !ok {
		rec = &failRecord{}
		l.fails[key] = rec
	}
	rec.count++
	if rec.count >= maxFailures {
		rec.until = time.Now().Add(lockout)
		rec.count = 0
	}
}

func (l *loginLimiter) success(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- sessions --------------------------------------------------------------

func (s *Server) sessionOK(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	ok, err := s.store.SessionValid(r.Context(), c.Value)
	return err == nil && ok
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := hex.EncodeToString(raw)
	exp := time.Now().Add(sessionLifetime)
	if err := s.store.CreateSession(r.Context(), token, exp); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// --- first-run setup -------------------------------------------------------

type setupView struct {
	Username string
	Error    string
}

func (s *Server) setupForm(w http.ResponseWriter, r *http.Request) {
	if has, _ := s.store.HasAdmin(r.Context()); has {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, "setup.html", "First-run setup", setupView{Username: "admin"})
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if has, _ := s.store.HasAdmin(r.Context()); has {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")
	v := setupView{Username: username}
	switch {
	case username == "":
		v.Error = "Username is required."
	case len(password) < minPasswordLen:
		v.Error = "Password must be at least 10 characters."
	case password != confirm:
		v.Error = "Passwords do not match."
	}
	if v.Error != "" {
		s.render(w, r, "setup.html", "First-run setup", v)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.CreateAdmin(r.Context(), username, string(hash)); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.startSession(w, r); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("admin account created", "username", username)
	redirectMsg(w, r, "/", "Admin account created. Add your first connection.", "")
}

// --- login / logout --------------------------------------------------------

type loginView struct {
	Username string
	Error    string
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if has, _ := s.store.HasAdmin(r.Context()); !has {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if s.sessionOK(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login.html", "Log in", loginView{})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	key := clientKey(r)
	if blocked, wait := s.limiter.blocked(key); blocked {
		s.render(w, r, "login.html", "Log in", loginView{Error: "Too many failed attempts. Try again in " + wait.Round(time.Second).String() + "."})
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	storedUser, storedHash, err := s.store.GetAdmin(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	hash := []byte(storedHash)
	userMatch := subtle.ConstantTimeCompare([]byte(storedUser), []byte(username)) == 1
	if !userMatch {
		hash = dummyHash
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || !userMatch {
		s.limiter.failure(key)
		s.log.Warn("failed login", "username", username, "from", key)
		s.render(w, r, "login.html", "Log in", loginView{Username: username, Error: "Incorrect username or password."})
		return
	}
	s.limiter.success(key)
	if err := s.startSession(w, r); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.endSession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// passwordChange handles the form on the settings page.
func (s *Server) passwordChange(w http.ResponseWriter, r *http.Request) {
	current := r.FormValue("current_password")
	next := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	_, storedHash, err := s.store.GetAdmin(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	switch {
	case bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(current)) != nil:
		redirectMsg(w, r, "/settings", "", "Current password is incorrect.")
		return
	case len(next) < minPasswordLen:
		redirectMsg(w, r, "/settings", "", "New password must be at least 10 characters.")
		return
	case next != confirm:
		redirectMsg(w, r, "/settings", "", "New passwords do not match.")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.UpdateAdminPassword(r.Context(), string(hash)); err != nil {
		s.fail(w, err)
		return
	}
	// Invalidate every other session, then keep this browser logged in.
	_ = s.store.DeleteAllSessions(r.Context())
	if err := s.startSession(w, r); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("admin password changed")
	redirectMsg(w, r, "/settings", "Password changed. Other sessions have been logged out.", "")
}
