package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/scheduler"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/store"
)

type connectionRow struct {
	Conn         *model.Connection
	Last         *model.Run
	Next         time.Time
	ScheduleDesc string
	Running      bool
	Retention    int
}

type connectionsView struct {
	Rows       []connectionRow
	Timezone   string
	BackupRoot string
}

func timezone() string {
	name, _ := time.Now().Zone()
	loc := time.Local.String()
	if loc == "Local" {
		return name
	}
	return loc + " (" + name + ")"
}

func (s *Server) connectionsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	conns, err := s.store.ListConnections(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	defaults, err := s.store.GetDefaults(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	last, err := s.store.LastRuns(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	v := connectionsView{Timezone: timezone(), BackupRoot: s.backupRoot}
	for _, c := range conns {
		eff := c.Effective(defaults)
		v.Rows = append(v.Rows, connectionRow{
			Conn:         c,
			Last:         last[c.ID],
			Next:         s.sched.NextRun(c.ID),
			ScheduleDesc: eff.Schedule.Describe(),
			Running:      s.sched.IsRunning(c.ID),
			Retention:    eff.RetentionKeep,
		})
	}
	s.render(w, r, "connections.html", "Connections", v)
}

func (s *Server) connectionNew(w http.ResponseWriter, r *http.Request) {
	defaults, err := s.store.GetDefaults(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "connection_form.html", "Add connection", newConnectionView(nil, defaults, timezone()))
}

func (s *Server) connectionCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	defaults, err := s.store.GetDefaults(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	c, password, v := parseConnectionForm(r, nil, defaults, timezone())
	if len(v.Errors) > 0 {
		s.render(w, r, "connection_form.html", "Add connection", v)
		return
	}
	enc, err := s.vault.Encrypt(password)
	password = "" //nolint:ineffassign
	if err != nil {
		s.fail(w, err)
		return
	}
	c.PasswordEnc = enc
	if err := s.store.CreateConnection(ctx, c); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.sched.Reload(ctx); err != nil {
		s.log.Error("scheduler reload", "err", err)
	}
	s.log.Info("connection created", "connection", c.Name, "host", c.Host)
	redirectMsg(w, r, "/", "Connection \""+c.Name+"\" added.", "")
}

func (s *Server) loadConn(w http.ResponseWriter, r *http.Request) (*model.Connection, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	c, err := s.store.GetConnection(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return c, true
}

func (s *Server) connectionEdit(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConn(w, r)
	if !ok {
		return
	}
	defaults, err := s.store.GetDefaults(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "connection_form.html", "Edit connection", newConnectionView(c, defaults, timezone()))
}

func (s *Server) connectionUpdate(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.loadConn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	defaults, err := s.store.GetDefaults(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	c, password, v := parseConnectionForm(r, existing, defaults, timezone())
	if len(v.Errors) > 0 {
		s.render(w, r, "connection_form.html", "Edit connection", v)
		return
	}
	if password != "" {
		enc, err := s.vault.Encrypt(password)
		password = "" //nolint:ineffassign
		if err != nil {
			s.fail(w, err)
			return
		}
		c.PasswordEnc = enc
	}
	if err := s.store.UpdateConnection(ctx, c); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.sched.Reload(ctx); err != nil {
		s.log.Error("scheduler reload", "err", err)
	}
	s.log.Info("connection updated", "connection", c.Name)
	redirectMsg(w, r, "/", "Connection \""+c.Name+"\" saved.", "")
}

func (s *Server) connectionDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConn(w, r)
	if !ok {
		return
	}
	if s.sched.IsRunning(c.ID) {
		redirectMsg(w, r, "/", "", "A backup is running for \""+c.Name+"\". Wait for it to finish before deleting.")
		return
	}
	if err := s.store.DeleteConnection(r.Context(), c.ID); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.sched.Reload(r.Context()); err != nil {
		s.log.Error("scheduler reload", "err", err)
	}
	s.log.Info("connection deleted", "connection", c.Name)
	redirectMsg(w, r, "/", "Connection \""+c.Name+"\" deleted. Its backup files were left in place.", "")
}

func (s *Server) connectionToggle(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConn(w, r)
	if !ok {
		return
	}
	if err := s.store.SetConnectionEnabled(r.Context(), c.ID, !c.Enabled); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.sched.Reload(r.Context()); err != nil {
		s.log.Error("scheduler reload", "err", err)
	}
	msg := "Scheduled backups enabled for \"" + c.Name + "\"."
	if c.Enabled {
		msg = "Scheduled backups disabled for \"" + c.Name + "\"."
	}
	redirectMsg(w, r, "/", msg, "")
}

func (s *Server) connectionRun(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConn(w, r)
	if !ok {
		return
	}
	err := s.sched.RunAsync(c.ID, model.TriggerManual)
	if errors.Is(err, scheduler.ErrAlreadyRunning) {
		redirectMsg(w, r, "/", "", "A backup for \""+c.Name+"\" is already running.")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	redirectMsg(w, r, "/", "Backup started for \""+c.Name+"\". Refresh to see the result.", "")
}

type runsView struct {
	Conn *model.Connection
	Runs []*model.Run
}

func (s *Server) connectionRuns(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConn(w, r)
	if !ok {
		return
	}
	runs, err := s.store.ListRuns(r.Context(), c.ID, 100)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "runs.html", "History: "+c.Name, runsView{Conn: c, Runs: runs})
}
