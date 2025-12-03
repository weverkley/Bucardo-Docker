package bucardo

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"replication-service/internal/core/domain"
	"replication-service/internal/core/ports"
)

// MonitorAdapter implements the Monitor port for observing Bucardo.
type MonitorAdapter struct {
	logger         ports.Logger
	bucardoLogPath string
	bucardoUser    string
	bucardoCmd     string
}

// NewMonitorAdapter creates a new MonitorAdapter.
func NewMonitorAdapter(logger ports.Logger, logPath, user, cmd string) *MonitorAdapter {
	return &MonitorAdapter{
		logger:         logger,
		bucardoLogPath: logPath,
		bucardoUser:    user,
		bucardoCmd:     cmd,
	}
}

// MonitorBucardo handles the default long-running mode.
func (m *MonitorAdapter) MonitorBucardo(ctx context.Context, stopFunc func()) {
	tailCmd := m.streamBucardoLog(ctx)
	if tailCmd != nil && tailCmd.Process != nil {
		defer func() {
			m.logger.Info("Stopping log streamer", "component", "log_streamer")
			syscall.Kill(-tailCmd.Process.Pid, syscall.SIGKILL)
		}()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	select {
	case sig := <-sigChan:
		m.logger.Info("Received signal, shutting down gracefully", "component", "shutdown", "signal", sig)
		stopFunc()
	case <-ctx.Done():
		m.logger.Info("Context cancelled, shutting down", "component", "shutdown")
		stopFunc()
	}
}

// MonitorSyncs handles the "run-once" mode by tailing the Bucardo log for completion.
func (m *MonitorAdapter) MonitorSyncs(ctx context.Context, config *domain.BucardoConfig, runOnceSyncs map[string]bool, maxTimeout *int, stopBucardoFunc func()) {
	if config.LogLevel != "VERBOSE" && config.LogLevel != "DEBUG" {
		m.logger.Warn("'exit_on_complete' is true, but 'log_level' is not 'VERBOSE' or 'DEBUG'. The completion message may not be logged.")
	}

	m.logger.Info("Monitoring sync(s) for completion", "count", len(runOnceSyncs), "syncs", getMapKeys(runOnceSyncs))

	allSyncsAreRunOnce := len(config.Syncs) == len(runOnceSyncs)
	var timeoutChannel <-chan time.Time

	// Setup log tailing
	cmd := exec.CommandContext(ctx, "tail", "-F", m.bucardoLogPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.logger.Error("Could not create pipe for tail command", "error", err)
		os.Exit(1) // This is a fatal startup error
	}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	defer func() {
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
		}
	}()

	if err := cmd.Start(); err != nil {
		m.logger.Error("Could not start log tailing command", "error", err)
		os.Exit(1) // Fatal
	}

	if maxTimeout != nil && *maxTimeout > 0 {
		timeoutDuration := time.Duration(*maxTimeout) * time.Second
		m.logger.Info("Setting a timeout for run-once sync completion", "timeout", timeoutDuration)
		timeoutChannel = time.After(timeoutDuration)
	}

	lineChan := make(chan string)
	go func() {
		defer close(lineChan)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case lineChan <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()

	bucardoExecutor := NewCLIExecutor(m.logger, m.bucardoUser, m.bucardoCmd)

	for {
		select {
		case line, ok := <-lineChan:
			if !ok {
				m.logger.Info("Log streaming finished unexpectedly.")
				return
			}
			// Log the line to ensure it goes to the websocket/stdout via the multiwriter
			m.logger.Info(line, "component", "bucardo_log")

			if strings.Contains(line, "Reason: Normal exit") {
				for syncName := range runOnceSyncs {
					if strings.Contains(line, fmt.Sprintf("KID (%s)", syncName)) {
						m.logger.Info("Completion message for sync detected", "sync_name", syncName)
						if err := bucardoExecutor.ExecuteBucardoCommand(ctx, "stop", syncName); err != nil {
							m.logger.Warn("Failed to stop sync after completion", "error", err, "sync_name", syncName)
						}
						delete(runOnceSyncs, syncName)
						m.logger.Info("Run-once sync(s) remaining", "count", len(runOnceSyncs))
					}
				}
			}

			if len(runOnceSyncs) == 0 {
				m.logger.Info("All monitored syncs have completed.")
				if allSyncsAreRunOnce {
					m.logger.Info("All configured syncs were run-once. Shutting down container.")
					stopBucardoFunc()
					return
				}
				m.logger.Info("Other syncs are still running. Switching to standard monitoring mode.")
				m.MonitorBucardo(ctx, stopBucardoFunc)
				return
			}
		case <-timeoutChannel:
			m.logger.Error("Timeout reached for run-once syncs", "timeout_seconds", *maxTimeout, "incomplete_syncs", getMapKeys(runOnceSyncs))
			stopBucardoFunc()
			os.Exit(1)
		case <-ctx.Done():
			m.logger.Info("Context cancelled during sync monitoring.")
			stopBucardoFunc()
			return
		}
	}
}

func (m *MonitorAdapter) streamBucardoLog(ctx context.Context) *exec.Cmd {
	time.Sleep(2 * time.Second)
	cmd := exec.CommandContext(ctx, "tail", "-F", m.bucardoLogPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	
	// Capture stdout to pipe it through our logger (so it goes to WebSocket)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.logger.Error("Could not create pipe for tail command", "error", err)
		return nil
	}
	cmd.Stderr = os.Stderr

	m.logger.Info("Streaming Bucardo log file", "path", m.bucardoLogPath)
	if err := cmd.Start(); err != nil {
		m.logger.Warn("Could not start streaming Bucardo log file", "error", err)
		return nil
	}

	// Consume the log stream in a background goroutine
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			// We log it as INFO so it appears in the standard log stream/websocket
			// We use a specific component tag to distinguish it
			m.logger.Info(scanner.Text(), "component", "bucardo_log")
		}
	}()

	return cmd
}

func getMapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
