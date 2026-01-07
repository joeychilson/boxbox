package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/joeychilson/boxbox/daytona"
	"github.com/joeychilson/boxbox/s3"
)

// Syncer handles bidirectional file sync between S3 and sandboxes.
type Syncer struct {
	s3      *s3.Client
	daytona *daytona.Client
	workers int
}

// NewSyncer creates a new file syncer.
func NewSyncer(s3Client *s3.Client, daytonaClient *daytona.Client, workers int) *Syncer {
	return &Syncer{
		s3:      s3Client,
		daytona: daytonaClient,
		workers: workers,
	}
}

// SyncedFile represents a file that was synced to the sandbox.
type SyncedFile struct {
	Path      string
	MediaType string
	Size      int64
}

// OutputFile represents a file created during execution.
type OutputFile struct {
	Path      string
	MediaType string
	Size      int64
}

// SyncToSandbox downloads specified files from S3 and uploads them to the sandbox.
func (s *Syncer) SyncToSandbox(ctx context.Context, userID, chatID, sandboxID string, requestedFiles []string) ([]SyncedFile, error) {
	if len(requestedFiles) == 0 {
		return nil, nil
	}

	requested := make(map[string]bool)
	for _, f := range requestedFiles {
		requested[f] = true
	}

	allFiles, err := s.s3.ListFiles(ctx, userID, chatID)
	if err != nil {
		return nil, fmt.Errorf("listing S3 files: %w", err)
	}

	var files []s3.FileInfo
	for _, f := range allFiles {
		if requested[f.Filename] {
			files = append(files, f)
		}
	}

	if len(files) == 0 {
		return nil, nil
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		synced  []SyncedFile
		errChan = make(chan error, len(files))
		sem     = make(chan struct{}, s.workers)
	)

	for _, file := range files {
		wg.Add(1)
		go func(f s3.FileInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := s.syncFileToSandbox(ctx, sandboxID, f); err != nil {
				errChan <- fmt.Errorf("syncing %s: %w", f.Filename, err)
				return
			}

			mu.Lock()
			synced = append(synced, SyncedFile{
				Path:      f.Filename,
				MediaType: f.MediaType,
				Size:      f.Size,
			})
			mu.Unlock()
		}(file)
	}

	wg.Wait()
	close(errChan)

	var errs []string
	for err := range errChan {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return synced, fmt.Errorf("sync errors: %s", strings.Join(errs, "; "))
	}

	return synced, nil
}

func (s *Syncer) syncFileToSandbox(ctx context.Context, sandboxID string, file s3.FileInfo) error {
	reader, err := s.s3.Download(ctx, file.Key)
	if err != nil {
		return fmt.Errorf("downloading from S3: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
	}

	sandboxPath := SandboxWorkDir + "/" + file.Filename
	if err := s.daytona.UploadFile(ctx, sandboxID, sandboxPath, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("uploading to sandbox: %w", err)
	}
	return nil
}

// SyncFromSandbox downloads new files from the sandbox and uploads them to S3.
func (s *Syncer) SyncFromSandbox(ctx context.Context, sandboxID, userID, chatID string, existingFiles []SyncedFile) ([]OutputFile, error) {
	existing := make(map[string]bool)
	for _, f := range existingFiles {
		existing[f.Path] = true
	}

	type fileWithPath struct {
		info daytona.FileInfo
		path string
	}
	var newFiles []fileWithPath

	workspaceFiles, err := s.daytona.ListFiles(ctx, sandboxID, SandboxWorkDir)
	if err == nil {
		for _, f := range workspaceFiles {
			if !f.IsDir && !existing[f.Name] && isOutputFile(f.Name) {
				newFiles = append(newFiles, fileWithPath{info: f, path: SandboxWorkDir + "/" + f.Name})
			}
		}
	}

	tmpFiles, err := s.daytona.ListFiles(ctx, sandboxID, "/tmp")
	if err == nil {
		for _, f := range tmpFiles {
			if !f.IsDir && isOutputFile(f.Name) {
				newFiles = append(newFiles, fileWithPath{info: f, path: "/tmp/" + f.Name})
			}
		}
	}

	if len(newFiles) == 0 {
		return nil, nil
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		outputs []OutputFile
		errChan = make(chan error, len(newFiles))
		sem     = make(chan struct{}, s.workers)
	)
	for _, file := range newFiles {
		wg.Add(1)
		go func(f fileWithPath) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			output, err := s.syncFileFromSandbox(ctx, sandboxID, f.path, userID, chatID, f.info)
			if err != nil {
				errChan <- fmt.Errorf("syncing %s: %w", f.info.Name, err)
				return
			}

			mu.Lock()
			outputs = append(outputs, *output)
			mu.Unlock()
		}(file)
	}

	wg.Wait()
	close(errChan)

	var errs []string
	for err := range errChan {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return outputs, fmt.Errorf("sync errors: %s", strings.Join(errs, "; "))
	}

	return outputs, nil
}

// syncFileFromSandbox syncs a file from a sandbox to S3.
func (s *Syncer) syncFileFromSandbox(ctx context.Context, sandboxID, sandboxPath, userID, chatID string, file daytona.FileInfo) (*OutputFile, error) {
	reader, err := s.daytona.DownloadFile(ctx, sandboxID, sandboxPath)
	if err != nil {
		return nil, fmt.Errorf("downloading from sandbox: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	relativePath := strings.TrimPrefix(sandboxPath, SandboxWorkDir+"/")
	relativePath = strings.TrimPrefix(relativePath, "/tmp/")

	s3Key := userID + "/" + chatID + "/" + relativePath
	if err := s.s3.Upload(ctx, s3Key, bytes.NewReader(data), int64(len(data))); err != nil {
		return nil, fmt.Errorf("uploading to S3: %w", err)
	}

	return &OutputFile{
		Path:      relativePath,
		MediaType: s3.DetectMediaType(file.Name),
		Size:      file.Size,
	}, nil
}

// isOutputFile checks if a file should be synced as output.
func isOutputFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	outputExtensions := map[string]bool{
		// Images
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
		".svg": true, ".bmp": true, ".tiff": true, ".ico": true,
		// Video
		".mp4": true, ".webm": true, ".avi": true, ".mov": true, ".mkv": true,
		// Audio
		".mp3": true, ".wav": true, ".ogg": true, ".flac": true, ".aac": true, ".m4a": true,
		// Documents
		".pdf": true, ".csv": true, ".json": true, ".xml": true, ".txt": true,
		".html": true, ".md": true, ".xlsx": true, ".docx": true, ".pptx": true,
	}
	return outputExtensions[ext]
}
