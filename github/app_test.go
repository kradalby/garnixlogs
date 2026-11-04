package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	testSHA = "3090e8b2cb364fe70b4a5d0db00dd082ec5f1649"
	instTok = "inst-token"
)

func testKey(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// fakeGitHub serves the four calls a rerun makes and records the rerequests.
// App-level calls must carry the App's JWT and repo-level calls the
// installation token, or a mixed-up credential would pass unnoticed.
func fakeGitHub(t *testing.T, suites []int64) (*httptest.Server, *[]string) {
	t.Helper()

	var (
		mu          sync.Mutex
		rerequested []string
	)

	appAuth := func(w http.ResponseWriter, r *http.Request) bool {
		jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || strings.Count(jwt, ".") != 2 {
			http.Error(w, "want app jwt", http.StatusUnauthorized)

			return false
		}

		return true
	}

	tokAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "token "+instTok {
			http.Error(w, "want installation token", http.StatusUnauthorized)

			return false
		}

		return true
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /repos/kradalby/dotfiles/installation", func(w http.ResponseWriter, r *http.Request) {
		if appAuth(w, r) {
			json.NewEncoder(w).Encode(map[string]any{"id": 7})
		}
	})

	mux.HandleFunc("POST /app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !appAuth(w, r) {
			return
		}

		var opts struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		json.NewDecoder(r.Body).Decode(&opts)

		if len(opts.Repositories) != 1 || opts.Repositories[0] != "dotfiles" ||
			len(opts.Permissions) != 1 || opts.Permissions["checks"] != "write" {
			http.Error(w, "token must be scoped to the repo and checks", http.StatusBadRequest)

			return
		}

		json.NewEncoder(w).Encode(map[string]any{"token": instTok})
	})

	mux.HandleFunc("GET /repos/kradalby/dotfiles/commits/"+testSHA+"/check-suites",
		func(w http.ResponseWriter, r *http.Request) {
			if !tokAuth(w, r) {
				return
			}

			if r.URL.Query().Get("app_id") != "42" {
				http.Error(w, "suites must be filtered to the app", http.StatusBadRequest)

				return
			}

			list := []map[string]any{}
			for _, id := range suites {
				list = append(list, map[string]any{"id": id})
			}

			json.NewEncoder(w).Encode(map[string]any{"check_suites": list})
		})

	mux.HandleFunc("POST /repos/kradalby/dotfiles/check-suites/{id}/rerequest",
		func(w http.ResponseWriter, r *http.Request) {
			if !tokAuth(w, r) {
				return
			}

			mu.Lock()
			rerequested = append(rerequested, r.PathValue("id"))
			mu.Unlock()

			w.WriteHeader(http.StatusCreated)
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv, &rerequested
}

func TestRerun(t *testing.T) {
	t.Parallel()

	keyPEM := testKey(t)

	tests := []struct {
		name    string
		suites  []int64
		want    []string
		wantErr error
	}{
		{name: "one suite", suites: []int64{11}, want: []string{"11"}},
		{name: "every suite of the app", suites: []int64{11, 12}, want: []string{"11", "12"}},
		{name: "nothing to re-run", wantErr: ErrNoSuite},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gh, got := fakeGitHub(t, tt.suites)

			app, err := New("42\n", keyPEM, WithAPIURL(gh.URL))
			require.NoError(t, err)

			n, err := app.Rerun(context.Background(), "kradalby", "dotfiles", testSHA)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, len(tt.want), n)
			require.Equal(t, tt.want, *got)
		})
	}
}

// TestRerunNotInstalled keeps GitHub's own refusal in the error, since that
// text is what tells a caller the App is not on the repository.
func TestRerunNotInstalled(t *testing.T) {
	t.Parallel()

	gh, _ := fakeGitHub(t, nil)

	app, err := New("42", testKey(t), WithAPIURL(gh.URL))
	require.NoError(t, err)

	_, err = app.Rerun(context.Background(), "someone", "else", testSHA)
	require.ErrorContains(t, err, "find installation")
	require.ErrorContains(t, err, "404")
}

func TestNew(t *testing.T) {
	t.Parallel()

	key := testKey(t)

	_, err := New("42", key)
	require.NoError(t, err)

	_, err = New("", key)
	require.Error(t, err)

	_, err = New("42", []byte("not a key"))
	require.Error(t, err)
}
