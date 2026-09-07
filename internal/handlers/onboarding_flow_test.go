package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/config"
	"onboarding-service/internal/googleworkspace"
	"onboarding-service/internal/mattermost"
	"onboarding-service/internal/models"
	"onboarding-service/internal/provisioning"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeGoogle is a minimal in-memory googleworkspace.Provisioner for the
// full HTTP-flow tests in this file — no network, no real credential. See
// internal/provisioning's own tests for more thorough provisioning-logic
// coverage; this one only needs to prove the flow reaches state=complete.
type fakeGoogle struct {
	users map[string]bool
}

func newFakeGoogle() *fakeGoogle { return &fakeGoogle{users: map[string]bool{}} }

func (f *fakeGoogle) UserExists(ctx context.Context, primaryEmail string) (bool, error) {
	return f.users[primaryEmail], nil
}
func (f *fakeGoogle) CreateUser(ctx context.Context, in googleworkspace.NewUser) error {
	f.users[in.PrimaryEmail] = true
	return nil
}
func (f *fakeGoogle) ResetPassword(ctx context.Context, primaryEmail, tempPassword string) error {
	return nil
}
func (f *fakeGoogle) AddToGroup(ctx context.Context, groupEmail, memberEmail string) error {
	return nil
}

// waitFor/tick bound require.Eventually calls that wait on the
// fire-and-forget email goroutines each handler kicks off after its main
// transaction commits.
const (
	waitFor = 2 * time.Second
	tick    = 10 * time.Millisecond
)

// fakeBackend stands in for landingpage-backend's
// /internal/onboarding/send-email and /internal/onboarding/record-account
// endpoints so this test never needs real SES/Postgres — it just records
// what was sent/recorded so tests can assert on it without caring about
// actual delivery.
type fakeBackend struct {
	sentEmails       []map[string]string
	recordedAccounts []map[string]string
	mattermostFail   bool
}

func newTestServer(t *testing.T) (*gin.Engine, *config.Config, *fakeBackend) {
	t.Helper()

	fake := &fakeBackend{}
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/onboarding/send-email":
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			fake.sentEmails = append(fake.sentEmails, body)
			w.WriteHeader(http.StatusOK)
		case "/internal/onboarding/record-account":
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			fake.recordedAccounts = append(fake.recordedAccounts, body)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backendServer.Close)

	mmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fake.mattermostFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mmServer.Close)

	cfg := &config.Config{
		OnboardingServiceSecret: "test-onboarding-service-secret",
		BackendURL:              backendServer.URL,
		PortalBaseURL:           "https://kthais.com/onboarding",
		MattermostURL:           mmServer.URL,
		MattermostBotToken:      "test-bot-token",
		MattermostTeamID:        "team-123",
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.OnboardingRecord{}, &models.OnboardingToken{}))

	backend := backendclient.New(cfg)
	mm := mattermost.New(cfg)
	provisioningService := provisioning.NewService(db, newFakeGoogle(), mm, backend)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/")
	NewNotifyHandler(db, cfg, backend).Register(api)
	NewPortalHandler(db, cfg, backend, provisioningService).Register(api)
	NewRetryHandler(db, cfg, provisioningService).Register(api)

	return engine, cfg, fake
}

func doJSON(t *testing.T, engine *gin.Engine, method, path string, body map[string]string, secret string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Service-Secret", secret)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestNotify(t *testing.T) {
	engine, cfg, fake := newTestServer(t)

	notifyBody := map[string]string{
		"application_id": "app-123",
		"first_name":     "Ada",
		"last_name":      "Lovelace",
		"personal_email": "ada@example.com",
		"assigned_team":  "IT",
	}

	t.Run("missing secret is rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/notify", notifyBody, "")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("wrong secret is rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/notify", notifyBody, "wrong-secret")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("creates a record and sends the start-portal email", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/notify", notifyBody, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)

		var record models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &record))
		require.NotNil(t, record.ApplicationID)
		require.Equal(t, "app-123", *record.ApplicationID)
		require.Equal(t, models.StateNotified, record.State)

		require.Eventually(t, func() bool { return len(fake.sentEmails) == 1 }, waitFor, tick)
		require.Equal(t, "ada@example.com", fake.sentEmails[0]["to"])
		require.Contains(t, fake.sentEmails[0]["button_url"], cfg.PortalBaseURL+"/start?token=")
	})

	t.Run("a second notify for the same application is idempotent", func(t *testing.T) {
		emailsBefore := len(fake.sentEmails)
		rec := doJSON(t, engine, "POST", "/notify", notifyBody, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, emailsBefore, len(fake.sentEmails), "should not send a second start-portal email")
	})

	t.Run("missing fields are rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/notify", map[string]string{"application_id": "app-456"}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("a manual onboarding (no application_id) is accepted and never deduplicated", func(t *testing.T) {
		manualBody := map[string]string{
			"first_name":     "Board",
			"last_name":      "Member",
			"personal_email": "board@example.com",
			"assigned_team":  "IT",
		}

		rec := doJSON(t, engine, "POST", "/notify", manualBody, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)
		var first models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &first))
		require.Nil(t, first.ApplicationID)

		// A second identical manual submission must create a SECOND record,
		// not return the first one — there's no application_id to
		// deduplicate on, unlike the backend-triggered path above.
		rec2 := doJSON(t, engine, "POST", "/notify", manualBody, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec2.Code)
		var second models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &second))
		require.NotEqual(t, first.ID, second.ID)
	})
}

func TestPortalFlow(t *testing.T) {
	engine, cfg, fake := newTestServer(t)

	notifyRec := doJSON(t, engine, "POST", "/notify", map[string]string{
		"application_id": "app-flow",
		"first_name":     "Grace",
		"last_name":      "Hopper",
		"personal_email": "grace@example.com",
		"assigned_team":  "Development",
	}, cfg.OnboardingServiceSecret)
	require.Equal(t, http.StatusOK, notifyRec.Code)

	require.Eventually(t, func() bool { return len(fake.sentEmails) == 1 }, waitFor, tick)
	startToken := extractToken(t, fake.sentEmails[0]["button_url"])

	t.Run("wrong or missing token is rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
			"token": "not-a-real-token", "kth_email": "grace@kth.se",
		}, "")
		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("non-kth.se address is rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
			"token": startToken, "kth_email": "grace@gmail.com",
		}, "")
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("valid token and kth.se address advances the record and sends a confirm email", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
			"token": startToken, "kth_email": "GRACE@KTH.SE",
		}, "")
		require.Equal(t, http.StatusOK, rec.Code)

		var record models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &record))
		require.Equal(t, models.StateKthEmailSubmitted, record.State)
		require.Equal(t, "grace@kth.se", record.KthEmail)

		require.Eventually(t, func() bool { return len(fake.sentEmails) == 2 }, waitFor, tick)
		require.Equal(t, "grace@kth.se", fake.sentEmails[1]["to"])
	})

	t.Run("the start-portal token cannot be reused", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
			"token": startToken, "kth_email": "grace@kth.se",
		}, "")
		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	confirmToken := extractToken(t, fake.sentEmails[1]["button_url"])

	t.Run("confirming with the wrong token is rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": "bogus"}, "")
		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("confirming runs provisioning through to complete", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": confirmToken}, "")
		require.Equal(t, http.StatusOK, rec.Code)

		var record models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &record))
		require.Equal(t, models.StateComplete, record.State)
		require.Equal(t, "grace.hopper@kthais.com", record.KthaisEmail)

		require.Len(t, fake.sentEmails, 4, "start-portal, confirm, account-info, mattermost getting-started")
		require.Len(t, fake.recordedAccounts, 1)
		require.Equal(t, "grace.hopper@kthais.com", fake.recordedAccounts[0]["kthais_email"])
	})

	t.Run("the confirm token cannot be reused", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": confirmToken}, "")
		require.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func extractToken(t *testing.T, url string) string {
	t.Helper()
	idx := strings.Index(url, "token=")
	require.GreaterOrEqual(t, idx, 0, "url should contain a token param: %s", url)
	return url[idx+len("token="):]
}

func TestRetryProvisioning(t *testing.T) {
	engine, cfg, fake := newTestServer(t)

	notifyRec := doJSON(t, engine, "POST", "/notify", map[string]string{
		"application_id": "app-retry",
		"first_name":     "Ada",
		"last_name":      "Lovelace",
		"personal_email": "ada@example.com",
		"assigned_team":  "IT",
	}, cfg.OnboardingServiceSecret)
	require.Equal(t, http.StatusOK, notifyRec.Code)
	require.Eventually(t, func() bool { return len(fake.sentEmails) == 1 }, waitFor, tick)
	startToken := extractToken(t, fake.sentEmails[0]["button_url"])

	doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
		"token": startToken, "kth_email": "ada@kth.se",
	}, "")
	require.Eventually(t, func() bool { return len(fake.sentEmails) == 2 }, waitFor, tick)
	confirmToken := extractToken(t, fake.sentEmails[1]["button_url"])

	fake.mattermostFail = true
	rec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": confirmToken}, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var record models.OnboardingRecord
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &record))
	require.Equal(t, models.StateFailed, record.State)
	require.NotEmpty(t, record.FailureReason)

	t.Run("retry with the wrong secret is rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/internal/onboarding/retry-provisioning",
			map[string]string{"application_id": "app-retry"}, "wrong-secret")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("retry succeeds once the underlying problem is fixed", func(t *testing.T) {
		fake.mattermostFail = false
		rec := doJSON(t, engine, "POST", "/internal/onboarding/retry-provisioning",
			map[string]string{"application_id": "app-retry"}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)

		var retried models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &retried))
		require.Equal(t, models.StateComplete, retried.State)
		require.Equal(t, "ada.lovelace@kthais.com", retried.KthaisEmail)
	})

	t.Run("retrying an application with no onboarding record 404s", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/internal/onboarding/retry-provisioning",
			map[string]string{"application_id": "does-not-exist"}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusNotFound, rec.Code)
	})
}
