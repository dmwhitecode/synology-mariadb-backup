// Package model holds the data types shared between the store, scheduler
// and web layers.
package model

import (
	"time"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/naming"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/schedule"
)

// DefaultPort is the standard MariaDB port.
const DefaultPort = 3306

// Connection is a remote MariaDB database to back up. PasswordEnc is the
// vault ciphertext; the plaintext never lives on this struct.
type Connection struct {
	ID          int64
	Name        string
	Slug        string
	Host        string
	Port        int
	Database    string
	Username    string
	PasswordEnc string
	TLS         bool
	Enabled     bool

	// Overrides. nil means "inherit the global default".
	Schedule      *schedule.Schedule
	RetentionKeep *int
	Naming        *naming.Options

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Defaults are the global settings a connection inherits unless overridden.
type Defaults struct {
	Schedule      schedule.Schedule `json:"schedule"`
	RetentionKeep int               `json:"retention_keep"`
	Naming        naming.Options    `json:"naming"`
}

// DefaultDefaults is what a fresh install starts with.
func DefaultDefaults() Defaults {
	return Defaults{
		Schedule:      schedule.Default(),
		RetentionKeep: 7,
		Naming:        naming.Default(),
	}
}

// Effective resolves the connection's settings against the global defaults.
type Effective struct {
	Schedule      schedule.Schedule
	RetentionKeep int
	Naming        naming.Options
}

// Effective returns the settings that actually apply to the connection.
func (c *Connection) Effective(d Defaults) Effective {
	e := Effective{Schedule: d.Schedule, RetentionKeep: d.RetentionKeep, Naming: d.Naming}
	if c.Schedule != nil {
		e.Schedule = *c.Schedule
	}
	if c.RetentionKeep != nil {
		e.RetentionKeep = *c.RetentionKeep
	}
	if c.Naming != nil {
		e.Naming = *c.Naming
	}
	return e
}

// RunStatus is the outcome of a backup run.
type RunStatus string

const (
	StatusRunning RunStatus = "running"
	StatusSuccess RunStatus = "success"
	StatusFailed  RunStatus = "failed"
	StatusSkipped RunStatus = "skipped"
)

// Trigger records what started a run.
const (
	TriggerScheduled = "scheduled"
	TriggerManual    = "manual"
)

// Run is one backup attempt.
type Run struct {
	ID           int64
	ConnectionID int64
	StartedAt    time.Time
	FinishedAt   *time.Time
	Status       RunStatus
	Trigger      string
	File         string
	Size         int64
	// Error is already scrubbed of credentials.
	Error string
}

// Duration returns how long the run took, or how long it has been running.
func (r Run) Duration() time.Duration {
	if r.FinishedAt == nil {
		return time.Since(r.StartedAt)
	}
	return r.FinishedAt.Sub(r.StartedAt)
}
