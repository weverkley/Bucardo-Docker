package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"replication-service/internal/adapters/bucardo"
	"replication-service/internal/adapters/config"
	logadapter "replication-service/internal/adapters/logger"
	"replication-service/internal/adapters/postgres"
	"replication-service/internal/adapters/server"
	"replication-service/internal/core/services/orchestrator"
)

const (
	bucardoLogPath    = "/var/log/bucardo/log.bucardo"
	bucardoConfigPath = "/media/bucardo/bucardo.json"
	pgpassPath        = "/var/lib/postgresql/.pgpass"
	bucardoUser       = "postgres"
	bucardoCmd        = "bucardo"
	httpPort          = 8080
)

func main() {
	// 1. Setup Log Broadcaster and Multi-Writer
	logBroadcaster := server.NewLogBroadcaster()
	go logBroadcaster.Start()

	// Logs go to stdout AND the websocket broadcaster
	multiWriter := logadapter.NewMultiWriter(os.Stdout, logBroadcaster)

	// 2. Setup global logger using the multi-writer
	slogger := slog.New(slog.NewJSONHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(slogger)
	logger := logadapter.NewSlogAdapter(slogger)

	// 3. Instantiate adapters (the concrete implementations)
	configProvider := config.NewJSONProvider(bucardoConfigPath)
	credentialManager := postgres.NewPgpassManager(logger, pgpassPath, bucardoUser)
	bucardoExecutor := bucardo.NewCLIExecutor(logger, bucardoUser, bucardoCmd)
	monitor := bucardo.NewMonitorAdapter(logger, bucardoLogPath, bucardoUser, bucardoCmd)

	// 4. Instantiate the core service
	appService := orchestrator.NewService(
		logger,
		configProvider,
		credentialManager,
		bucardoExecutor,
		monitor,
		bucardoConfigPath,
		pgpassPath,
		bucardoUser,
		bucardoCmd,
		bucardoLogPath,
	)

	// 5. Instantiate and start HTTP server
	httpServer := server.NewHTTPServer(logger, appService, logBroadcaster, httpPort)
	go httpServer.Start()

	// 6. Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		slogger.Info("Received signal, initiating shutdown...", "signal", sig)
		if err := httpServer.Stop(context.Background()); err != nil {
			slogger.Error("Failed to shutdown HTTP server gracefully", "error", err)
		}
		cancel()
	}()

	// 7. Run the application
	if err := appService.Run(ctx); err != nil {
		slogger.Error("Application exited with an error", "error", err)
		os.Exit(1)
	}

	slogger.Info("Application finished successfully.")
}
