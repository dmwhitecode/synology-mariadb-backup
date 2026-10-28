package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/dumper"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/naming"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/store"
)

// exampleTime is the fixed instant used for filename previews.
var exampleTime = time.Date(2026, 1, 31, 14, 30, 5, 0, time.Local)

const maxJSONBody = 64 * 1024

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("expected application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

type testConnectionRequest struct {
	ID       int64  `json:"id"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
	TLS      bool   `json:"tls"`
}

// apiTestConnection probes the server with the values currently in the
// form. Nothing is persisted. If no password was typed and the form is for
// an existing connection, the stored password is used.
func (s *Server) apiTestConnection(w http.ResponseWriter, r *http.Request) {
	var req testConnectionRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad request: " + err.Error()})
		return
	}
	if req.Port == 0 {
		req.Port = model.DefaultPort
	}
	password := req.Password
	if password == "" && req.ID > 0 {
		c, err := s.store.GetConnection(r.Context(), req.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.fail(w, err)
			return
		}
		if c != nil {
			password, err = s.vault.Decrypt(c.PasswordEnc)
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "stored password could not be decrypted; enter it again"})
				return
			}
		}
	}
	if password == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "enter a password to test"})
		return
	}
	target := dumper.Target{
		Host: strings.TrimSpace(req.Host), Port: req.Port, Database: strings.TrimSpace(req.Database),
		Username: strings.TrimSpace(req.Username), Password: password, TLS: req.TLS,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res, err := dumper.TestConnection(ctx, target)
	if err != nil {
		s.log.Info("connection test failed", "host", target.Host, "err", err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"version":          res.Version,
		"database_visible": res.DatabaseVisible,
		"latency":          res.Latency,
	})
}

type namingPreviewRequest struct {
	Database string         `json:"database"`
	Naming   naming.Options `json:"naming"`
}

func (s *Server) apiNamingPreview(w http.ResponseWriter, r *http.Request) {
	var req namingPreviewRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad request"})
		return
	}
	db := strings.TrimSpace(req.Database)
	if db == "" {
		db = "database"
	}
	name, err := req.Naming.Build(db, exampleTime)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "filename": name})
}
