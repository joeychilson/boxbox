package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joeychilson/boxbox/config"
	"github.com/joeychilson/boxbox/daytona"
	"github.com/joeychilson/boxbox/db"
	"github.com/joeychilson/boxbox/pool"
	"github.com/joeychilson/boxbox/s3"
	"github.com/joeychilson/boxbox/sandbox"
	"github.com/joeychilson/boxbox/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		logger.Info("received shutdown signal", "signal", sig)
		cancel()
	}()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger.Info("configuration loaded")

	database, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	logger.Info("database connected")

	if err := database.Migrate(ctx); err != nil {
		return err
	}
	logger.Info("database migrations applied")

	s3Client, err := s3.New(
		ctx,
		cfg.S3Bucket,
		cfg.S3Region,
		cfg.S3AccessKeyID,
		cfg.S3SecretAccessKey,
		cfg.S3Endpoint,
	)
	if err != nil {
		return err
	}
	logger.Info("S3 client initialized")

	daytonaClient := daytona.NewClient(cfg.DaytonaAPIURL, cfg.DaytonaAPIKey)
	if err := daytonaClient.Init(ctx); err != nil {
		return err
	}
	logger.Info("Daytona client initialized")

	syncer := sandbox.NewSyncer(s3Client, daytonaClient, cfg.SyncConcurrency)
	executor := sandbox.NewExecutor(database, daytonaClient, syncer, logger)

	poolMgr := pool.New(database, daytonaClient, cfg, logger)
	poolMgr.Start(ctx)
	defer poolMgr.Stop()
	logger.Info("pool manager started")

	srv := server.New(database, poolMgr, executor, s3Client, cfg, logger)
	return srv.Start(ctx)
}
