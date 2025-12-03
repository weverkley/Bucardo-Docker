package bucardo

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"replication-service/internal/core/ports"
)

// redactPassword replaces the password in a command string with asterisks.
func redactPassword(cmd string) string {
	re := regexp.MustCompile(`pass=[^ ]+`)
	return re.ReplaceAllString(cmd, "pass=*****")
}

// CLIExecutor implements the BucardoExecutor port using os/exec.
type CLIExecutor struct {
	logger      ports.Logger
	bucardoUser string
	bucardoCmd  string
}

// NewCLIExecutor creates a new CLIExecutor.
func NewCLIExecutor(logger ports.Logger, bucardoUser, bucardoCmd string) *CLIExecutor {
	return &CLIExecutor{
		logger:      logger,
		bucardoUser: bucardoUser,
		bucardoCmd:  bucardoCmd,
	}
}

func (e *CLIExecutor) runCommand(ctx context.Context, logCmd, name string, arg ...string) error {
	cmd := exec.CommandContext(ctx, name, arg...)
	if logCmd == "" {
		logCmd = cmd.String()
	}
	// Redact password before logging
	redactedLogCmd := redactPassword(logCmd)
	e.logger.Info("Running command", "component", "command_runner", "command", redactedLogCmd)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}
	// Note: Streaming stdout directly might not be desirable for all commands in a library.
	// We'll keep it for commands that are expected to produce user-facing output.
	cmd.Stdout = os.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		// Filter known harmless messages
		if !strings.HasPrefix(line, "No such dbgroup:") && !strings.HasPrefix(line, "No such sync:") {
			fmt.Fprintln(os.Stderr, line)
		}
	}

	return cmd.Wait()
}

func (e *CLIExecutor) runBucardoCommand(ctx context.Context, args ...string) error {
	bucardoCmdWithArgs := e.bucardoCmd + " " + strings.Join(args, " ")
	return e.runCommand(ctx, bucardoCmdWithArgs, "su", "-", e.bucardoUser, "-c", bucardoCmdWithArgs)
}

func (e *CLIExecutor) runBucardoCommandWithOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmdStr := fmt.Sprintf("%s %s", e.bucardoCmd, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "su", "-", e.bucardoUser, "-c", cmdStr)
	e.logger.Debug("Running command for output", "command", cmdStr)
	return cmd.CombinedOutput()
}

// EnsureBucardoUserPassword forces the password for the 'bucardo' user to match the configuration.
// This is critical for idempotency on existing volumes where the user might already exist with an unknown password.
func (e *CLIExecutor) EnsureBucardoUserPassword(ctx context.Context, dbhost, dbuser, dbpass, bucardoUser, bucardoPass string, dbport int) error {
	// Construct the SQL command: ALTER USER bucardo WITH PASSWORD '...'
	sql := fmt.Sprintf("ALTER USER %s WITH PASSWORD '%s';", bucardoUser, bucardoPass)

	// We use psql to execute this as the superuser (dbuser).
	// PGPASSWORD is used for authentication.
	cmdStr := fmt.Sprintf("PGPASSWORD=%s psql -h %s -p %d -U %s -d postgres -c \"%s\"", dbpass, dbhost, dbport, dbuser, sql)

	// Note: We run this command directly (not via 'su - postgres') because we are passing environment variables
	// and we want to run psql. However, the container runs as root, so we can run psql directly if installed.
	// Or we can run it as the bucardoUser (postgres) if that user has psql in path.
	// The safest is to run as the OS user 'postgres' to match other commands.
	cmd := exec.CommandContext(ctx, "su", "-", e.bucardoUser, "-c", cmdStr)

	e.logger.Info("Ensuring 'bucardo' user password is correct", "component", "auth_fixer", "host", dbhost, "user", bucardoUser)
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		// If the user doesn't exist, ALTER USER will fail. We can ignore that because InstallBucardo will create it.
		if strings.Contains(string(output), "does not exist") {
			e.logger.Info("User 'bucardo' does not exist yet, skipping password reset.", "component", "auth_fixer")
			return nil
		}
		return fmt.Errorf("failed to reset bucardo user password: %w. Output: %s", err, string(output))
	}
	e.logger.Info("Successfully updated 'bucardo' user password.", "component", "auth_fixer")
	return nil
}

// InstallBucardo runs the 'bucardo install' command to set up the Bucardo schema in the core database.
func (e *CLIExecutor) InstallBucardo(ctx context.Context, dbname, host, user, pass string) error {
	// 1. Pre-check: See if Bucardo is already operational.
	if _, err := e.runBucardoCommandWithOutput(ctx, "list", "dbs"); err == nil {
		e.logger.Info("Bucardo appears to be already installed and operational.", "component", "bucardo_installer")
		return nil
	}

	// The output of this command can be verbose and includes normal notices.
	// We prepend PGPASSWORD to the command for non-interactive authentication.
	logCmd := fmt.Sprintf("bucardo install --batch --dbname=%s --dbhost=%s --dbuser=%s --dbpass=****", dbname, host, user)
	cmdStr := fmt.Sprintf("PGPASSWORD=%s bucardo install --batch --dbname=%s --dbhost=%s --dbuser=%s", pass, dbname, host, user)

	cmd := exec.CommandContext(ctx, "su", "-", e.bucardoUser, "-c", cmdStr)

	e.logger.Info("Running Bucardo installation", "component", "bucardo_installer", "command", logCmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// 'bucardo install' can exit with a non-zero status if it's already installed (e.g. "role already exists").
		// If that happens, we check if the installation is actually working now.
		if strings.Contains(string(output), "already exists") {
			e.logger.Info("Installation reported 'already exists'. Verifying installation state...", "component", "bucardo_installer")
			if _, checkErr := e.runBucardoCommandWithOutput(ctx, "list", "dbs"); checkErr == nil {
				e.logger.Info("Bucardo is operational despite install error. Assuming success.", "component", "bucardo_installer")
				return nil
			}
		}
		return fmt.Errorf("bucardo install failed: %w. Output: %s", err, string(output))
	}
	e.logger.Info("Bucardo installation command finished.", "output", string(output))
	return nil
}

// SetLogLevel sets the global Bucardo logging level.
func (e *CLIExecutor) SetLogLevel(ctx context.Context, level string) error {
	return e.runBucardoCommand(ctx, "set", fmt.Sprintf("log_level=%s", level))
}

// ListDatabases returns a slice of all database names currently configured in Bucardo.
func (e *CLIExecutor) ListDatabases(ctx context.Context) ([]string, error) {
	re := regexp.MustCompile(`Database: (\S+)`)
	output, err := e.runBucardoCommandWithOutput(ctx, "list", "dbs")
	outputStr := string(output)

	if err != nil {
		if strings.Contains(outputStr, "No databases found") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to execute 'bucardo list dbs': %w. Output: %s", err, outputStr)
	}

	matches := re.FindAllStringSubmatch(outputStr, -1)
	if matches == nil {
		return []string{}, nil
	}

	var dbs []string
	for _, match := range matches {
		dbs = append(dbs, match[1])
	}
	return dbs, nil
}

// DatabaseExists checks if a Bucardo database with the given name already exists.
func (e *CLIExecutor) DatabaseExists(ctx context.Context, dbName string) (bool, error) {
	allDbs, err := e.ListDatabases(ctx)
	if err != nil {
		e.logger.Warn("Could not list Bucardo databases to check for existence", "error", err)
		return false, err
	}
	for _, bdb := range allDbs {
		if bdb == dbName {
			return true, nil
		}
	}
	return false, nil
}

// ExecuteBucardoCommand is a general-purpose method to run any bucardo command.
func (e *CLIExecutor) ExecuteBucardoCommand(ctx context.Context, args ...string) error {
	return e.runBucardoCommand(ctx, args...)
}

// RemoveDatabase removes a database from bucardo.
func (e *CLIExecutor) RemoveDatabase(ctx context.Context, dbName string) error {
	return e.runBucardoCommand(ctx, "del", "dbs", dbName)
}

// ListSyncs returns a slice of all sync names currently configured in Bucardo.
func (e *CLIExecutor) ListSyncs(ctx context.Context) ([]string, error) {
	re := regexp.MustCompile(`Sync "([^"]+)"`)
	output, err := e.runBucardoCommandWithOutput(ctx, "list", "syncs")
	outputStr := string(output)

	if err != nil {
		if strings.Contains(outputStr, "No syncs found") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to execute 'bucardo list syncs': %w. Output: %s", err, outputStr)
	}

	matches := re.FindAllStringSubmatch(outputStr, -1)
	if matches == nil {
		return []string{}, nil
	}
	var syncs []string
	for _, match := range matches {
		syncs = append(syncs, match[1])
	}
	return syncs, nil
}

// SyncExists checks if a Bucardo sync with the given name already exists.
func (e *CLIExecutor) SyncExists(ctx context.Context, syncName string) (bool, []byte, error) {
	cmd := exec.CommandContext(ctx, "su", "-", e.bucardoUser, "-c", fmt.Sprintf("bucardo list sync %s", syncName))
	var outb, errb strings.Builder
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	err := cmd.Run()

	stdoutString := outb.String()
	// Bucardo can return exit 0 even if the sync is not found, usually printing "No such sync" or similar.
	exists := err == nil && stdoutString != "" && !strings.Contains(stdoutString, "No such sync")
	return exists, []byte(stdoutString), nil
}

// GetSyncRelgroup parses the output of `bucardo list sync` to find the relgroup name.
func (e *CLIExecutor) GetSyncRelgroup(_ context.Context, syncDetailsOutput []byte) (string, error) {
	re := regexp.MustCompile(`Relgroup: (\S+)`)
	matches := re.FindStringSubmatch(string(syncDetailsOutput))
	if len(matches) < 2 {
		return "", fmt.Errorf("could not find relgroup in sync details")
	}
	return matches[1], nil
}

// GetSyncTables fetches the list of tables for a given relgroup from Bucardo.
func (e *CLIExecutor) GetSyncTables(ctx context.Context, relgroupName string) ([]string, error) {
	if relgroupName == "" {
		return []string{}, nil
	}
	output, err := e.runBucardoCommandWithOutput(ctx, "list", "relgroup", relgroupName, "--verbose")
	if err != nil {
		return nil, fmt.Errorf("failed to list relgroup %s: %w. Output: %s", relgroupName, err, string(output))
	}

	re := regexp.MustCompile(`\s+(\S+\.\S+)`)
	matches := re.FindAllStringSubmatch(string(output), -1)
	if len(matches) == 0 {
		return []string{}, nil
	}

	var tables []string
	for _, match := range matches {
		tableName := strings.TrimRight(match[1], ",")
		tableName = strings.TrimSpace(strings.Split(tableName, "(")[0])
		if tableName != "" {
			tables = append(tables, tableName)
		}
	}
	sort.Strings(tables)
	return tables, nil
}

// RemoveSyncAndRelgroup removes a sync and its associated relgroup.
func (e *CLIExecutor) RemoveSyncAndRelgroup(ctx context.Context, syncName, relgroupName, dbHost, dbUser, dbPass string, dbPort int) error {
	// 1. Try standard CLI removal
	cliErr := e.runBucardoCommand(ctx, "del", "sync", syncName, "--force")
	if cliErr != nil {
		e.logger.Warn("Standard 'del sync' failed, attempting direct SQL cleanup as fallback", "error", cliErr)
		
		// 2. Fallback: Direct SQL deletion
		// We delete from bucardo.sync (which cascades to dependent objects usually, but we be specific)
		// Note: The table for relgroups is 'bucardo.herd'.
		sql := fmt.Sprintf("DELETE FROM bucardo.sync WHERE name = '%s'; DELETE FROM bucardo.herd WHERE name = '%s';", syncName, relgroupName)
		
		cmdStr := fmt.Sprintf("PGPASSWORD=%s psql -h %s -p %d -U %s -d bucardo -c \"%s\"", dbPass, dbHost, dbPort, dbUser, sql)
		cmd := exec.CommandContext(ctx, "su", "-", e.bucardoUser, "-c", cmdStr)
		
		output, sqlErr := cmd.CombinedOutput()
		if sqlErr != nil {
			e.logger.Error("Fallback SQL cleanup also failed", "error", sqlErr, "output", string(output))
			// Return the original CLI error as it's likely the root cause investigation point, 
			// but logged the SQL error too.
			return cliErr 
		}
		e.logger.Info("Fallback SQL cleanup succeeded")
	}

	// 3. Cleanup Relgroup (Best effort via CLI, might have been deleted by SQL above)
	// We ignore errors here because if SQL deleted it, this will fail harmlessly.
	e.runBucardoCommand(ctx, "del", "relgroup", relgroupName)
	
	return nil
}

// StartBucardo starts the main Bucardo process.
func (e *CLIExecutor) StartBucardo(ctx context.Context) error {
	e.logger.Info("Checking for and stopping any stale Bucardo processes...")
	if err := e.StopBucardo(ctx); err != nil {
		e.logger.Warn("Pre-start stop command failed, continuing anyway...", "error", err)
	}
	e.logger.Info("Starting main Bucardo service", "component", "bucardo_service")
	return e.runBucardoCommand(ctx, "start")
}

// StopBucardo gracefully stops the Bucardo service.
func (e *CLIExecutor) StopBucardo(ctx context.Context) error {
	e.logger.Info("Stopping main Bucardo service", "component", "bucardo_service")
	if err := e.runBucardoCommand(ctx, "stop"); err != nil {
		e.logger.Warn("'bucardo stop' command failed", "error", err)
	}

	const shutdownTimeout = 30 * time.Second
	deadline := time.Now().Add(shutdownTimeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if _, err := os.Stat("/var/run/bucardo/bucardo.mcp.pid"); os.IsNotExist(err) {
				e.logger.Info("Bucardo has stopped.")
				return nil
			}
			time.Sleep(1 * time.Second)
		}
	}
	return fmt.Errorf("bucardo did not stop gracefully within %v", shutdownTimeout)
}
