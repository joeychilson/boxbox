package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/joeychilson/boxbox/db"
	"github.com/joeychilson/boxbox/queue"
)

// maxBodySize is the maximum allowed size for request bodies.
const maxBodySize = 10 * 1024 * 1024

// ExecuteRequest is the request body for POST /execute.
type ExecuteRequest struct {
	Language       string   `json:"language"`
	Path           string   `json:"path"`
	Code           string   `json:"code"`
	Files          []string `json:"files,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// ExecuteResponse is the response for POST /execute.
type ExecuteResponse struct {
	ExecutionID     string       `json:"execution_id"`
	Status          string       `json:"status"`
	ExitCode        int          `json:"exit_code"`
	Stdout          string       `json:"stdout"`
	Stderr          string       `json:"stderr"`
	ExecutionTimeMs int64        `json:"execution_time_ms"`
	OutputFiles     []OutputFile `json:"output_files"`
}

// OutputFile represents a file created during execution.
type OutputFile struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

// ErrorResponse is returned for API errors.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// handleHealth handles the health check endpoint.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

// handleExecute handles the execute endpoint.
func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := GetUserID(ctx)
	chatID := GetChatID(ctx)

	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "failed to parse request body")
		return
	}

	if req.Language == "" {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "language is required")
		return
	}
	if req.Language != "python" && req.Language != "bash" && req.Language != "shell" {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "language must be 'python' or 'bash'")
		return
	}
	if req.Path == "" {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "path is required")
		return
	}
	if req.Code == "" {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	}

	if err := s.queue.Acquire(ctx); err != nil {
		if errors.Is(err, queue.ErrQueueClosed) {
			s.writeError(w, http.StatusServiceUnavailable, "server_shutting_down", "server is shutting down, please retry")
			return
		}
		if errors.Is(err, queue.ErrQueueTimeout) {
			s.writeError(w, http.StatusServiceUnavailable, "queue_timeout", "execution queue is full, please retry later")
			return
		}
		return
	}
	defer s.queue.Release()

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = int(s.cfg.ExecutionTimeout.Seconds())
	}
	if timeout > 600 {
		timeout = 600
	}

	execution, err := s.db.CreateExecution(ctx, userID, chatID, req.Language, req.Path, req.Code, timeout)
	if err != nil {
		s.logger.Error("failed to create execution", "error", err)
		s.writeError(w, http.StatusInternalServerError, "internal_error", "failed to create execution")
		return
	}

	s.logger.Info("execution started",
		"execution_id", execution.ID,
		"user_id", userID,
		"chat_id", chatID,
		"language", req.Language,
		"path", req.Path,
		"files", req.Files,
	)

	sbx, err := s.pool.ClaimSandbox(ctx, execution.ID)
	if errors.Is(err, db.ErrNoAvailableSandbox) {
		_ = s.db.CompleteExecution(ctx, execution.ID, db.ExecutionStatusFailed, 1, "", "no sandbox available", 0, nil)
		s.writeError(w, http.StatusServiceUnavailable, "no_sandbox_available", "no sandbox available, please retry")
		return
	}
	if err != nil {
		s.logger.Error("failed to claim sandbox", "error", err)
		_ = s.db.CompleteExecution(ctx, execution.ID, db.ExecutionStatusFailed, 1, "", "failed to claim sandbox", 0, nil)
		s.writeError(w, http.StatusInternalServerError, "internal_error", "failed to claim sandbox")
		return
	}

	if err := s.pool.MarkExecuting(ctx, sbx.ID); err != nil {
		s.logger.Error("failed to mark executing", "error", err)
	}

	result, err := s.executor.Execute(ctx, sbx, execution, req.Files)

	s.pool.DeleteSandbox(ctx, sbx, "execution completed")

	if err != nil {
		s.logger.Error("execution failed", "execution_id", execution.ID, "error", err)
		s.writeError(w, http.StatusInternalServerError, "execution_error", err.Error())
		return
	}

	response := ExecuteResponse{
		ExecutionID:     result.ExecutionID.String(),
		Status:          string(result.Status),
		ExitCode:        result.ExitCode,
		Stdout:          result.Stdout,
		Stderr:          result.Stderr,
		ExecutionTimeMs: result.ExecutionTimeMs,
		OutputFiles:     make([]OutputFile, len(result.OutputFiles)),
	}

	for i, f := range result.OutputFiles {
		response.OutputFiles[i] = OutputFile{
			Path:      f.Path,
			MediaType: f.MediaType,
			Size:      f.Size,
		}
	}

	s.writeJSON(w, http.StatusOK, response)
}

// writeJSON writes the given data as JSON to the response writer.
func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		s.logger.Error("failed to encode response", "error", err)
	}
}

// writeError writes the given error as JSON to the response writer.
func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	s.writeJSON(w, status, ErrorResponse{
		Error:   code,
		Message: message,
	})
}
