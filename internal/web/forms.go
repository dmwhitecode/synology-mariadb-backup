package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/naming"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/schedule"
)

// Partial view models shared by the connection form and the settings page.

type schedulePart struct {
	Schedule    schedule.Schedule
	Inherit     bool
	ShowInherit bool
	Modes       []schedule.Mode
	DefaultDesc string
}

type retentionPart struct {
	Keep        int
	Inherit     bool
	ShowInherit bool
	DefaultKeep int
}

type namingPart struct {
	Naming      naming.Options
	Inherit     bool
	ShowInherit bool
	DefaultDesc string
}

func checked(r *http.Request, name string) bool {
	v := strings.ToLower(r.FormValue(name))
	return v == "on" || v == "1" || v == "true"
}

func parseSchedule(r *http.Request) (schedule.Schedule, error) {
	s := schedule.Schedule{
		Mode: schedule.Mode(strings.TrimSpace(r.FormValue("schedule_mode"))),
		Time: strings.TrimSpace(r.FormValue("schedule_time")),
		Cron: strings.TrimSpace(r.FormValue("schedule_cron")),
	}
	if v := strings.TrimSpace(r.FormValue("schedule_every_hours")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return s, errors.New("every N hours must be a number")
		}
		s.EveryHours = n
	}
	for _, d := range r.Form["schedule_days"] {
		n, err := strconv.Atoi(d)
		if err != nil {
			return s, errors.New("invalid weekday")
		}
		s.Days = append(s.Days, n)
	}
	return s, s.Validate()
}

func parseRetention(r *http.Request) (int, error) {
	v := strings.TrimSpace(r.FormValue("retention_keep"))
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 10000 {
		return 0, errors.New("versions to keep must be a whole number between 1 and 10000")
	}
	return n, nil
}

func parseNaming(r *http.Request) (naming.Options, error) {
	o := naming.Options{
		UseDBName:       checked(r, "naming_use_db_name"),
		Base:            strings.TrimSpace(r.FormValue("naming_base")),
		Prefix:          r.FormValue("naming_prefix"),
		Suffix:          r.FormValue("naming_suffix"),
		TimestampPrefix: checked(r, "naming_ts_prefix"),
		TimestampSuffix: checked(r, "naming_ts_suffix"),
		TimestampFormat: strings.TrimSpace(r.FormValue("naming_ts_format")),
		Gzip:            checked(r, "naming_gzip"),
		Overwrite:       checked(r, "naming_overwrite"),
	}
	if o.TimestampFormat == "" {
		o.TimestampFormat = naming.DefaultTimestampFormat
	}
	return o, o.Validate()
}

// connectionView backs the add / edit form.
type connectionView struct {
	IsNew       bool
	ID          int64
	Name        string
	Host        string
	Port        int
	Database    string
	Username    string
	TLS         bool
	Enabled     bool
	HasPassword bool
	Slug        string

	Schedule  schedulePart
	Retention retentionPart
	Naming    namingPart
	Errors    []string
	Timezone  string
}

func newConnectionView(c *model.Connection, d model.Defaults, tz string) connectionView {
	v := connectionView{
		IsNew:    c == nil,
		Port:     model.DefaultPort,
		Enabled:  true,
		Timezone: tz,
		Schedule: schedulePart{Schedule: d.Schedule, Inherit: true, ShowInherit: true, Modes: schedule.Modes, DefaultDesc: d.Schedule.Describe()},
		Retention: retentionPart{Keep: d.RetentionKeep, Inherit: true, ShowInherit: true, DefaultKeep: d.RetentionKeep},
		Naming:    namingPart{Naming: d.Naming, Inherit: true, ShowInherit: true, DefaultDesc: describeNaming(d.Naming)},
	}
	if c == nil {
		return v
	}
	v.ID, v.Name, v.Host, v.Port, v.Database, v.Username = c.ID, c.Name, c.Host, c.Port, c.Database, c.Username
	v.TLS, v.Enabled, v.Slug = c.TLS, c.Enabled, c.Slug
	v.HasPassword = c.PasswordEnc != ""
	if c.Schedule != nil {
		v.Schedule.Schedule, v.Schedule.Inherit = *c.Schedule, false
	}
	if c.RetentionKeep != nil {
		v.Retention.Keep, v.Retention.Inherit = *c.RetentionKeep, false
	}
	if c.Naming != nil {
		v.Naming.Naming, v.Naming.Inherit = *c.Naming, false
	}
	return v
}

// parseConnectionForm reads the form into a connection. The returned
// password is plaintext ("" means unchanged). The view is returned populated
// so the form can be re-rendered with errors.
func parseConnectionForm(r *http.Request, existing *model.Connection, d model.Defaults, tz string) (*model.Connection, string, connectionView) {
	if err := r.ParseForm(); err != nil {
		v := newConnectionView(existing, d, tz)
		v.Errors = append(v.Errors, "could not read form")
		return nil, "", v
	}
	v := newConnectionView(existing, d, tz)
	v.Name = strings.TrimSpace(r.FormValue("name"))
	v.Host = strings.TrimSpace(r.FormValue("host"))
	v.Database = strings.TrimSpace(r.FormValue("database"))
	v.Username = strings.TrimSpace(r.FormValue("username"))
	v.TLS = checked(r, "tls")
	v.Enabled = checked(r, "enabled")
	password := r.FormValue("password")

	portStr := strings.TrimSpace(r.FormValue("port"))
	if portStr == "" {
		v.Port = model.DefaultPort
	} else if p, err := strconv.Atoi(portStr); err != nil || p < 1 || p > 65535 {
		v.Errors = append(v.Errors, "Port must be between 1 and 65535.")
	} else {
		v.Port = p
	}
	if v.Name == "" {
		v.Errors = append(v.Errors, "Name is required.")
	}
	if v.Host == "" {
		v.Errors = append(v.Errors, "Host is required.")
	}
	if v.Database == "" {
		v.Errors = append(v.Errors, "Database is required.")
	}
	if v.Username == "" {
		v.Errors = append(v.Errors, "Username is required.")
	}
	if existing == nil && password == "" {
		v.Errors = append(v.Errors, "Password is required.")
	}

	c := &model.Connection{
		Name: v.Name, Host: v.Host, Port: v.Port, Database: v.Database,
		Username: v.Username, TLS: v.TLS, Enabled: v.Enabled,
	}
	if existing != nil {
		c.ID, c.Slug, c.CreatedAt = existing.ID, existing.Slug, existing.CreatedAt
	}

	v.Schedule.Inherit = checked(r, "schedule_inherit")
	if !v.Schedule.Inherit {
		sc, err := parseSchedule(r)
		v.Schedule.Schedule = sc
		if err != nil {
			v.Errors = append(v.Errors, "Schedule: "+err.Error())
		} else {
			c.Schedule = &sc
		}
	}
	v.Retention.Inherit = checked(r, "retention_inherit")
	if !v.Retention.Inherit {
		keep, err := parseRetention(r)
		if err != nil {
			v.Errors = append(v.Errors, "Retention: "+err.Error())
		} else {
			v.Retention.Keep = keep
			c.RetentionKeep = &keep
		}
	}
	v.Naming.Inherit = checked(r, "naming_inherit")
	if !v.Naming.Inherit {
		no, err := parseNaming(r)
		v.Naming.Naming = no
		if err != nil {
			v.Errors = append(v.Errors, "Filename: "+err.Error())
		} else {
			c.Naming = &no
		}
	}
	return c, password, v
}

func describeNaming(o naming.Options) string {
	name, err := o.Build("database", exampleTime)
	if err != nil {
		return "invalid"
	}
	return name
}
