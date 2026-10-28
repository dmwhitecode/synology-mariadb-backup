// Package config reads runtime configuration from environment variables.
package config

import (
	"os"
	"strings"
)

// Config holds everything the process needs to know at startup.
type Config struct {
	// DataDir holds the SQLite database and the master key file.
	DataDir string
	// BackupDir is the root under which dump files are written.
	BackupDir string
	// Listen is the HTTP listen address, e.g. ":8080".
	Listen string
	// DumpBinary is the mariadb-dump executable to run.
	DumpBinary string
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
}

// Load builds a Config from the environment, applying defaults.
func Load() Config {
	return Config{
		DataDir:    env("DATA_DIR", "/data"),
		BackupDir:  env("BACKUP_DIR", "/backups"),
		Listen:     env("LISTEN", ":8080"),
		DumpBinary: env("MARIADB_DUMP", "mariadb-dump"),
		LogLevel:   strings.ToLower(env("LOG_LEVEL", "info")),
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
