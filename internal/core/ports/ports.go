package ports

import (
	"context"

	"replication-service/internal/core/domain"
)

// Logger defines the interface for structured logging.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	Debug(msg string, args ...any)
	With(args ...any) Logger
}

// ConfigProvider defines the interface for loading the application configuration.
type ConfigProvider interface {
	LoadConfig(ctx context.Context) (*domain.BucardoConfig, error)
	SaveConfig(ctx context.Context, config *domain.BucardoConfig) error
}

// CredentialManager defines the interface for managing database credentials.
type CredentialManager interface {
	SetupPgpass(ctx context.Context, dbs []domain.Database) error
	CleanupPgpass(ctx context.Context) error
}

// BucardoExecutor defines the interface for interacting with the Bucardo CLI and related services.
type BucardoExecutor interface {
	InstallBucardo(ctx context.Context, dbname, host, user, pass string) error
	EnsureBucardoUserPassword(ctx context.Context, dbhost, dbuser, dbpass, bucardoUser, bucardoPass string, dbport int) error
	SetLogLevel(ctx context.Context, level string) error
	ListDatabases(ctx context.Context) ([]string, error)
	DatabaseExists(ctx context.Context, dbName string) (bool, error)
	RemoveDatabase(ctx context.Context, dbName string) error
	ListSyncs(ctx context.Context) ([]string, error)
	SyncExists(ctx context.Context, syncName string) (bool, []byte, error)
	GetSyncRelgroup(ctx context.Context, syncDetailsOutput []byte) (string, error)
	GetSyncTables(ctx context.Context, relgroupName string) ([]string, error)
	RemoveSyncAndRelgroup(ctx context.Context, syncName, relgroupName, dbHost, dbUser, dbPass string, dbPort int) error
	ExecuteBucardoCommand(ctx context.Context, args ...string) error
	StartBucardo(ctx context.Context) error
	StopBucardo(ctx context.Context) error
}

// Monitor defines the port for observing the Bucardo process.
type Monitor interface {
	MonitorSyncs(ctx context.Context, config *domain.BucardoConfig, runOnceSyncs map[string]bool, maxTimeout *int, stopBucardoFunc func())
	MonitorBucardo(ctx context.Context, stopFunc func())
}
