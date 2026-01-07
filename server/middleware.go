package server

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

type contextKey string

const (
	contextKeyUserID    contextKey = "user_id"
	contextKeyChatID    contextKey = "chat_id"
	contextKeyRequestID contextKey = "request_id"
)

// GetUserID extracts the user ID from the request context.
func GetUserID(ctx context.Context) string {
	if v := ctx.Value(contextKeyUserID); v != nil {
		return v.(string)
	}
	return ""
}

// GetChatID extracts the chat ID from the request context.
func GetChatID(ctx context.Context) string {
	if v := ctx.Value(contextKeyChatID); v != nil {
		return v.(string)
	}
	return ""
}

// GetRequestID extracts the request ID from the request context.
func GetRequestID(ctx context.Context) string {
	if v := ctx.Value(contextKeyRequestID); v != nil {
		return v.(string)
	}
	return ""
}

// RequestID adds a unique request ID to each request.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}
		ctx := context.WithValue(r.Context(), contextKeyRequestID, requestID)
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// APIKeyAuth validates the API key and extracts user/chat IDs.
func APIKeyAuth(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}

			if subtle.ConstantTimeCompare([]byte(key), []byte(apiKey)) != 1 {
				http.Error(w, `{"error": "unauthorized", "message": "invalid or missing API key"}`, http.StatusUnauthorized)
				return
			}

			userID := r.Header.Get("X-User-ID")
			if userID == "" {
				http.Error(w, `{"error": "bad_request", "message": "X-User-ID header is required"}`, http.StatusBadRequest)
				return
			}

			chatID := r.Header.Get("X-Chat-ID")
			if chatID == "" {
				http.Error(w, `{"error": "bad_request", "message": "X-Chat-ID header is required"}`, http.StatusBadRequest)
				return
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, contextKeyUserID, userID)
			ctx = context.WithValue(ctx, contextKeyChatID, chatID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Logger logs HTTP requests.
func Logger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status code
			wrapped := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(wrapped, r)

			duration := time.Since(start)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", wrapped.status,
				"duration_ms", duration.Milliseconds(),
				"request_id", GetRequestID(r.Context()),
				"user_id", GetUserID(r.Context()),
			)
		})
	}
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Recoverer recovers from panics and logs them.
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						"error", rec,
						"stack", string(debug.Stack()),
						"request_id", GetRequestID(r.Context()),
					)
					http.Error(w, `{"error": "internal_error", "message": "internal server error"}`, http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimiter provides per-user rate limiting with automatic cleanup.
type RateLimiter struct {
	limiters sync.Map
	rate     rate.Limit
	burst    int
}

// NewRateLimiter creates a new rate limiter.
func NewRateLimiter(requestsPerSecond float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		rate:  rate.Limit(requestsPerSecond),
		burst: burst,
	}
	go rl.cleanup()
	return rl
}

// cleanup removes stale rate limiters every 10 minutes.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(10 * time.Minute)
	for range ticker.C {
		rl.limiters.Range(func(key, value any) bool {
			l := value.(*rate.Limiter)
			// If limiter has full tokens, user hasn't been active - remove it
			if l.Tokens() >= float64(rl.burst) {
				rl.limiters.Delete(key)
			}
			return true
		})
	}
}

// RateLimit returns middleware that limits requests per user.
func (rl *RateLimiter) RateLimit() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := GetUserID(r.Context())
			if userID == "" {
				next.ServeHTTP(w, r)
				return
			}

			limiter, _ := rl.limiters.LoadOrStore(userID, rate.NewLimiter(rl.rate, rl.burst))
			l := limiter.(*rate.Limiter)

			if !l.Allow() {
				w.Header().Set("Retry-After", "1")
				http.Error(w, `{"error": "rate_limit_exceeded", "message": "too many requests"}`, http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// CORS adds CORS headers.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, X-User-ID, X-Chat-ID, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
