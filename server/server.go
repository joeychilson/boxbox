package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joeychilson/boxbox/config"
	"github.com/joeychilson/boxbox/db"
	"github.com/joeychilson/boxbox/pool"
	"github.com/joeychilson/boxbox/queue"
	"github.com/joeychilson/boxbox/s3"
	"github.com/joeychilson/boxbox/sandbox"
)

// Server is the HTTP server.
type Server struct {
	router   chi.Router
	db       *db.DB
	pool     *pool.Manager
	executor *sandbox.Executor
	s3       *s3.Client
	queue    *queue.Queue
	cfg      *config.Config
	logger   *slog.Logger
}

// New creates a new HTTP server.
func New(
	database *db.DB,
	poolMgr *pool.Manager,
	executor *sandbox.Executor,
	s3Client *s3.Client,
	cfg *config.Config,
	logger *slog.Logger,
) *Server {
	s := &Server{
		db:       database,
		pool:     poolMgr,
		executor: executor,
		s3:       s3Client,
		queue:    queue.New(cfg.QueueMaxConcurrent, cfg.QueueWaitTimeout),
		cfg:      cfg,
		logger:   logger.With("component", "server"),
	}

	rateLimiter := NewRateLimiter(10, 20)

	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(CORS)
	r.Use(Recoverer(logger))
	r.Use(Logger(logger))

	r.Get("/health", s.handleHealth)

	r.Group(func(r chi.Router) {
		r.Use(APIKeyAuth(cfg.APIKey))
		r.Use(rateLimiter.RateLimit())

		r.Post("/execute", s.handleExecute)
	})

	s.router = r
	return s
}

// Start starts the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:         ":" + s.cfg.APIPort,
		Handler:      s.router,
		ReadTimeout:  10 * time.Minute,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	errChan := make(chan error, 1)
	go func() {
		s.logger.Info("server starting",
			"port", s.cfg.APIPort,
			"queue_max_concurrent", s.cfg.QueueMaxConcurrent,
			"queue_wait_timeout", s.cfg.QueueWaitTimeout,
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutting down server")

		s.queue.Close()
		metrics := s.queue.GetMetrics()
		s.logger.Info("queue closed",
			"waiting", metrics.Waiting,
			"active", metrics.Active,
			"total", metrics.Total,
			"timeouts", metrics.Timeouts,
		)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errChan:
		return err
	}
}
