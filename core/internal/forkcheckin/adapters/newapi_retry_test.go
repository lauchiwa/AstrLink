package adapters

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

func TestNewAPIReadPreservesRetryAfterWithoutSiteText(t *testing.T) {
	adapter, snapshot := readFixture(t, NewAPILegacy, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("private-site-prose"))
	})
	before := time.Now().Add(2 * time.Minute)
	_, err := adapter.ValidateIdentity(context.Background(), snapshot)
	after := time.Now().Add(2 * time.Minute)
	var limited *forkcheckin.RateLimitError
	if !errors.Is(err, forkcheckin.ErrRateLimited) || !errors.As(err, &limited) || limited.NotBefore.Before(before) || limited.NotBefore.After(after) {
		t.Fatalf("Retry-After lost: %v", err)
	}
	if err.Error() != forkcheckin.ErrRateLimited.Error() {
		t.Fatal("site text escaped")
	}
}
