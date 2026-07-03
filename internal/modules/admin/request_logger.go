package admin

import (
	"sync"
	"time"
)

// RequestRecord represents a single API request record
type RequestRecord struct {
	ID              string    `json:"id"`
	Timestamp       time.Time `json:"timestamp"`
	AccountID       string    `json:"account_id"`
	Model           string    `json:"model"`
	Stream          bool      `json:"stream"`
	Status          string    `json:"status"` // "success", "error", "pending"
	ErrorMessage    string    `json:"error_message,omitempty"`
	Duration        int64     `json:"duration_ms"`        // request duration in milliseconds
	FirstByteLatency int64  `json:"first_byte_ms"`      // time to first byte in milliseconds
	TokensInput     int       `json:"tokens_input,omitempty"`
	TokensOutput    int       `json:"tokens_output,omitempty"`
	UserAgent       string    `json:"user_agent,omitempty"`
	RequestPath     string    `json:"request_path"`
}

// RequestLogger stores recent API requests in a ring buffer
type RequestLogger struct {
	mu       sync.RWMutex
	records  []RequestRecord
	capacity int
	head     int
	size     int
}

// NewRequestLogger creates a new request logger with specified capacity
func NewRequestLogger(capacity int) *RequestLogger {
	if capacity <= 0 {
		capacity = 1000
	}
	return &RequestLogger{
		records:  make([]RequestRecord, capacity),
		capacity: capacity,
		head:     0,
		size:     0,
	}
}

// LogRequest adds a new request record
func (rl *RequestLogger) LogRequest(record RequestRecord) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now()
	}

	rl.records[rl.head] = record
	rl.head = (rl.head + 1) % rl.capacity
	if rl.size < rl.capacity {
		rl.size++
	}
}

// GetRecent returns the most recent n records (newest first)
func (rl *RequestLogger) GetRecent(n int) []RequestRecord {
	rl.mu.RLock()
	defer rl.mu.RUnlock()

	if n <= 0 || n > rl.size {
		n = rl.size
	}

	result := make([]RequestRecord, n)
	for i := 0; i < n; i++ {
		idx := (rl.head - 1 - i + rl.capacity) % rl.capacity
		result[i] = rl.records[idx]
	}
	return result
}

// GetAll returns all records (newest first)
func (rl *RequestLogger) GetAll() []RequestRecord {
	return rl.GetRecent(rl.size)
}

// GetStats returns statistics for the logged requests
func (rl *RequestLogger) GetStats() RequestStats {
	rl.mu.RLock()
	defer rl.mu.RUnlock()

	stats := RequestStats{
		Total:     rl.size,
		ByAccount: make(map[string]int),
		ByModel:   make(map[string]int),
		ByStatus:  make(map[string]int),
	}

	for i := 0; i < rl.size; i++ {
		idx := (rl.head - 1 - i + rl.capacity) % rl.capacity
		record := rl.records[idx]

		stats.ByAccount[record.AccountID]++
		stats.ByModel[record.Model]++
		stats.ByStatus[record.Status]++

		if record.Status == "success" {
			stats.SuccessCount++
		} else if record.Status == "error" {
			stats.ErrorCount++
		}
	}

	return stats
}

// RequestStats holds aggregated statistics
type RequestStats struct {
	Total        int            `json:"total"`
	SuccessCount int            `json:"success_count"`
	ErrorCount   int            `json:"error_count"`
	ByAccount    map[string]int `json:"by_account"`
	ByModel      map[string]int `json:"by_model"`
	ByStatus     map[string]int `json:"by_status"`
}

// Global logger instance
var globalLogger = NewRequestLogger(1000)

// GetGlobalLogger returns the global request logger instance
func GetGlobalLogger() *RequestLogger {
	return globalLogger
}

// SetGlobalLogger sets the global request logger (useful for testing)
func SetGlobalLogger(logger *RequestLogger) {
	globalLogger = logger
}
