package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joeychilson/boxbox/daytona"
	"github.com/joeychilson/boxbox/db"
)

// SandboxWorkDir is the working directory in Daytona sandboxes.
const SandboxWorkDir = "/home/daytona"

// ExecutionError represents an error during execution with context.
type ExecutionError struct {
	Phase   string
	Message string
	Err     error
}

// Error implements error interface.
func (e *ExecutionError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Phase, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Phase, e.Message)
}

// Unwrap implements error interface.
func (e *ExecutionError) Unwrap() error {
	return e.Err
}

// Executor handles code execution in sandboxes.
type Executor struct {
	db      *db.DB
	daytona *daytona.Client
	syncer  *Syncer
	logger  *slog.Logger
}

// NewExecutor creates a new executor.
func NewExecutor(database *db.DB, daytonaClient *daytona.Client, syncer *Syncer, logger *slog.Logger) *Executor {
	return &Executor{
		db:      database,
		daytona: daytonaClient,
		syncer:  syncer,
		logger:  logger.With("component", "executor"),
	}
}

// ExecutionResult is the result of an execution.
type ExecutionResult struct {
	ExecutionID     uuid.UUID
	Status          db.ExecutionStatus
	ExitCode        int
	Stdout          string
	Stderr          string
	ExecutionTimeMs int64
	OutputFiles     []OutputFile
}

// Execute runs code in a sandbox.
func (e *Executor) Execute(ctx context.Context, sandbox *db.Sandbox, execution *db.Execution, files []string) (*ExecutionResult, error) {
	startTime := time.Now()
	execLogger := e.logger.With(
		"execution_id", execution.ID,
		"sandbox_id", sandbox.ID,
		"language", execution.Language,
	)

	if err := e.db.UpdateExecutionSandbox(ctx, execution.ID, sandbox.ID); err != nil {
		return nil, &ExecutionError{Phase: "init", Message: "failed to link sandbox", Err: err}
	}

	if err := e.db.UpdateExecutionStatus(ctx, execution.ID, db.ExecutionStatusSyncingIn); err != nil {
		execLogger.Warn("failed to update status", "error", err)
	}

	syncedFiles, syncInErr := e.syncer.SyncToSandbox(ctx, execution.UserID, execution.ChatID, sandbox.DaytonaID, files)
	if syncInErr != nil {
		if errors.Is(syncInErr, daytona.ErrRateLimited) {
			return nil, &ExecutionError{Phase: "sync_in", Message: "rate limited by Daytona", Err: syncInErr}
		}
		if errors.Is(syncInErr, daytona.ErrSandboxNotFound) {
			return nil, &ExecutionError{Phase: "sync_in", Message: "sandbox not found", Err: syncInErr}
		}
		execLogger.Warn("sync to sandbox had errors", "error", syncInErr)
	}

	if len(syncedFiles) > 0 {
		inputFiles := make([]db.FileInfo, len(syncedFiles))
		for i, f := range syncedFiles {
			inputFiles[i] = db.FileInfo{
				Path:      f.Path,
				MediaType: f.MediaType,
				Size:      f.Size,
			}
		}
		if data, err := json.Marshal(inputFiles); err != nil {
			execLogger.Warn("failed to marshal input files", "error", err)
		} else if err := e.db.UpdateExecutionInputFiles(ctx, execution.ID, data); err != nil {
			execLogger.Warn("failed to update input files", "error", err)
		}
	}

	if err := e.db.UpdateExecutionStatus(ctx, execution.ID, db.ExecutionStatusExecuting); err != nil {
		execLogger.Warn("failed to update status", "error", err)
	}

	execResult, execErr := e.executeCode(ctx, sandbox.DaytonaID, execution)
	executionTime := time.Since(startTime)

	if err := e.db.UpdateExecutionStatus(ctx, execution.ID, db.ExecutionStatusSyncingOut); err != nil {
		execLogger.Warn("failed to update status", "error", err)
	}

	outputFiles, syncOutErr := e.syncer.SyncFromSandbox(ctx, sandbox.DaytonaID, execution.UserID, execution.ChatID, syncedFiles)
	if syncOutErr != nil {
		execLogger.Warn("sync from sandbox had errors", "error", syncOutErr)
	}

	status := db.ExecutionStatusCompleted
	exitCode := 0
	stdout := ""
	stderr := ""

	if execResult != nil {
		exitCode = execResult.ExitCode
		stdout = execResult.Result
		if exitCode != 0 {
			status = db.ExecutionStatusFailed
		}
	}

	if execErr != nil {
		status = db.ExecutionStatusFailed
		stderr = execErr.Error()

		if errors.Is(execErr, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			status = db.ExecutionStatusTimeout
			stderr = fmt.Sprintf("execution killed after %d seconds", execution.TimeoutSeconds)
		} else if errors.Is(execErr, daytona.ErrRateLimited) {
			stderr = "execution failed: rate limited by Daytona API"
		} else if errors.Is(execErr, daytona.ErrSandboxNotFound) {
			stderr = "execution failed: sandbox was terminated unexpectedly"
		}
	}

	const maxOutputLen = 100000 // 100KB
	if len(stdout) > maxOutputLen {
		stdout = stdout[:maxOutputLen] + "\n... (output truncated)"
	}
	if len(stderr) > maxOutputLen {
		stderr = stderr[:maxOutputLen] + "\n... (output truncated)"
	}

	var outputFilesJSON []byte
	if len(outputFiles) > 0 {
		dbFiles := make([]db.FileInfo, len(outputFiles))
		for i, f := range outputFiles {
			dbFiles[i] = db.FileInfo{
				Path:      f.Path,
				MediaType: f.MediaType,
				Size:      f.Size,
			}
		}
		var err error
		outputFilesJSON, err = json.Marshal(dbFiles)
		if err != nil {
			execLogger.Warn("failed to marshal output files", "error", err)
		}
	}

	if err := e.db.CompleteExecution(ctx, execution.ID, status, exitCode, stdout, stderr, int(executionTime.Milliseconds()), outputFilesJSON); err != nil {
		execLogger.Error("failed to complete execution", "error", err)
	}

	execLogger.Info("execution complete",
		"status", status,
		"exit_code", exitCode,
		"duration_ms", executionTime.Milliseconds(),
		"output_files", len(outputFiles),
	)

	return &ExecutionResult{
		ExecutionID:     execution.ID,
		Status:          status,
		ExitCode:        exitCode,
		Stdout:          stdout,
		Stderr:          stderr,
		ExecutionTimeMs: executionTime.Milliseconds(),
		OutputFiles:     outputFiles,
	}, nil
}

// executeCode executes the code in the sandbox.
func (e *Executor) executeCode(ctx context.Context, sandboxID string, execution *db.Execution) (*daytona.ProcessExecuteResponse, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(execution.TimeoutSeconds)*time.Second)
	defer cancel()

	codePath := SandboxWorkDir + "/" + execution.Path
	if err := e.daytona.UploadFile(timeoutCtx, sandboxID, codePath, strings.NewReader(execution.Code)); err != nil {
		return nil, fmt.Errorf("uploading code: %w", err)
	}

	var command string
	switch execution.Language {
	case "python":
		command = "python -u " + execution.Path
	case "bash", "shell":
		command = "bash " + execution.Path
	default:
		return nil, fmt.Errorf("unsupported language: %s (supported: python, bash)", execution.Language)
	}

	return e.daytona.Execute(timeoutCtx, sandboxID, &daytona.ProcessExecuteRequest{
		Command: command,
		Cwd:     SandboxWorkDir,
		Timeout: execution.TimeoutSeconds,
	})
}
