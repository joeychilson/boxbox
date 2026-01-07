package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Error types for Daytona API.
var (
	ErrRateLimited           = errors.New("rate limited by Daytona API")
	ErrSandboxNotFound       = errors.New("sandbox not found")
	ErrSandboxStopped        = errors.New("sandbox is stopped")
	ErrQuotaExceeded         = errors.New("Daytona quota exceeded")
	ErrStateChangeInProgress = errors.New("state change in progress")
)

// SandboxState represents the state of a Daytona sandbox.
type SandboxState string

const (
	SandboxStateStarting SandboxState = "starting"
	SandboxStateStarted  SandboxState = "started"
	SandboxStateRunning  SandboxState = "running"
	SandboxStateStopped  SandboxState = "stopped"
	SandboxStateError    SandboxState = "error"
)

// Sandbox represents a Daytona sandbox.
type Sandbox struct {
	ID        string       `json:"id"`
	State     SandboxState `json:"state"`
	CreatedAt time.Time    `json:"createdAt"`
	UpdatedAt time.Time    `json:"updatedAt"`
}

// IsHealthy returns true if the sandbox is in a healthy running state.
func (s *Sandbox) IsHealthy() bool {
	return s.State == SandboxStateStarted || s.State == SandboxStateRunning
}

// BuildInfo contains dockerfile content for dynamic image builds.
type BuildInfo struct {
	DockerfileContent string `json:"dockerfileContent,omitempty"`
}

// CreateSandboxRequest is the request to create a sandbox.
type CreateSandboxRequest struct {
	Image            string            `json:"image,omitempty"`
	BuildInfo        *BuildInfo        `json:"buildInfo,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Public           bool              `json:"public,omitempty"`
	CPU              int               `json:"cpu,omitempty"`
	Memory           int               `json:"memory,omitempty"`
	Disk             int               `json:"disk,omitempty"`
	GPU              string            `json:"gpu,omitempty"`
	AutoStopInterval *int32            `json:"autoStopInterval,omitempty"`
	User             string            `json:"user,omitempty"`
}

// ProcessExecuteRequest is the request to execute a command.
type ProcessExecuteRequest struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
}

// ProcessExecuteResponse is the response from executing a command.
type ProcessExecuteResponse struct {
	ExitCode int    `json:"exitCode"`
	Result   string `json:"result"`
}

// FileInfo represents file metadata from the sandbox.
type FileInfo struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"modTime"`
}

// ErrorResponse represents an error from the Daytona API.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Client is the Daytona API client with rate limit handling.
type Client struct {
	baseURL       string
	toolboxURL    string
	apiKey        string
	httpClient    *http.Client
	mu            sync.RWMutex
	rateLimitedAt time.Time
	retryAfter    time.Duration
}

// DaytonaConfig represents the configuration returned by the Daytona API.
type DaytonaConfig struct {
	ProxyToolboxURL string `json:"proxyToolboxUrl"`
}

// NewClient creates a new Daytona API client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

// Init fetches the toolbox URL from the Daytona config endpoint.
func (c *Client) Init(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/config", nil)
	if err != nil {
		return fmt.Errorf("creating config request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("fetching config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("config request failed (%d): %s", resp.StatusCode, string(body))
	}

	var cfg DaytonaConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return fmt.Errorf("decoding config: %w", err)
	}

	if cfg.ProxyToolboxURL == "" {
		return fmt.Errorf("proxyToolboxUrl not found in config")
	}

	c.toolboxURL = cfg.ProxyToolboxURL
	return nil
}

// Create creates a new sandbox with retry logic for rate limits.
func (c *Client) Create(ctx context.Context, req *CreateSandboxRequest) (*Sandbox, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/sandbox", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	c.setHeaders(httpReq)

	resp, err := c.doWithRetry(ctx, httpReq, 3)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, c.parseError(resp)
	}

	var sandbox Sandbox
	if err := json.NewDecoder(resp.Body).Decode(&sandbox); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &sandbox, nil
}

// List retrieves all sandboxes matching the given labels.
func (c *Client) List(ctx context.Context, labels map[string]string) ([]Sandbox, error) {
	u := c.baseURL + "/sandbox"
	if len(labels) > 0 {
		labelsJSON, err := json.Marshal(labels)
		if err != nil {
			return nil, fmt.Errorf("marshaling labels: %w", err)
		}
		u += "?labels=" + url.QueryEscape(string(labelsJSON))
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.doWithRetry(ctx, httpReq, 2)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var sandboxes []Sandbox
	if err := json.NewDecoder(resp.Body).Decode(&sandboxes); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return sandboxes, nil
}

// Get retrieves a sandbox by ID with retry logic.
func (c *Client) Get(ctx context.Context, id string) (*Sandbox, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/sandbox/"+id, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.doWithRetry(ctx, httpReq, 2)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var sandbox Sandbox
	if err := json.NewDecoder(resp.Body).Decode(&sandbox); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &sandbox, nil
}

// IsHealthy checks if a sandbox is healthy and running.
func (c *Client) IsHealthy(ctx context.Context, id string) (bool, error) {
	sandbox, err := c.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			return false, nil
		}
		return false, err
	}
	return sandbox.IsHealthy(), nil
}

// Delete deletes a sandbox with retry logic.
func (c *Client) Delete(ctx context.Context, id string) error {
	httpReq, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+"/sandbox/"+id, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.doWithRetry(ctx, httpReq, 3)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return c.parseError(resp)
	}

	return nil
}

// DeleteAndVerify deletes a sandbox and verifies it's actually gone.
func (c *Client) DeleteAndVerify(ctx context.Context, id string) error {
	if err := c.Delete(ctx, id); err != nil {
		if !errors.Is(err, ErrStateChangeInProgress) {
			return fmt.Errorf("deleting sandbox: %w", err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, err := c.Get(ctx, id)
		if errors.Is(err, ErrSandboxNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("verifying deletion: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	return fmt.Errorf("sandbox %s still exists after deletion", id)
}

// Start starts a sandbox with retry logic.
func (c *Client) Start(ctx context.Context, id string) error {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/sandbox/"+id+"/start", nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.doWithRetry(ctx, httpReq, 3)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return c.parseError(resp)
	}

	return nil
}

// StartAndWait starts a sandbox and waits for it to be running.
func (c *Client) StartAndWait(ctx context.Context, id string, timeout time.Duration) error {
	if err := c.Start(ctx, id); err != nil {
		if !errors.Is(err, ErrStateChangeInProgress) {
			return err
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sandbox, err := c.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("checking sandbox state: %w", err)
		}
		if sandbox.IsHealthy() {
			return nil
		}
		if sandbox.State == SandboxStateError {
			return fmt.Errorf("sandbox entered error state")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for sandbox to start")
}

// Execute runs a command in a sandbox via the toolbox API.
func (c *Client) Execute(ctx context.Context, id string, req *ProcessExecuteRequest) (*ProcessExecuteResponse, error) {
	if c.isRateLimited() {
		return nil, ErrRateLimited
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	toolboxEndpoint := c.toolboxURL + "/" + id + "/process/execute"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", toolboxEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		c.setRateLimited(retryAfter)
		return nil, ErrRateLimited
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	c.clearRateLimit()
	var result ProcessExecuteResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &result, nil
}

// ListFiles lists files in a sandbox directory via the toolbox API.
func (c *Client) ListFiles(ctx context.Context, id, path string) ([]FileInfo, error) {
	u := c.toolboxURL + "/" + id + "/files"
	if path != "" {
		u += "?path=" + url.QueryEscape(path)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.doWithRetry(ctx, httpReq, 2)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var files []FileInfo
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return files, nil
}

// UploadFile uploads a file to a sandbox via the toolbox API using multipart/form-data.
func (c *Client) UploadFile(ctx context.Context, id, path string, content io.Reader) error {
	if c.isRateLimited() {
		return ErrRateLimited
	}

	contentBytes, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("reading content: %w", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return fmt.Errorf("creating form file: %w", err)
	}
	if _, err := part.Write(contentBytes); err != nil {
		return fmt.Errorf("writing form content: %w", err)
	}
	writer.Close()

	u := c.toolboxURL + "/" + id + "/files/upload?path=" + url.QueryEscape(path)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", u, &body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		c.setRateLimited(retryAfter)
		return ErrRateLimited
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return c.parseError(resp)
	}

	c.clearRateLimit()
	return nil
}

// DownloadFile downloads a file from a sandbox via the toolbox API.
func (c *Client) DownloadFile(ctx context.Context, id, path string) (io.ReadCloser, error) {
	if c.isRateLimited() {
		return nil, ErrRateLimited
	}

	u := c.toolboxURL + "/" + id + "/files/download?path=" + url.QueryEscape(path)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Accept", "application/octet-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		c.setRateLimited(retryAfter)
		return nil, ErrRateLimited
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, c.parseError(resp)
	}

	c.clearRateLimit()
	return resp.Body, nil
}

// CreateFolder creates a directory in a sandbox via the toolbox API.
func (c *Client) CreateFolder(ctx context.Context, id, path, mode string) error {
	u := c.toolboxURL + "/" + id + "/files/folder?path=" + url.QueryEscape(path) + "&mode=" + url.QueryEscape(mode)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.doWithRetry(ctx, httpReq, 2)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return c.parseError(resp)
	}

	return nil
}

// IsRateLimited returns true if the client is currently rate limited.
func (c *Client) IsRateLimited() bool {
	return c.isRateLimited()
}

// isRateLimited checks if we're currently rate limited.
func (c *Client) isRateLimited() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.rateLimitedAt.IsZero() {
		return false
	}
	return time.Since(c.rateLimitedAt) < c.retryAfter
}

// setRateLimited marks the client as rate limited.
func (c *Client) setRateLimited(retryAfter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rateLimitedAt = time.Now()
	c.retryAfter = retryAfter
}

// clearRateLimit clears the rate limit state.
func (c *Client) clearRateLimit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rateLimitedAt = time.Time{}
	c.retryAfter = 0
}

// doWithRetry executes an HTTP request with exponential backoff for rate limits.
func (c *Client) doWithRetry(ctx context.Context, req *http.Request, maxRetries int) (*http.Response, error) {
	if c.isRateLimited() {
		return nil, ErrRateLimited
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		reqClone := req.Clone(ctx)
		if req.Body != nil && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("getting request body: %w", err)
			}
			reqClone.Body = body
		}

		resp, err := c.httpClient.Do(reqClone)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			c.setRateLimited(retryAfter)
			lastErr = ErrRateLimited
			continue
		}

		c.clearRateLimit()
		return resp, nil
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
}

func (c *Client) parseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var errResp ErrorResponse
	if json.Unmarshal(body, &errResp) == nil && errResp.Message != "" {
		if strings.Contains(strings.ToLower(errResp.Message), "state change in progress") {
			return ErrStateChangeInProgress
		}
		return fmt.Errorf("daytona API error (%d): %s", resp.StatusCode, errResp.Message)
	}
	bodyStr := string(body)
	if strings.Contains(strings.ToLower(bodyStr), "state change in progress") {
		return ErrStateChangeInProgress
	}
	return fmt.Errorf("daytona API error (%d): %s", resp.StatusCode, bodyStr)
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 30 * time.Second
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return 30 * time.Second
}
