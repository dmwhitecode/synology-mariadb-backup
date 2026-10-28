package web

import (
	"net/http"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/schedule"
)

type settingsView struct {
	Schedule   schedulePart
	Retention  retentionPart
	Naming     namingPart
	Errors     []string
	Timezone   string
	BackupRoot string
	DataDir    string
}

func (s *Server) settingsView(d model.Defaults) settingsView {
	return settingsView{
		Schedule:   schedulePart{Schedule: d.Schedule, Modes: schedule.Modes},
		Retention:  retentionPart{Keep: d.RetentionKeep},
		Naming:     namingPart{Naming: d.Naming},
		Timezone:   timezone(),
		BackupRoot: s.backupRoot,
		DataDir:    s.dataDir,
	}
}

func (s *Server) settingsForm(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetDefaults(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "settings.html", "Settings", s.settingsView(d))
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var d model.Defaults
	var errs []string

	sc, err := parseSchedule(r)
	if err != nil {
		errs = append(errs, "Schedule: "+err.Error())
	}
	d.Schedule = sc

	keep, err := parseRetention(r)
	if err != nil {
		errs = append(errs, "Retention: "+err.Error())
	}
	d.RetentionKeep = keep

	no, err := parseNaming(r)
	if err != nil {
		errs = append(errs, "Filename: "+err.Error())
	}
	d.Naming = no

	if len(errs) > 0 {
		v := s.settingsView(d)
		v.Errors = errs
		s.render(w, r, "settings.html", "Settings", v)
		return
	}
	if err := s.store.SaveDefaults(r.Context(), d); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.sched.Reload(r.Context()); err != nil {
		s.log.Error("scheduler reload", "err", err)
	}
	s.log.Info("default settings saved")
	redirectMsg(w, r, "/settings", "Default settings saved.", "")
}
