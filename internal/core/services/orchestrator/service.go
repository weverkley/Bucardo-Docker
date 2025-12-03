package orchestrator

import (
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"sort"
	"strings"

	"replication-service/internal/core/domain"
	"replication-service/internal/core/ports"
)

// Service is the core orchestrator for Bucardo replication.
type Service struct {
	logger         ports.Logger
	config         ports.ConfigProvider
	creds          ports.CredentialManager
	bucardo        ports.BucardoExecutor
	monitor        ports.Monitor
	configPath     string
	pgpassPath     string
	bucardoUser    string
	bucardoCmd     string
	bucardoLogPath string
}

// NewService creates a new orchestration service.
func NewService(
	logger ports.Logger,
	config ports.ConfigProvider,
	creds ports.CredentialManager,
	bucardo ports.BucardoExecutor,
	monitor ports.Monitor,
	configPath, pgpassPath, bucardoUser, bucardoCmd, bucardoLogPath string,
) *Service {
	return &Service{
		logger:         logger,
		config:         config,
		creds:          creds,
		bucardo:        bucardo,
		monitor:        monitor,
		configPath:     configPath,
		pgpassPath:     pgpassPath,
		bucardoUser:    bucardoUser,
		bucardoCmd:     bucardoCmd,
		bucardoLogPath: bucardoLogPath,
	}
}

// Run starts the main application logic.
func (s *Service) Run(ctx context.Context) error {
	if err := s.ReloadAndRestart(ctx); err != nil {
		return err
	}

	// Monitor logic
	config, err := s.config.LoadConfig(ctx)
	if err != nil {
		return err
	}

	runOnceSyncs := make(map[string]bool)
	var maxTimeout *int

	for _, sync := range config.Syncs {
		if sync.ExitOnComplete != nil && *sync.ExitOnComplete {
			runOnceSyncs[sync.Name] = true
			if sync.ExitOnCompleteTimeout != nil {
				if maxTimeout == nil || *sync.ExitOnCompleteTimeout > *maxTimeout {
					timeoutVal := *sync.ExitOnCompleteTimeout
					maxTimeout = &timeoutVal
				}
			}
		}
	}

	stopBucardoFunc := func() {
		if err := s.bucardo.StopBucardo(context.Background()); err != nil {
			s.logger.Error("Failed to stop Bucardo", "error", err)
		}
	}

	if len(runOnceSyncs) > 0 {
		s.monitor.MonitorSyncs(ctx, config, runOnceSyncs, maxTimeout, stopBucardoFunc)
	} else {
		s.monitor.MonitorBucardo(ctx, stopBucardoFunc)
	}

	return nil
}

func (s *Service) GetConfig(ctx context.Context) (*domain.BucardoConfig, error) {
	return s.config.LoadConfig(ctx)
}

func (s *Service) UpdateConfig(ctx context.Context, config *domain.BucardoConfig) error {
	// Validate before saving
	if errs := s.validateConfig(config); len(errs) > 0 {
		return fmt.Errorf("invalid config: %v", errs)
	}
	return s.config.SaveConfig(ctx, config)
}

func (s *Service) StartBucardoProcess(ctx context.Context) error {
	return s.bucardo.StartBucardo(ctx)
}

func (s *Service) StopBucardoProcess(ctx context.Context) error {
	return s.bucardo.StopBucardo(ctx)
}

func (s *Service) ReloadAndRestart(ctx context.Context) error {
	s.logger.Info("Reloading and restarting application...")

	if _, err := os.Stat(s.configPath); os.IsNotExist(err) {
		s.logger.Error("Configuration file not found.", "path", s.configPath)
		return err
	}

	config, err := s.config.LoadConfig(ctx)
	if err != nil {
		s.logger.Error("Failed to load configuration", "error", err)
		return err
	}

	if validationErrors := s.validateConfig(config); len(validationErrors) > 0 {
		s.logger.Error("Invalid configuration found in bucardo.json")
		for _, e := range validationErrors {
			s.logger.Error(e.Error())
		}
		return fmt.Errorf("configuration validation failed")
	}

	// Stop Bucardo before making changes (safe mode)
	s.bucardo.StopBucardo(ctx)

	// Load Env Vars
	dbName := getEnv("BUCARDO_DB_NAME", "bucardo")
	dbHost := getEnv("BUCARDO_DB_HOST", "postgres")
	dbUser := getEnv("BUCARDO_DB_USER", "postgres")
	dbPass := getEnv("BUCARDO_DB_PASS", "changeme")
	dbPortStr := getEnv("BUCARDO_DB_PORT", "5432")
	var dbPort int
	fmt.Sscanf(dbPortStr, "%d", &dbPort)

	// Setup .pgpass
	systemDB := domain.Database{
		ID:     0,
		DBName: dbName,
		Host:   dbHost,
		User:   dbName,
		Pass:   dbPass,
		Port:   &dbPort,
	}
	superuserDB := domain.Database{
		ID:     -1,
		DBName: dbName,
		Host:   dbHost,
		User:   dbUser,
		Pass:   dbPass,
		Port:   &dbPort,
	}
	allDBsForPass := append([]domain.Database{systemDB, superuserDB}, config.Databases...)

	if err := s.creds.SetupPgpass(ctx, allDBsForPass); err != nil {
		s.logger.Error("Failed to setup .pgpass file", "error", err)
		return err
	}
	defer s.creds.CleanupPgpass(ctx)

	// Ensure Bucardo User Password
	if err := s.bucardo.EnsureBucardoUserPassword(ctx, dbHost, dbUser, dbPass, dbName, dbPass, dbPort); err != nil {
		s.logger.Warn("Failed to ensure bucardo user password", "error", err)
	}

	// Install/Ensure Bucardo
	if err := s.bucardo.InstallBucardo(ctx, dbName, dbHost, dbUser, dbPass); err != nil {
		s.logger.Error("Failed to install Bucardo schema", "error", err)
		return err
	}

	if err := s.setLogLevel(ctx, config); err != nil {
		s.logger.Warn("Failed to set log_level", "error", err)
	}

	if err := s.removeOrphanedDbs(ctx, config); err != nil {
		s.logger.Error("Failed to remove orphaned databases", "error", err)
	}

	if err := s.removeOrphanedSyncs(ctx, config, dbHost, dbUser, dbPass, dbPort); err != nil {
		s.logger.Error("Failed to remove orphaned syncs", "error", err)
	}

	if err := s.addDatabasesToBucardo(ctx, config); err != nil {
		s.logger.Error("Failed to reconcile databases", "error", err)
		return err
	}

	if err := s.addSyncsToBucardo(ctx, config, dbHost, dbUser, dbPass, dbPort); err != nil {
		s.logger.Error("Failed to reconcile syncs", "error", err)
		return err
	}

	if err := s.bucardo.StartBucardo(ctx); err != nil {
		s.logger.Error("Failed to start bucardo", "error", err)
		return err
	}

	s.logger.Info("Reload and restart complete.")
	return nil
}

func (s *Service) ListSyncs(ctx context.Context) ([]domain.Sync, error) {
	config, err := s.config.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	return config.Syncs, nil
}

func (s *Service) GetSync(ctx context.Context, name string) (*domain.Sync, error) {
	config, err := s.config.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	for _, sync := range config.Syncs {
		if sync.Name == name {
			return &sync, nil
		}
	}
	return nil, fmt.Errorf("sync not found: %s", name)
}

func (s *Service) AddSync(ctx context.Context, sync domain.Sync) error {
	config, err := s.config.LoadConfig(ctx)
	if err != nil {
		return err
	}
	for _, existing := range config.Syncs {
		if existing.Name == sync.Name {
			return fmt.Errorf("sync already exists: %s", sync.Name)
		}
	}
	config.Syncs = append(config.Syncs, sync)
	return s.UpdateConfig(ctx, config)
}

func (s *Service) UpdateSync(ctx context.Context, name string, updated domain.Sync) error {
	config, err := s.config.LoadConfig(ctx)
	if err != nil {
		return err
	}
	found := false
	for i, sync := range config.Syncs {
		if sync.Name == name {
			// Enforce the name from the path/identifier to ensure consistency
			updated.Name = name
			config.Syncs[i] = updated
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("sync not found: %s", name)
	}
	return s.UpdateConfig(ctx, config)
}

func (s *Service) DeleteSync(ctx context.Context, name string) error {
	config, err := s.config.LoadConfig(ctx)
	if err != nil {
		return err
	}
	newSyncs := make([]domain.Sync, 0, len(config.Syncs))
	found := false
	for _, sync := range config.Syncs {
		if sync.Name == name {
			found = true
			continue
		}
		newSyncs = append(newSyncs, sync)
	}
	if !found {
		return fmt.Errorf("sync not found: %s", name)
	}
	config.Syncs = newSyncs
	return s.UpdateConfig(ctx, config)
}

// validateConfig performs a pre-check of the configuration to catch common errors.
func (s *Service) validateConfig(config *domain.BucardoConfig) []error {
	var errors []error
	dbIDs := make(map[int]bool)
	syncNames := make(map[string]bool)

	for _, db := range config.Databases {
		if dbIDs[db.ID] {
			errors = append(errors, fmt.Errorf("database ID %d is duplicated", db.ID))
		}
		dbIDs[db.ID] = true
	}

	for _, sync := range config.Syncs {
		if sync.Name == "" {
			errors = append(errors, fmt.Errorf("a sync is missing the required 'name' property"))
			continue // Can't validate this sync further
		}
		if syncNames[sync.Name] {
			errors = append(errors, fmt.Errorf("sync name '%s' is duplicated", sync.Name))
		}
		syncNames[sync.Name] = true

		if len(sync.Bidirectional) > 0 {
			if len(sync.Bidirectional) < 2 {
				errors = append(errors, fmt.Errorf("sync '%s': 'bidirectional' requires at least two database IDs", sync.Name))
			}
			for _, id := range sync.Bidirectional {
				if !dbIDs[id] {
					errors = append(errors, fmt.Errorf("sync '%s': 'bidirectional' database ID %d is not defined in the 'databases' list", sync.Name, id))
				}
			}
			if sync.ConflictStrategy == "bucardo_source" || sync.ConflictStrategy == "bucardo_target" {
				errors = append(errors, fmt.Errorf("sync '%s': invalid conflict_strategy '%s' for a bidirectional sync. Use 'bucardo_latest' instead", sync.Name, sync.ConflictStrategy))
			}
		} else { // Standard source/target sync
			if len(sync.Sources) == 0 {
				errors = append(errors, fmt.Errorf("sync '%s': must have at least one source", sync.Name))
			}
			if len(sync.Targets) == 0 {
				errors = append(errors, fmt.Errorf("sync '%s': must have at least one target", sync.Name))
			}
			if sync.Herd == "" && sync.Tables == "" {
				errors = append(errors, fmt.Errorf("sync '%s': must define either 'herd' or 'tables'", sync.Name))
			}
		}

		if sync.ConflictStrategy != "" {
			validStrategies := map[string]bool{
				"bucardo_source": true, "bucardo_target": true, "bucardo_skip": true,
				"bucardo_random": true, "bucardo_latest": true, "bucardo_abort": true,
			}
			if !validStrategies[sync.ConflictStrategy] {
				validKeys := make([]string, 0, len(validStrategies))
				for k := range validStrategies {
					validKeys = append(validKeys, k)
				}
				errors = append(errors, fmt.Errorf("sync '%s': invalid conflict_strategy '%s'. Must be one of: %v", sync.Name, sync.ConflictStrategy, validKeys))
			}
		}
	}
	return errors
}

func (s *Service) setLogLevel(ctx context.Context, config *domain.BucardoConfig) error {
	if config.LogLevel != "" {
		s.logger.Info("Setting Bucardo global log level", "component", "config", "level", config.LogLevel)
		return s.bucardo.SetLogLevel(ctx, config.LogLevel)
	}
	return nil
}

func (s *Service) removeOrphanedDbs(ctx context.Context, config *domain.BucardoConfig) error {
	appLogger := s.logger.With("component", "cleanup")
	appLogger.Info("Checking for orphaned databases to remove")

	configDbs := make(map[string]bool)
	for _, db := range config.Databases {
		configDbs[fmt.Sprintf("db%d", db.ID)] = true
	}

	bucardoDbs, err := s.bucardo.ListDatabases(ctx)
	if err != nil {
		return fmt.Errorf("could not list existing Bucardo databases for cleanup: %w", err)
	}

	for _, bucardoDbName := range bucardoDbs {
		if !configDbs[bucardoDbName] {
			appLogger.Info("Removing orphaned database not found in configuration", "db_name", bucardoDbName)
			if err := s.bucardo.RemoveDatabase(ctx, bucardoDbName); err != nil {
				appLogger.Error("Failed to remove orphaned db", "db_name", bucardoDbName, "error", err)
			}
		}
	}
	return nil
}

func (s *Service) removeOrphanedSyncs(ctx context.Context, config *domain.BucardoConfig, dbHost, dbUser, dbPass string, dbPort int) error {
	appLogger := s.logger.With("component", "cleanup")
	appLogger.Info("Checking for orphaned syncs to remove")

	configSyncs := make(map[string]bool)
	for _, sync := range config.Syncs {
		configSyncs[sync.Name] = true
	}

	bucardoSyncs, err := s.bucardo.ListSyncs(ctx)
	if err != nil {
		return fmt.Errorf("could not list existing Bucardo syncs for cleanup: %w", err)
	}

	for _, bucardoSyncName := range bucardoSyncs {
		if !configSyncs[bucardoSyncName] {
			appLogger.Info("Removing orphaned sync not found in configuration", "sync_name", bucardoSyncName)
			exists, syncDetails, err := s.bucardo.SyncExists(ctx, bucardoSyncName)
			if err != nil || !exists {
				continue
			}

			relgroupName, err := s.bucardo.GetSyncRelgroup(ctx, syncDetails)
			if err != nil {
				relgroupName = bucardoSyncName // Fallback
			}

			if err := s.bucardo.RemoveSyncAndRelgroup(ctx, bucardoSyncName, relgroupName, dbHost, dbUser, dbPass, dbPort); err != nil {
				appLogger.Error("Failed to remove orphaned sync/relgroup", "sync_name", bucardoSyncName, "error", err)
			}
		}
	}
	return nil
}

func getDbPassword(db domain.Database) (string, error) {
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

func (s *Service) addDatabasesToBucardo(ctx context.Context, config *domain.BucardoConfig) error {
	appLogger := s.logger.With("component", "db_reconciler")
	appLogger.Info("Starting database reconciliation")

	for _, db := range config.Databases {
		dbName := fmt.Sprintf("db%d", db.ID)
		dbLogger := appLogger.With("db_name", dbName, "db_id", db.ID, "db_host", db.Host)

		exists, err := s.bucardo.DatabaseExists(ctx, dbName)
		if err != nil {
			return fmt.Errorf("could not check if database exists %s: %w", dbName, err)
		}

		password, err := getDbPassword(db)
		if err != nil {
			return fmt.Errorf("error getting password for db %d: %w", db.ID, err)
		}

		var args []string
		if exists {
			dbLogger.Info("Database exists, preparing update")
			args = []string{
				"update", "db", dbName,
				fmt.Sprintf("dbname=%s", db.DBName),
				fmt.Sprintf("host=%s", db.Host),
				fmt.Sprintf("user=%s", db.User),
				fmt.Sprintf("pass=%s", password),
			}
		} else {
			dbLogger.Info("Database not found, preparing to add")
			args = []string{
				"add", "db", dbName,
				fmt.Sprintf("dbname=%s", db.DBName),
				fmt.Sprintf("host=%s", db.Host),
				fmt.Sprintf("user=%s", db.User),
				fmt.Sprintf("pass=%s", password),
			}
		}

		if db.Port != nil {
			args = append(args, fmt.Sprintf("port=%d", *db.Port))
		}

		if err := s.bucardo.ExecuteBucardoCommand(ctx, args...); err != nil {
			return fmt.Errorf("failed to modify database %s: %w", dbName, err)
		}
	}
	return nil
}

func (s *Service) addSyncsToBucardo(ctx context.Context, config *domain.BucardoConfig, dbHost, dbUser, dbPass string, dbPort int) error {
	appLogger := s.logger.With("component", "sync_reconciler")
	appLogger.Info("Starting sync reconciliation")

	for _, sync := range config.Syncs {
		syncLogger := appLogger.With("sync_name", sync.Name)
		exists, syncDetailsOutput, err := s.bucardo.SyncExists(ctx, sync.Name)
		if err != nil {
			return fmt.Errorf("could not check sync existence for %s: %w", sync.Name, err)
		}

		shouldRecreate := false
		if exists {
			if sync.Tables != "" {
				relgroupName, err := s.bucardo.GetSyncRelgroup(ctx, syncDetailsOutput)
				if err != nil {
					syncLogger.Debug("Could not parse relgroup from sync details, falling back to sync name.", "error", err)
					relgroupName = sync.Name
				}

				currentTables, err := s.bucardo.GetSyncTables(ctx, relgroupName)
				if err != nil {
					syncLogger.Warn("Could not get tables for relgroup, cannot compare. Assuming no change.", "relgroup", relgroupName, "error", err)
				}

				configTablesRaw := strings.Split(sync.Tables, ",")
				configTables := make([]string, 0, len(configTablesRaw))
				for _, t := range configTablesRaw {
					configTables = append(configTables, strings.TrimSpace(t))
				}
				sort.Strings(configTables)

				if strings.Join(currentTables, ",") != strings.Join(configTables, ",") {
					syncLogger.Warn("Table list for sync has changed. This requires a destructive re-creation.", "current_tables", currentTables, "new_tables", configTables)
					shouldRecreate = true
					if err := s.bucardo.RemoveSyncAndRelgroup(ctx, sync.Name, relgroupName, dbHost, dbUser, dbPass, dbPort); err != nil {
						return fmt.Errorf("failed to delete sync for recreation %s: %w", sync.Name, err)
					}
				}
			}

			if !shouldRecreate {
				syncLogger.Info("Sync exists and tables are unchanged. Applying non-destructive update.")
				updateArgs := []string{"update", "sync", sync.Name}
				if sync.StrictChecking != nil {
					updateArgs = append(updateArgs, fmt.Sprintf("strict_checking=%t", *sync.StrictChecking))
				}
				if sync.ConflictStrategy != "" {
					updateArgs = append(updateArgs, fmt.Sprintf("conflict_strategy=%s", sync.ConflictStrategy))
				}
				if err := s.bucardo.ExecuteBucardoCommand(ctx, updateArgs...); err != nil {
					return fmt.Errorf("failed to update sync %s: %w", sync.Name, err)
				}
				continue
			}
		}

		syncLogger.Info("Preparing to add sync.")
		args := []string{"add", "sync", sync.Name, fmt.Sprintf("onetimecopy=%d", sync.Onetimecopy)}

		if len(sync.Bidirectional) > 0 {
			dbgroupName := fmt.Sprintf("bg_%s", sync.Name)
			dbgroupMembers := make([]string, len(sync.Bidirectional))
			for i, dbID := range sync.Bidirectional {
				dbgroupMembers[i] = fmt.Sprintf("db%d:source", dbID)
			}
			s.bucardo.ExecuteBucardoCommand(ctx, "del", "dbgroup", dbgroupName)
			s.bucardo.ExecuteBucardoCommand(ctx, append([]string{"add", "dbgroup", dbgroupName}, dbgroupMembers...)...)
			args = append(args, fmt.Sprintf("dbs=%s", dbgroupName))
		} else {
			var dbStrings []string
			var memberNames []string
			for _, sourceID := range sync.Sources {
				member := fmt.Sprintf("db%d:source", sourceID)
				dbStrings = append(dbStrings, member)
				memberNames = append(memberNames, member)
			}
			for _, targetID := range sync.Targets {
				member := fmt.Sprintf("db%d:target", targetID)
				dbStrings = append(dbStrings, member)
				memberNames = append(memberNames, member)
			}
			sort.Strings(memberNames)
			hash := sha1.Sum([]byte(strings.Join(memberNames, ",")))
			dbgroupName := fmt.Sprintf("sg_%s_%x", sync.Name, hash[:4])

			s.bucardo.ExecuteBucardoCommand(ctx, "del", "dbgroup", dbgroupName)
			s.bucardo.ExecuteBucardoCommand(ctx, append([]string{"add", "dbgroup", dbgroupName}, dbStrings...)...)
			args = append(args, fmt.Sprintf("dbs=%s", dbgroupName))
		}

		if sync.Herd != "" {
			sourceDB := fmt.Sprintf("db%d", sync.Sources[0])
			s.bucardo.ExecuteBucardoCommand(ctx, "del", "herd", sync.Herd, "--force")
			s.bucardo.ExecuteBucardoCommand(ctx, "add", "herd", sync.Herd)
			s.bucardo.ExecuteBucardoCommand(ctx, "add", "all", "tables", fmt.Sprintf("--herd=%s", sync.Herd), fmt.Sprintf("db=%s", sourceDB))
			args = append(args, fmt.Sprintf("herd=%s", sync.Herd))
		} else if sync.Tables != "" {
			args = append(args, fmt.Sprintf("tables=%s", sync.Tables))
		}

		if sync.ExitOnComplete != nil && *sync.ExitOnComplete {
			args = append(args, "stayalive=0", "kidsalive=0")
		}
		if sync.StrictChecking != nil {
			args = append(args, fmt.Sprintf("strict_checking=%t", *sync.StrictChecking))
		}
		if sync.ConflictStrategy != "" {
			args = append(args, fmt.Sprintf("conflict_strategy=%s", sync.ConflictStrategy))
		}

		if err := s.bucardo.ExecuteBucardoCommand(ctx, args...); err != nil {
			return fmt.Errorf("failed to add sync %s: %w", sync.Name, err)
		}
	}
	return nil
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
