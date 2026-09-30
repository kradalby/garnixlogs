package garnix

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()

	c, err := New(url, "u", "tok", WithLogger(slog.New(slog.DiscardHandler)))
	require.NoError(t, err)

	return c
}

// stallingMint serves a JWT endpoint that hangs until the test ends,
// signalling on minting each time a mint starts.
func stallingMint(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()

	minting := make(chan struct{}, 8)
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/jwt" {
			minting <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}

		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	return srv, minting
}

// A caller with no deadline stuck in a mint must not hold others past theirs.
func TestStalledMintHonoursOtherDeadlines(t *testing.T) {
	t.Parallel()

	srv, minting := stallingMint(t)
	c := newTestClient(t, srv.URL)

	go c.Build(context.Background(), "a")
	<-minting

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.Build(ctx, "b")
		done <- err
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		t.Fatal("caller blocked past its deadline by another caller's mint")
	}
}

// A mint must give up on its own even when the caller never will, or it holds
// the slot forever.
func TestStalledMintTimesOut(t *testing.T) {
	t.Parallel()

	srv, _ := stallingMint(t)
	c := newTestClient(t, srv.URL)
	c.mintTimeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := c.Build(context.Background(), "a")
		done <- err
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		t.Fatal("mint with no caller deadline never gave up")
	}
}

// Concurrent requests rejected with the same JWT must share one re-mint.
func TestRejectedJWTRemintedOnce(t *testing.T) {
	t.Parallel()

	const n = 10

	var (
		mints    atomic.Int64
		rejected atomic.Int64
		allIn    = make(chan struct{})
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/jwt" {
			fmt.Fprintf(w, `{"token":"t%d","expiresAt":"2099-01-01T00:00:00Z"}`, mints.Add(1))

			return
		}

		if r.Header.Get("Authorization") == "Bearer t1" {
			// Hold every first attempt until all have used t1, so each
			// caller sees its own 401 before anyone re-mints.
			if rejected.Add(1) == n {
				close(allIn)
			}
			select {
			case <-allIn:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		fmt.Fprint(w, `{"id":"x"}`)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)

	// Bounded so a caller that never reaches the barrier fails the test
	// instead of hanging it.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = c.Build(ctx, "b")
		})
	}
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}

	// One to start, one to replace the rejected token.
	require.Equal(t, int64(2), mints.Load())
}
