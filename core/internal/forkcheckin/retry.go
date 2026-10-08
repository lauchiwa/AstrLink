package forkcheckin

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitError carries only a parsed deadline, never the site's header text.
// Deadlines are persisted with completion, before any retry is considered.
type RateLimitError struct{ NotBefore time.Time }

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

func RateLimited(retryAfter string, now time.Time) error {
	value := strings.TrimSpace(retryAfter)
	var deadline time.Time
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		// A very long delay must suppress today's retry, not overflow into an
		// immediate one. The daily scheduler never carries retries to old days.
		if seconds > 7*24*60*60 {
			seconds = 7 * 24 * 60 * 60
		}
		deadline = now.Add(time.Duration(seconds) * time.Second)
	} else if date, err := http.ParseTime(value); err == nil && date.After(now) {
		deadline = date
	}
	return &RateLimitError{NotBefore: deadline}
}
