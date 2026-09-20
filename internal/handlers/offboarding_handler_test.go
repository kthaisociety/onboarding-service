package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/config"
	"onboarding-service/internal/mattermost"
	"onboarding-service/internal/models"
	"onboarding-service/internal/offboarding"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newTestOffboardingDB is a fresh in-memory SQLite database per test — same
// convention as onboarding_flow_test.go's newTestServer, so this doesn't
// need a real Postgres instance.
func newTestOffboardingDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.OnboardingRecord{}))
	return db
}

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

	// Fake backend server that always succeeds the one endpoint this
	// handler's offboarding.Service calls — real Luma removal behavior is
	// covered by internal/offboarding's own tests; this one stays focused
	// on the HTTP layer (secret gating, validation).
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backendServer.Close)

	cfg := &config.Config{
		OnboardingServiceSecret: "test-secret",
		MattermostURL:           mmServer.URL,
		MattermostBotToken:      "test-bot-token",
		BackendURL:              backendServer.URL,
	}
	mm := mattermost.New(cfg)
	backend := backendclient.New(cfg)
	svc := offboarding.NewService(noopDeprovisioner{}, mm, backend)
	db := newTestOffboardingDB(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/")
	NewOffboardingHandler(db, cfg, svc).Register(api)

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

	// Regression for the stale-record gap Sam hit in the running app: a
	// member who completed onboarding and was later permanently deleted
	// kept showing "Complete" in the admin records list forever, with no
	// action available on it (retry/restart/cancel all only apply to
	// non-complete records).
	t.Run("delete marks the matching onboarding record as offboarded", func(t *testing.T) {
		require.NoError(t, db.Create(&models.OnboardingRecord{
			FirstName:     "Grace",
			LastName:      "Hopper",
			PersonalEmail: "grace@example.com",
			AssignedTeam:  "Development",
			State:         models.StateComplete,
			KthaisEmail:   "grace@kthais.com",
		}).Error)

		rec := post("/internal/offboarding/delete", "test-secret", map[string]any{"email": "grace@kthais.com"})
		require.Equal(t, http.StatusOK, rec.Code)

		var record models.OnboardingRecord
		require.NoError(t, db.Where("kthais_email = ?", "grace@kthais.com").First(&record).Error)
		require.Equal(t, models.StateOffboarded, record.State)
	})

	t.Run("delete leaves an already-cancelled record alone", func(t *testing.T) {
		require.NoError(t, db.Create(&models.OnboardingRecord{
			FirstName:     "Ada",
			LastName:      "Lovelace",
			PersonalEmail: "ada@example.com",
			AssignedTeam:  "Research",
			State:         models.StateCancelled,
			KthaisEmail:   "ada@kthais.com",
		}).Error)

		rec := post("/internal/offboarding/delete", "test-secret", map[string]any{"email": "ada@kthais.com"})
		require.Equal(t, http.StatusOK, rec.Code)

		var record models.OnboardingRecord
		require.NoError(t, db.Where("kthais_email = ?", "ada@kthais.com").First(&record).Error)
		require.Equal(t, models.StateCancelled, record.State, "an already-cancelled record shouldn't be relabeled offboarded")
	})

	t.Run("delete with no matching onboarding record is still a success", func(t *testing.T) {
		rec := post("/internal/offboarding/delete", "test-secret", map[string]any{"email": "no-record@kthais.com"})
		require.Equal(t, http.StatusOK, rec.Code)
	})
}
