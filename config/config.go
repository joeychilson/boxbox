package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all configuration for the application.
type Config struct {
	DatabaseURL            string
	DaytonaAPIKey          string
	DaytonaAPIURL          string
	DaytonaImage           string
	S3Bucket               string
	S3Region               string
	S3AccessKeyID          string
	S3SecretAccessKey      string
	S3Endpoint             string
	PoolMinWarm            int
	PoolMaxWarm            int
	PoolScaleDownThreshold int
	PoolSandboxTTL         time.Duration
	APIPort                string
	APIKey                 string
	ExecutionTimeout       time.Duration
	MaxFileSizeMB          int
	SyncConcurrency        int
	QueueMaxConcurrent     int
	QueueWaitTimeout       time.Duration
	SandboxCPU              int
	SandboxMemory           int
	SandboxDisk             int
	SandboxAutoStopInterval int
}

// Load loads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:            getEnv("DATABASE_URL", ""),
		DaytonaAPIKey:          getEnv("DAYTONA_API_KEY", ""),
		DaytonaAPIURL:          getEnv("DAYTONA_API_URL", "https://app.daytona.io/api"),
		DaytonaImage:           getEnv("DAYTONA_IMAGE", ""),
		S3Bucket:               getEnv("S3_BUCKET", ""),
		S3Region:               getEnv("S3_REGION", "us-east-1"),
		S3AccessKeyID:          getEnv("S3_ACCESS_KEY_ID", ""),
		S3SecretAccessKey:      getEnv("S3_SECRET_ACCESS_KEY", ""),
		S3Endpoint:             getEnv("S3_ENDPOINT", ""),
		PoolMinWarm:            getEnvInt("POOL_MIN_WARM", 3),
		PoolMaxWarm:            getEnvInt("POOL_MAX_WARM", 5),
		PoolScaleDownThreshold: getEnvInt("POOL_SCALE_DOWN_THRESHOLD", 5),
		PoolSandboxTTL:         getEnvDuration("POOL_SANDBOX_TTL", 60*time.Minute),
		APIPort:                getEnv("API_PORT", "8080"),
		APIKey:                 getEnv("API_KEY", ""),
		ExecutionTimeout:       getEnvDuration("EXECUTION_TIMEOUT", 5*time.Minute),
		MaxFileSizeMB:          getEnvInt("MAX_FILE_SIZE_MB", 100),
		SyncConcurrency:        getEnvInt("SYNC_CONCURRENCY", 5),
		QueueMaxConcurrent:     getEnvInt("QUEUE_MAX_CONCURRENT", 10),
		QueueWaitTimeout:       getEnvDuration("QUEUE_WAIT_TIMEOUT", 30*time.Second),
		SandboxCPU:              getEnvInt("SANDBOX_CPU", 1),
		SandboxMemory:           getEnvInt("SANDBOX_MEMORY", 1),
		SandboxDisk:             getEnvInt("SANDBOX_DISK", 1),
		SandboxAutoStopInterval: getEnvInt("SANDBOX_AUTO_STOP_INTERVAL", 1),
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.DaytonaAPIKey == "" {
		return nil, fmt.Errorf("DAYTONA_API_KEY is required")
	}
	if cfg.S3Bucket == "" {
		return nil, fmt.Errorf("S3_BUCKET is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("API_KEY is required")
	}

	return cfg, nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}
