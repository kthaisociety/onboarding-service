package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onboarding-service/internal/config"
	"onboarding-service/internal/mattermost"
	"onboarding-service/internal/offboarding"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// noopDeprovisioner always succeeds — this test suite only needs to prove
// the HTTP layer (secret gating, email validation, status codes), not
// re-exercise the underlying Google/Mattermost calls (see
// internal/offboarding's own tests for that).
type noopDeprovisioner struct{}

func (noopDeprovisioner) SuspendUser(ctx context.Context, primaryEmail string) error { return nil }
func (noopDeprovisioner) DeleteUser(ctx context.Context, primaryEmail string) error  { return nil }

func TestOffboardingHandler(t *testing.T) {
	// Real mattermost.Client against a fake server that always reports "no
	// such user" — keeps this handler test focused on request validation,
	// secret gating, and response shape.
	mmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(mmServer.Close)

	cfg := &config.Config{
		OnboardingServiceSecret: "test-secret",
		MattermostURL:           mmServer.URL,
		MattermostBotToken:      "test-bot-token",
	}
	mm := mattermost.New(cfg)
	svc := offboarding.NewService(noopDeprovisioner{}, mm)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/")
	NewOffboardingHandler(cfg, svc).Register(api)

	post := func(path, secret string, body map[string]any) *httptest.ResponseRecorder {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		if secret != "" {
			req.Header.Set("X-Service-Secret", secret)
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	for _, path := range []string{"/internal/offboarding/deactivate", "/internal/offboarding/delete"} {
		t.Run(path, func(t *testing.T) {
			t.Run("missing secret is rejected", func(t *testing.T) {
				rec := post(path, "", map[string]any{"email": "grace@kthais.com"})
				require.Equal(t, http.StatusUnauthorized, rec.Code)
			})

			t.Run("wrong secret is rejected", func(t *testing.T) {
				rec := post(path, "wrong", map[string]any{"email": "grace@kthais.com"})
				require.Equal(t, http.StatusUnauthorized, rec.Code)
			})

			t.Run("a non-kthais.com email is rejected", func(t *testing.T) {
				rec := post(path, "test-secret", map[string]any{"email": "grace@kth.se"})
				require.Equal(t, http.StatusBadRequest, rec.Code)
			})

			t.Run("missing email is rejected", func(t *testing.T) {
				rec := post(path, "test-secret", map[string]any{})
				require.Equal(t, http.StatusBadRequest, rec.Code)
			})

			t.Run("a valid kthais.com email succeeds", func(t *testing.T) {
				rec := post(path, "test-secret", map[string]any{"email": "grace@kthais.com"})
				require.Equal(t, http.StatusOK, rec.Code)
			})
		})
	}
}
