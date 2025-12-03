package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"replication-service/internal/core/domain"
	"replication-service/internal/core/ports"
)

// PgpassManager handles the creation and cleanup of the .pgpass file.
type PgpassManager struct {
	logger      ports.Logger
	pgpassPath  string
	bucardoUser string
}

// NewPgpassManager creates a new PgpassManager.
func NewPgpassManager(logger ports.Logger, pgpassPath, bucardoUser string) *PgpassManager {
	return &PgpassManager{
		logger:      logger,
		pgpassPath:  pgpassPath,
		bucardoUser: bucardoUser,
	}
}

// SetupPgpass creates a single .pgpass file containing credentials for all databases.
func (m *PgpassManager) SetupPgpass(ctx context.Context, dbs []domain.Database) error {
	m.logger.Info("Setting up .pgpass file", "path", m.pgpassPath)
	os.Remove(m.pgpassPath) // Ignore error if it doesn't exist

	for _, db := range dbs {
		password, err := m.getDbPassword(db)
		if err != nil {
			return fmt.Errorf("failed to get password for .pgpass setup for db %d: %w", db.ID, err)
		}
		if err := m.appendPgpassEntry(db, password); err != nil {
			return fmt.Errorf("failed to write entry to .pgpass file for db %d: %w", db.ID, err)
		}
	}
	return nil
}

// CleanupPgpass removes the .pgpass file.
func (m *PgpassManager) CleanupPgpass(_ context.Context) error {
	m.logger.Info("Cleaning up .pgpass file", "path", m.pgpassPath)
	return os.Remove(m.pgpassPath)
}

func (m *PgpassManager) appendPgpassEntry(db domain.Database, password string) error {
	port := "*"
	if db.Port != nil {
		port = fmt.Sprintf("%d", *db.Port)
	}
	pgpassEntry := fmt.Sprintf("%s:%s:%s:%s:%s\n", db.Host, port, db.DBName, db.User, password)

	f, err := os.OpenFile(m.pgpassPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open .pgpass file: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(pgpassEntry); err != nil {
		return fmt.Errorf("failed to write to .pgpass file: %w", err)
	}

	// The chown is handled by the Bucardo CLI adapter's generic command runner if needed,
	// but it's better to ensure the file is created with the correct permissions from the start.
	// For now, we assume the process has the correct user context.
	cmd := exec.Command("chown", fmt.Sprintf("%s:%s", m.bucardoUser, m.bucardoUser), m.pgpassPath)
	if err := cmd.Run(); err != nil {
		m.logger.Warn("Failed to chown .pgpass file", "error", err)
	}

	return nil
}

func (m *PgpassManager) getDbPassword(db domain.Database) (string, error) {
	if db.Pass == "env" {
		envVar := fmt.Sprintf("BUCARDO_DB%d", db.ID)
		password := os.Getenv(envVar)
		if password == "" {
			return "", fmt.Errorf("environment variable %s not set for db id %d", envVar, db.ID)
		}
		return password, nil
	}
	return db.Pass, nil
}
