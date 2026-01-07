package pool

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/joeychilson/boxbox/config"
	"github.com/joeychilson/boxbox/daytona"
	"github.com/joeychilson/boxbox/db"
	"github.com/joeychilson/boxbox/image"
)

// BoxboxLabel is the label used to identify sandboxes created by boxbox.
const BoxboxLabel = "created_by"
const BoxboxLabelValue = "boxbox"

// Manager manages the sandbox pool with health verification and cleanup.
type Manager struct {
	db             *db.DB
	daytona        *daytona.Client
	cfg            *config.Config
	logger         *slog.Logger
	stopCh         chan struct{}
	wg             sync.WaitGroup
	createdCount   atomic.Int64
	deletedCount   atomic.Int64
	failedCount    atomic.Int64
	claimedCount   atomic.Int64
	healthyCount   atomic.Int64
	unhealthyCount atomic.Int64
}

// PoolMetrics contains pool operation metrics.
type PoolMetrics struct {
	Created   int64
	Deleted   int64
	Failed    int64
	Claimed   int64
	Healthy   int64
	Unhealthy int64
}

// New creates a new pool manager.
func New(database *db.DB, daytonaClient *daytona.Client, cfg *config.Config, logger *slog.Logger) *Manager {
	return &Manager{
		db:      database,
		daytona: daytonaClient,
		cfg:     cfg,
		logger:  logger.With("component", "pool"),
		stopCh:  make(chan struct{}),
	}
}

// GetMetrics returns pool operation metrics.
func (m *Manager) GetMetrics() PoolMetrics {
	return PoolMetrics{
		Created:   m.createdCount.Load(),
		Deleted:   m.deletedCount.Load(),
		Failed:    m.failedCount.Load(),
		Claimed:   m.claimedCount.Load(),
		Healthy:   m.healthyCount.Load(),
		Unhealthy: m.unhealthyCount.Load(),
	}
}

// Start starts the pool manager background workers.
func (m *Manager) Start(ctx context.Context) {
	m.cleanupOrphans(ctx)
	m.reconcileWithDaytona(ctx)

	m.wg.Add(3)

	go func() {
		defer m.wg.Done()
		m.runScaler(ctx)
	}()

	go func() {
		defer m.wg.Done()
		m.runReaper(ctx)
	}()

	go func() {
		defer m.wg.Done()
		m.runHealthChecker(ctx)
	}()
}

// Stop stops the pool manager.
func (m *Manager) Stop() {
	close(m.stopCh)
	m.wg.Wait()
}

// ClaimSandbox claims an available sandbox for execution with health verification.
func (m *Manager) ClaimSandbox(ctx context.Context, executionID uuid.UUID) (*db.Sandbox, error) {
	const maxAttempts = 3

	for range maxAttempts {
		sandbox, err := m.db.ClaimAvailableSandbox(ctx, executionID)
		if err != nil {
			return nil, err
		}

		healthy, err := m.daytona.IsHealthy(ctx, sandbox.DaytonaID)
		if err != nil {
			m.logger.Warn("health check failed, deleting sandbox",
				"sandbox_id", sandbox.ID,
				"daytona_id", sandbox.DaytonaID,
				"error", err,
			)
			m.unhealthyCount.Add(1)
			m.DeleteSandbox(ctx, sandbox, "health check failed")
			continue
		}

		if !healthy {
			m.logger.Warn("sandbox unhealthy, deleting",
				"sandbox_id", sandbox.ID,
				"daytona_id", sandbox.DaytonaID,
			)
			m.unhealthyCount.Add(1)
			m.DeleteSandbox(ctx, sandbox, "unhealthy")
			continue
		}

		m.healthyCount.Add(1)
		m.claimedCount.Add(1)
		return sandbox, nil
	}

	return nil, db.ErrNoAvailableSandbox
}

// MarkExecuting marks a sandbox as executing.
func (m *Manager) MarkExecuting(ctx context.Context, sandboxID uuid.UUID) error {
	return m.db.UpdateSandboxState(ctx, sandboxID, db.SandboxStateExecuting)
}

// DeleteSandbox deletes a sandbox after use with verification.
func (m *Manager) DeleteSandbox(ctx context.Context, sandbox *db.Sandbox, reason string) {
	m.logger.Info("deleting sandbox", "sandbox_id", sandbox.ID, "daytona_id", sandbox.DaytonaID, "reason", reason)

	_ = m.db.UpdateSandboxState(ctx, sandbox.ID, db.SandboxStateDeleting)

	go func() {
		deleteCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := m.daytona.DeleteAndVerify(deleteCtx, sandbox.DaytonaID); err != nil {
			if !errors.Is(err, daytona.ErrSandboxNotFound) {
				m.logger.Warn("failed to delete sandbox from Daytona",
					"error", err,
					"sandbox_id", sandbox.ID,
					"daytona_id", sandbox.DaytonaID,
				)
				m.failedCount.Add(1)
				_ = m.db.UpdateSandboxStateWithError(deleteCtx, sandbox.ID, db.SandboxStateFailed, err.Error())
				return
			}
		}

		if err := m.db.DeleteSandbox(deleteCtx, sandbox.ID); err != nil {
			m.logger.Error("failed to mark sandbox as deleted in DB", "error", err, "sandbox_id", sandbox.ID)
		}

		m.deletedCount.Add(1)
		m.logger.Debug("sandbox deleted successfully", "sandbox_id", sandbox.ID, "daytona_id", sandbox.DaytonaID)
	}()
}

// GetStats returns pool statistics.
func (m *Manager) GetStats(ctx context.Context) (*db.PoolStats, error) {
	return m.db.GetPoolStats(ctx)
}

// cleanupOrphans cleans up sandboxes that are in non-terminal states on startup.
func (m *Manager) cleanupOrphans(ctx context.Context) {
	m.logger.Info("cleaning up orphaned sandboxes on startup")

	orphans, err := m.db.GetOrphanedSandboxes(ctx)
	if err != nil {
		m.logger.Error("failed to get orphaned sandboxes", "error", err)
		return
	}

	if len(orphans) == 0 {
		m.logger.Info("no orphaned sandboxes found")
		return
	}

	m.logger.Warn("found orphaned sandboxes", "count", len(orphans))

	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup

	for _, s := range orphans {
		wg.Add(1)
		go func(sandbox db.Sandbox) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			m.logger.Info("cleaning up orphaned sandbox",
				"sandbox_id", sandbox.ID,
				"daytona_id", sandbox.DaytonaID,
				"state", sandbox.State,
			)

			deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			if err := m.daytona.Delete(deleteCtx, sandbox.DaytonaID); err != nil && !errors.Is(err, daytona.ErrSandboxNotFound) {
				m.logger.Warn("failed to delete orphan from Daytona", "error", err, "daytona_id", sandbox.DaytonaID)
			}

			if err := m.db.DeleteSandbox(ctx, sandbox.ID); err != nil {
				m.logger.Error("failed to mark orphan as deleted", "error", err, "sandbox_id", sandbox.ID)
			}

			m.deletedCount.Add(1)
		}(s)
	}

	wg.Wait()
	m.logger.Info("orphan cleanup complete", "cleaned", len(orphans))
}

// reconcileWithDaytona finds sandboxes in Daytona that aren't tracked locally.
func (m *Manager) reconcileWithDaytona(ctx context.Context) {
	m.logger.Info("reconciling sandboxes with Daytona")

	daytonaSandboxes, err := m.daytona.List(ctx, map[string]string{BoxboxLabel: BoxboxLabelValue})
	if err != nil {
		m.logger.Error("failed to list sandboxes from Daytona", "error", err)
		return
	}

	if len(daytonaSandboxes) == 0 {
		m.logger.Info("no boxbox sandboxes found in Daytona")
		return
	}

	knownIDs, err := m.db.GetAllDaytonaIDs(ctx)
	if err != nil {
		m.logger.Error("failed to get known Daytona IDs", "error", err)
		return
	}

	var untracked []daytona.Sandbox
	for _, s := range daytonaSandboxes {
		if !knownIDs[s.ID] {
			untracked = append(untracked, s)
		}
	}

	if len(untracked) == 0 {
		m.logger.Info("no untracked sandboxes found", "daytona_count", len(daytonaSandboxes), "known_count", len(knownIDs))
		return
	}

	m.logger.Info("found untracked sandboxes in Daytona", "count", len(untracked))

	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	var imported, deleted int64

	for _, s := range untracked {
		wg.Add(1)
		go func(sandbox daytona.Sandbox) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			if sandbox.IsHealthy() {
				m.logger.Info("importing healthy sandbox", "daytona_id", sandbox.ID, "state", sandbox.State)

				if _, err := m.db.ImportSandbox(opCtx, sandbox.ID); err != nil {
					m.logger.Error("failed to import sandbox", "error", err, "daytona_id", sandbox.ID)
					return
				}

				atomic.AddInt64(&imported, 1)
				m.createdCount.Add(1)
			} else {
				m.logger.Info("deleting unhealthy untracked sandbox", "daytona_id", sandbox.ID, "state", sandbox.State)

				if err := m.daytona.Delete(opCtx, sandbox.ID); err != nil && !errors.Is(err, daytona.ErrSandboxNotFound) {
					m.logger.Warn("failed to delete unhealthy sandbox", "error", err, "daytona_id", sandbox.ID)
					return
				}

				atomic.AddInt64(&deleted, 1)
				m.deletedCount.Add(1)
			}
		}(s)
	}

	wg.Wait()
	m.logger.Info("reconciliation complete", "imported", imported, "deleted", deleted)
}

// runScaler maintains the warm pool size.
func (m *Manager) runScaler(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.scale(ctx)
		}
	}
}

// scale maintains the warm pool size.
func (m *Manager) scale(ctx context.Context) {
	stats, err := m.db.GetPoolStats(ctx)
	if err != nil {
		m.logger.Error("failed to get pool stats", "error", err)
		return
	}

	usableOrPending := stats.Available + stats.Creating + stats.Warming

	if usableOrPending < m.cfg.PoolMinWarm {
		toCreate := m.cfg.PoolMinWarm - usableOrPending
		if toCreate > 0 {
			m.logger.Info("scaling up",
				"available", stats.Available,
				"creating", stats.Creating,
				"warming", stats.Warming,
				"to_create", toCreate,
			)
			for range toCreate {
				go m.createSandbox(ctx)
			}
		}
	}

	if stats.Available > m.cfg.PoolScaleDownThreshold {
		toDelete := stats.Available - m.cfg.PoolMaxWarm
		if toDelete > 0 {
			m.logger.Info("scaling down", "available", stats.Available, "to_delete", toDelete)
			sandboxes, err := m.db.GetOldAvailableSandboxes(ctx, m.cfg.PoolSandboxTTL, toDelete)
			if err != nil {
				m.logger.Error("failed to get old sandboxes", "error", err)
				return
			}
			for _, s := range sandboxes {
				m.DeleteSandbox(ctx, &s, "scale down")
			}
		}
	}
}

// createSandbox creates a new sandbox.
func (m *Manager) createSandbox(ctx context.Context) {
	m.logger.Debug("creating sandbox")

	if m.daytona.IsRateLimited() {
		m.logger.Debug("skipping sandbox creation, rate limited")
		return
	}

	req := &daytona.CreateSandboxRequest{
		Labels:   map[string]string{BoxboxLabel: BoxboxLabelValue},
		CPU:      m.cfg.SandboxCPU,
		Memory:   m.cfg.SandboxMemory,
		Disk:     m.cfg.SandboxDisk,
		AutoStop: 60,
		Timeout:  120,
	}

	if m.cfg.DaytonaImage != "" {
		req.Image = m.cfg.DaytonaImage
		m.logger.Debug("using configured image", "image", m.cfg.DaytonaImage)
	} else {
		req.BuildInfo = &daytona.BuildInfo{DockerfileContent: image.DockerfileContent}
		m.logger.Debug("using embedded Dockerfile for dynamic build")
	}

	sandbox, err := m.daytona.Create(ctx, req)
	if err != nil {
		if errors.Is(err, daytona.ErrRateLimited) {
			m.logger.Warn("rate limited when creating sandbox")
		} else {
			m.logger.Error("failed to create sandbox in Daytona", "error", err)
			m.failedCount.Add(1)
		}
		return
	}

	dbSandbox, err := m.db.CreateSandbox(ctx, sandbox.ID)
	if err != nil {
		m.logger.Error("failed to create sandbox in DB", "error", err)
		m.failedCount.Add(1)
		_ = m.daytona.Delete(ctx, sandbox.ID)
		return
	}

	if err := m.db.UpdateSandboxState(ctx, dbSandbox.ID, db.SandboxStateWarming); err != nil {
		m.logger.Error("failed to mark sandbox as warming", "error", err)
		m.failedCount.Add(1)
		return
	}

	if err := m.daytona.StartAndWait(ctx, sandbox.ID, 2*time.Minute); err != nil {
		m.logger.Error("failed to start sandbox", "error", err)
		m.failedCount.Add(1)
		_ = m.db.UpdateSandboxStateWithError(ctx, dbSandbox.ID, db.SandboxStateFailed, err.Error())
		_ = m.daytona.Delete(ctx, sandbox.ID)
		return
	}

	if err := m.db.UpdateSandboxState(ctx, dbSandbox.ID, db.SandboxStateAvailable); err != nil {
		m.logger.Error("failed to mark sandbox as available", "error", err)
		m.failedCount.Add(1)
		return
	}

	m.createdCount.Add(1)
	m.logger.Info("sandbox created and ready", "sandbox_id", dbSandbox.ID, "daytona_id", sandbox.ID)
}

// runReaper cleans up stale and failed sandboxes.
func (m *Manager) runReaper(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.reap(ctx)
		}
	}
}

// reap cleans up stale and failed sandboxes.
func (m *Manager) reap(ctx context.Context) {
	stale, err := m.db.GetStaleSandboxes(ctx, 5*time.Minute)
	if err != nil {
		m.logger.Error("failed to get stale sandboxes", "error", err)
		return
	}

	for _, s := range stale {
		m.logger.Warn("reaping stale sandbox", "sandbox_id", s.ID, "state", s.State, "age", time.Since(s.CreatedAt))
		m.DeleteSandbox(ctx, &s, "stale")
	}

	failed, err := m.db.GetFailedSandboxes(ctx)
	if err != nil {
		m.logger.Error("failed to get failed sandboxes", "error", err)
		return
	}

	for _, s := range failed {
		m.logger.Warn("reaping failed sandbox", "sandbox_id", s.ID, "error", s.ErrorMessage)
		m.DeleteSandbox(ctx, &s, "failed")
	}
}

// runHealthChecker periodically verifies available sandboxes are healthy.
func (m *Manager) runHealthChecker(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.checkHealth(ctx)
		}
	}
}

// checkHealth verifies available sandboxes are healthy.
func (m *Manager) checkHealth(ctx context.Context) {
	sandboxes, err := m.db.GetAvailableSandboxes(ctx, 5)
	if err != nil {
		m.logger.Error("failed to get sandboxes for health check", "error", err)
		return
	}

	for _, s := range sandboxes {
		healthy, err := m.daytona.IsHealthy(ctx, s.DaytonaID)
		if err != nil {
			if errors.Is(err, daytona.ErrSandboxNotFound) {
				m.logger.Warn("sandbox not found in Daytona during health check",
					"sandbox_id", s.ID,
					"daytona_id", s.DaytonaID,
				)
				m.unhealthyCount.Add(1)
				m.DeleteSandbox(ctx, &s, "not found in Daytona")
			}
			continue
		}

		if !healthy {
			m.logger.Warn("sandbox unhealthy during health check",
				"sandbox_id", s.ID,
				"daytona_id", s.DaytonaID,
			)
			m.unhealthyCount.Add(1)
			m.DeleteSandbox(ctx, &s, "unhealthy during health check")
		}
	}
}
