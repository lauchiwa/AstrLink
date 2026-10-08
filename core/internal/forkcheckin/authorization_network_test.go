package forkcheckin

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthorizationVerifierBoundsWorkAndCancelsWaiters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}, maxAuthorizations+1), make(chan struct{})
	var builds atomic.Int32
	verify := NewAuthorizationVerifier(func(*TransportFactory) (SiteAdapter, error) {
		builds.Add(1)
		return &authorizationNetworkAdapter{entered: entered, release: release}, nil
	})
	var workers sync.WaitGroup
	results := make(chan error, maxAuthorizations)
	for range maxAuthorizations {
		workers.Go(func() {
			_, err := verify(ctx, AccountSnapshot{})
			results <- err
		})
	}
	for range maxAuthorizations {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("verification slots were not filled")
		}
	}
	waiting, stopWaiting := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stopWaiting()
	if _, err := verify(waiting, AccountSnapshot{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting verification: %v", err)
	}
	if got := builds.Load(); got != maxAuthorizations {
		t.Fatalf("constructed %d clients with %d occupied slots", got, maxAuthorizations)
	}
	close(release)
	workers.Wait()
	for range maxAuthorizations {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := verify(ctx, AccountSnapshot{}); err != nil {
		t.Fatalf("released slot was not reusable: %v", err)
	}
}

type authorizationNetworkAdapter struct {
	SiteAdapter
	entered chan<- struct{}
	release <-chan struct{}
}

func (a *authorizationNetworkAdapter) ValidateIdentity(ctx context.Context, _ AccountSnapshot) (SiteIdentity, error) {
	a.entered <- struct{}{}
	select {
	case <-a.release:
		return SiteIdentity{RemoteUserID: "7"}, nil
	case <-ctx.Done():
		return SiteIdentity{}, ctx.Err()
	}
}

func TestAuthorizationVerifierClosesPrivateFactoryOnEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "build error", "nil adapter", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var private *TransportFactory
			verify := NewAuthorizationVerifier(func(factory *TransportFactory) (SiteAdapter, error) {
				private = factory
				switch outcome {
				case "build error":
					return nil, errors.New("private builder details")
				case "nil adapter":
					return nil, nil
				case "cancelled":
					cancel()
					return &authorizationNetworkAdapter{entered: make(chan struct{}, 1), release: make(chan struct{})}, nil
				default:
					released := make(chan struct{})
					close(released)
					return &authorizationNetworkAdapter{entered: make(chan struct{}, 1), release: released}, nil
				}
			})
			_, err := verify(ctx, AccountSnapshot{})
			switch outcome {
			case "success":
				if err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled verification: %v", err)
				}
			default:
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("builder details were not sanitized: %v", err)
				}
			}
			if private == nil {
				t.Fatal("factory was not constructed")
			}
			private.mu.Lock()
			closed := private.closed
			private.mu.Unlock()
			if !closed {
				t.Fatal("private factory remains open")
			}
		})
	}
}
