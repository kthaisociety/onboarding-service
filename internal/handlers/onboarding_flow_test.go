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
	"onboarding-service/internal/emailcontent"
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
	sendEmailFail    bool
}

func newTestServer(t *testing.T) (*gin.Engine, *config.Config, *fakeBackend) {
	t.Helper()

	fake := &fakeBackend{}
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/onboarding/send-email":
			if fake.sendEmailFail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			fake.sentEmails = append(fake.sentEmails, body)
			w.WriteHeader(http.StatusOK)
		case "/internal/onboarding/record-account":
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			fake.recordedAccounts = append(fake.recordedAccounts, body)
			w.WriteHeader(http.StatusOK)
		case "/internal/onboarding/add-to-luma":
			// This flow only needs provisioning to reach state=complete, so
			// every add-member call just succeeds — see
			// internal/provisioning's own tests for real Luma-failure
			// coverage.
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backendServer.Close)

	// No fake Mattermost server: nothing in this flow calls Mattermost's API
	// anymore (the KTHAIS team is open + kthais.com-domain-restricted, so
	// there's no invite step — see provisioning.Service.Provision). The
	// mattermost.Client built from MattermostURL is only ever asked for its
	// BaseURL(), a local string, when composing the getting-started email.
	cfg := &config.Config{
		OnboardingServiceSecret: "test-onboarding-service-secret",
		BackendURL:              backendServer.URL,
		PortalBaseURL:           "https://kthais.com/onboarding",
		MattermostURL:           "https://chat.aisociety.se",
		MattermostBotToken:      "test-bot-token",
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	// ":memory:" gives every pooled connection its own separate database —
	// without pinning to one connection, a background goroutine (the
	// fire-and-forget email sends below) can land on a connection that
	// never saw AutoMigrate and see "no such table". Real deployments use a
	// file-backed DB, where this isn't a concern.
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&models.OnboardingRecord{}, &models.OnboardingToken{}, &models.OnboardingEmailSettings{}))

	backend := backendclient.New(cfg)
	mm := mattermost.New(cfg)
	provisioningService := provisioning.NewService(db, cfg, newFakeGoogle(), mm, backend)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/")
	NewNotifyHandler(db, cfg, backend).Register(api)
	NewPortalHandler(db, cfg, backend, provisioningService).Register(api)
	NewRecordActionsHandler(db, cfg, backend, provisioningService).Register(api)
	NewRecordsHandler(db, cfg).Register(api)
	NewEmailSettingsHandler(db, cfg).Register(api)

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

	// LumaKickoffURL configured up front — this test exercises the full
	// five-email flow; the contract email would otherwise be skipped as
	// "not configured yet" (see provisioning.Service.sendFinalEmails' own
	// doc comment, and provisioning.TestProvisionSkipsContractEmailWhenLumaKickoffURLUnset
	// for that case).
	settingsRec := doJSON(t, engine, "PUT", "/internal/onboarding/email-settings", map[string]string{
		"contract_url":     "https://drive.google.com/file/d/contract/view",
		"luma_kickoff_url": "https://lu.ma/kickoff",
		"updated_by_email": "admin@kthais.com",
	}, cfg.OnboardingServiceSecret)
	require.Equal(t, http.StatusOK, settingsRec.Code)

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

	t.Run("confirm info shows who the link is for, without consuming it", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/portal/confirm?token="+confirmToken, nil)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)

		var info struct {
			FirstName string `json:"first_name"`
			KthEmail  string `json:"kth_email"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
		require.Equal(t, "Grace", info.FirstName)
		require.Equal(t, "grace@kth.se", info.KthEmail)

		// The token must still be valid afterwards — a GET (including a
		// mail scanner's prefetch) must never consume it.
		badToken := httptest.NewRequest("GET", "/portal/confirm?token=bogus", nil)
		badRec := httptest.NewRecorder()
		engine.ServeHTTP(badRec, badToken)
		require.Equal(t, http.StatusNotFound, badRec.Code)
	})

	t.Run("confirming runs provisioning through to complete", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": confirmToken}, "")
		require.Equal(t, http.StatusOK, rec.Code)

		var record models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &record))
		require.Equal(t, models.StateComplete, record.State)
		require.Equal(t, "grace.hopper@kthais.com", record.KthaisEmail)

		require.Len(t, fake.sentEmails, 5, "start-portal, confirm, account-info, mattermost getting-started, contract")
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

	fake.sendEmailFail = true
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
		fake.sendEmailFail = false
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

func TestRecordsHandler(t *testing.T) {
	engine, cfg, fake := newTestServer(t)

	t.Run("missing secret is rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "GET", "/internal/onboarding/records", nil, "")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("wrong secret is rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "GET", "/internal/onboarding/records", nil, "wrong-secret")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("returns created records, newest first", func(t *testing.T) {
		doJSON(t, engine, "POST", "/notify", map[string]string{
			"application_id": "app-records-1",
			"first_name":     "Ada",
			"last_name":      "Lovelace",
			"personal_email": "ada@example.com",
			"assigned_team":  "IT",
		}, cfg.OnboardingServiceSecret)
		require.Eventually(t, func() bool { return len(fake.sentEmails) >= 1 }, waitFor, tick)

		doJSON(t, engine, "POST", "/notify", map[string]string{
			"application_id": "app-records-2",
			"first_name":     "Grace",
			"last_name":      "Hopper",
			"personal_email": "grace@example.com",
			"assigned_team":  "Development",
		}, cfg.OnboardingServiceSecret)
		require.Eventually(t, func() bool { return len(fake.sentEmails) >= 2 }, waitFor, tick)

		rec := doJSON(t, engine, "GET", "/internal/onboarding/records", nil, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)

		var records []models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &records))
		require.GreaterOrEqual(t, len(records), 2)
		require.Equal(t, "Grace", records[0].FirstName, "newest record should come first")
	})
}

func TestRetryProvisioningByRecordID(t *testing.T) {
	engine, cfg, fake := newTestServer(t)

	// A manual onboarding has no application_id at all — this is the case
	// retry-provisioning couldn't handle before this test was added.
	doJSON(t, engine, "POST", "/notify", map[string]string{
		"first_name":     "Ada",
		"last_name":      "Lovelace",
		"personal_email": "ada@example.com",
		"assigned_team":  "IT",
	}, cfg.OnboardingServiceSecret)
	require.Eventually(t, func() bool { return len(fake.sentEmails) >= 1 }, waitFor, tick)
	startToken := extractToken(t, fake.sentEmails[0]["button_url"])

	doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
		"token": startToken, "kth_email": "ada@kth.se",
	}, "")
	require.Eventually(t, func() bool { return len(fake.sentEmails) >= 2 }, waitFor, tick)
	confirmToken := extractToken(t, fake.sentEmails[1]["button_url"])

	fake.sendEmailFail = true
	confirmRec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": confirmToken}, "")
	require.Equal(t, http.StatusOK, confirmRec.Code)

	var failed models.OnboardingRecord
	require.NoError(t, json.Unmarshal(confirmRec.Body.Bytes(), &failed))
	require.Equal(t, models.StateFailed, failed.State)
	require.Nil(t, failed.ApplicationID)

	postJSON := func(body map[string]any) *httptest.ResponseRecorder {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest("POST", "/internal/onboarding/retry-provisioning", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Secret", cfg.OnboardingServiceSecret)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	t.Run("retrying by application_id fails — there isn't one", func(t *testing.T) {
		rec := postJSON(map[string]any{"application_id": ""})
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("retrying by record id succeeds once the underlying problem is fixed", func(t *testing.T) {
		fake.sendEmailFail = false
		rec := postJSON(map[string]any{"id": failed.ID})
		require.Equal(t, http.StatusOK, rec.Code)

		var retried models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &retried))
		require.Equal(t, models.StateComplete, retried.State)
		require.Equal(t, "ada.lovelace@kthais.com", retried.KthaisEmail)
	})
}

func TestCancel(t *testing.T) {
	engine, cfg, fake := newTestServer(t)

	postJSON := func(path string, body map[string]any) *httptest.ResponseRecorder {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Secret", cfg.OnboardingServiceSecret)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	doJSON(t, engine, "POST", "/notify", map[string]string{
		"first_name":     "Ada",
		"last_name":      "Lovelace",
		"personal_email": "ada@example.com",
		"assigned_team":  "IT",
	}, cfg.OnboardingServiceSecret)
	require.Eventually(t, func() bool { return len(fake.sentEmails) >= 1 }, waitFor, tick)
	startToken := extractToken(t, fake.sentEmails[0]["button_url"])

	// Fetch the record via the records endpoint rather than trusting the
	// notify response body's shape here — simpler than threading it through.
	listRec := doJSON(t, engine, "GET", "/internal/onboarding/records", nil, cfg.OnboardingServiceSecret)
	var records []models.OnboardingRecord
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &records))
	require.Len(t, records, 1)
	recordID := records[0].ID

	t.Run("cancelling an unknown record 404s", func(t *testing.T) {
		rec := postJSON("/internal/onboarding/cancel", map[string]any{"id": uint(999999)})
		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("cancel succeeds and invalidates the outstanding start-portal token", func(t *testing.T) {
		rec := postJSON("/internal/onboarding/cancel", map[string]any{"id": recordID})
		require.Equal(t, http.StatusOK, rec.Code)

		var cancelled models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cancelled))
		require.Equal(t, models.StateCancelled, cancelled.State)

		submitRec := doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
			"token": startToken, "kth_email": "ada@kth.se",
		}, "")
		require.Equal(t, http.StatusNotFound, submitRec.Code, "the old start-portal token must no longer work")
	})

	t.Run("cancelling an already-cancelled record is rejected", func(t *testing.T) {
		rec := postJSON("/internal/onboarding/cancel", map[string]any{"id": recordID})
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	// Regression for the stale-record gap Sam hit in the running app: a
	// completed onboarding whose member was later removed some other way
	// (or before OffboardingHandler.Delete existed) had no way to clear it
	// — Cancel used to reject StateComplete outright. It's now the manual
	// counterpart to Delete's automatic marking, landing on StateOffboarded
	// specifically (not StateCancelled) since the onboarding itself did
	// complete.
	t.Run("cancelling a complete record marks it offboarded instead of cancelled", func(t *testing.T) {
		doJSON(t, engine, "POST", "/notify", map[string]string{
			"first_name":     "Grace",
			"last_name":      "Hopper",
			"personal_email": "grace-cancel@example.com",
			"assigned_team":  "Development",
		}, cfg.OnboardingServiceSecret)
		require.Eventually(t, func() bool { return len(fake.sentEmails) >= 2 }, waitFor, tick)
		graceStartToken := extractToken(t, fake.sentEmails[len(fake.sentEmails)-1]["button_url"])

		doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
			"token": graceStartToken, "kth_email": "grace-cancel@kth.se",
		}, "")
		require.Eventually(t, func() bool { return len(fake.sentEmails) >= 3 }, waitFor, tick)
		graceConfirmToken := extractToken(t, fake.sentEmails[len(fake.sentEmails)-1]["button_url"])

		confirmRec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": graceConfirmToken}, "")
		var complete models.OnboardingRecord
		require.NoError(t, json.Unmarshal(confirmRec.Body.Bytes(), &complete))
		require.Equal(t, models.StateComplete, complete.State)

		rec := postJSON("/internal/onboarding/cancel", map[string]any{"id": complete.ID})
		require.Equal(t, http.StatusOK, rec.Code)

		var offboarded models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &offboarded))
		require.Equal(t, models.StateOffboarded, offboarded.State)

		t.Run("cancelling an already-offboarded record is rejected", func(t *testing.T) {
			rec := postJSON("/internal/onboarding/cancel", map[string]any{"id": complete.ID})
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})

		t.Run("restarting an offboarded record is rejected", func(t *testing.T) {
			rec := postJSON("/internal/onboarding/restart", map[string]any{"id": complete.ID})
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	})
}

func TestRestart(t *testing.T) {
	engine, cfg, fake := newTestServer(t)

	postJSON := func(path string, body map[string]any) *httptest.ResponseRecorder {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Secret", cfg.OnboardingServiceSecret)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	t.Run("restarting a complete record is rejected", func(t *testing.T) {
		doJSON(t, engine, "POST", "/notify", map[string]string{
			"first_name":     "Grace",
			"last_name":      "Hopper",
			"personal_email": "grace@example.com",
			"assigned_team":  "Development",
		}, cfg.OnboardingServiceSecret)
		require.Eventually(t, func() bool { return len(fake.sentEmails) >= 1 }, waitFor, tick)
		graceStartToken := extractToken(t, fake.sentEmails[len(fake.sentEmails)-1]["button_url"])

		doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
			"token": graceStartToken, "kth_email": "grace@kth.se",
		}, "")
		require.Eventually(t, func() bool { return len(fake.sentEmails) >= 2 }, waitFor, tick)
		graceConfirmToken := extractToken(t, fake.sentEmails[len(fake.sentEmails)-1]["button_url"])

		confirmRec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": graceConfirmToken}, "")
		var complete models.OnboardingRecord
		require.NoError(t, json.Unmarshal(confirmRec.Body.Bytes(), &complete))
		require.Equal(t, models.StateComplete, complete.State)

		rec := postJSON("/internal/onboarding/restart", map[string]any{"id": complete.ID})
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("restarting a cancelled record is rejected", func(t *testing.T) {
		doJSON(t, engine, "POST", "/notify", map[string]string{
			"first_name":     "Bob",
			"last_name":      "Noyce",
			"personal_email": "bob@example.com",
			"assigned_team":  "IT",
		}, cfg.OnboardingServiceSecret)

		listRec := doJSON(t, engine, "GET", "/internal/onboarding/records", nil, cfg.OnboardingServiceSecret)
		var records []models.OnboardingRecord
		require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &records))
		bob := records[len(records)-1]

		cancelRec := postJSON("/internal/onboarding/cancel", map[string]any{"id": bob.ID})
		require.Equal(t, http.StatusOK, cancelRec.Code)

		rec := postJSON("/internal/onboarding/restart", map[string]any{"id": bob.ID})
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	// Grace's completed flow synchronously sent two more emails beyond the
	// portal ones (account-info + Mattermost getting-started, from
	// sendFinalEmails) — baseline off the count as it now stands rather
	// than assuming an exact total.
	baseline := len(fake.sentEmails)

	doJSON(t, engine, "POST", "/notify", map[string]string{
		"first_name":     "Ada",
		"last_name":      "Lovelace",
		"personal_email": "ada@example.com",
		"assigned_team":  "IT",
	}, cfg.OnboardingServiceSecret)
	require.Eventually(t, func() bool { return len(fake.sentEmails) >= baseline+1 }, waitFor, tick)
	startToken := extractToken(t, fake.sentEmails[len(fake.sentEmails)-1]["button_url"])

	doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
		"token": startToken, "kth_email": "ada@kth.se",
	}, "")
	require.Eventually(t, func() bool { return len(fake.sentEmails) >= baseline+2 }, waitFor, tick)
	confirmToken := extractToken(t, fake.sentEmails[len(fake.sentEmails)-1]["button_url"])

	// Google succeeds (so KthaisEmail gets set) but the final-emails step
	// fails, so the record ends up StateFailed with a real KthaisEmail
	// already on it — exactly the case restart must not clobber. (There's
	// no Mattermost invite step anymore to induce this — see
	// provisioning.Service.Provision.)
	fake.sendEmailFail = true
	confirmRec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": confirmToken}, "")
	var failed models.OnboardingRecord
	require.NoError(t, json.Unmarshal(confirmRec.Body.Bytes(), &failed))
	require.Equal(t, models.StateFailed, failed.State)
	require.Equal(t, "ada.lovelace@kthais.com", failed.KthaisEmail)

	// Restart re-sends the start-onboarding email (see below) — clear the
	// fault now that it's done its job of producing a failed record, or
	// that resend fails too and the test hangs waiting for it.
	fake.sendEmailFail = false

	emailsBeforeRestart := len(fake.sentEmails)

	rec := postJSON("/internal/onboarding/restart", map[string]any{"id": failed.ID})
	require.Equal(t, http.StatusOK, rec.Code)

	var restarted models.OnboardingRecord
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &restarted))
	require.Equal(t, models.StateNotified, restarted.State)
	require.Equal(t, "ada.lovelace@kthais.com", restarted.KthaisEmail, "an already-provisioned address must survive a restart")
	require.Empty(t, restarted.FailureReason)

	require.Eventually(t, func() bool { return len(fake.sentEmails) > emailsBeforeRestart }, waitFor, tick)

	t.Run("the old confirm token no longer works after restart", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": confirmToken}, "")
		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("the new start-portal link works", func(t *testing.T) {
		newStartToken := extractToken(t, fake.sentEmails[len(fake.sentEmails)-1]["button_url"])
		rec := doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
			"token": newStartToken, "kth_email": "ada@kth.se",
		}, "")
		require.Equal(t, http.StatusOK, rec.Code)
	})
}

// TestDeleteRecord covers the admin's ability to actually remove an old
// onboarding record from the list — Cancel/Restart only ever changed
// State, never gave a way to clear a finished entry out entirely.
func TestDeleteRecord(t *testing.T) {
	engine, cfg, _ := newTestServer(t)

	postJSON := func(path string, body map[string]any) *httptest.ResponseRecorder {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Service-Secret", cfg.OnboardingServiceSecret)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}
	recordsList := func() []models.OnboardingRecord {
		rec := doJSON(t, engine, "GET", "/internal/onboarding/records", nil, cfg.OnboardingServiceSecret)
		var records []models.OnboardingRecord
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &records))
		return records
	}

	t.Run("deleting an unknown record 404s", func(t *testing.T) {
		rec := postJSON("/internal/onboarding/delete-record", map[string]any{"id": uint(999999)})
		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("deleting a record still in progress is rejected", func(t *testing.T) {
		doJSON(t, engine, "POST", "/notify", map[string]string{
			"first_name":     "Grace",
			"last_name":      "Hopper",
			"personal_email": "grace-delete@example.com",
			"assigned_team":  "Development",
		}, cfg.OnboardingServiceSecret)
		records := recordsList()
		grace := records[len(records)-1]
		require.Equal(t, models.StateNotified, grace.State)

		rec := postJSON("/internal/onboarding/delete-record", map[string]any{"id": grace.ID})
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("deleting a cancelled record removes it from the list", func(t *testing.T) {
		doJSON(t, engine, "POST", "/notify", map[string]string{
			"first_name":     "Ada",
			"last_name":      "Lovelace",
			"personal_email": "ada-delete@example.com",
			"assigned_team":  "IT",
		}, cfg.OnboardingServiceSecret)
		records := recordsList()
		ada := records[len(records)-1]

		cancelRec := postJSON("/internal/onboarding/cancel", map[string]any{"id": ada.ID})
		require.Equal(t, http.StatusOK, cancelRec.Code)

		rec := postJSON("/internal/onboarding/delete-record", map[string]any{"id": ada.ID})
		require.Equal(t, http.StatusOK, rec.Code)

		for _, r := range recordsList() {
			require.NotEqual(t, ada.ID, r.ID, "the deleted record must no longer appear in the list")
		}
	})
}

func TestEmailSettings(t *testing.T) {
	engine, cfg, fake := newTestServer(t)

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/internal/onboarding/email-settings", nil)
		req.Header.Set("X-Service-Secret", cfg.OnboardingServiceSecret)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	t.Run("missing secret is rejected", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/internal/onboarding/email-settings", nil)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("defaults to the built-in copy before anything is saved", func(t *testing.T) {
		rec := get()
		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, emailcontent.DefaultStartIntro, body["start_intro_text"])
		require.Equal(t, emailcontent.DefaultConfirmIntro, body["confirm_intro_text"])
		require.Equal(t, emailcontent.DefaultMattermostIntro, body["mattermost_intro_text"])
		require.Equal(t, emailcontent.DefaultContractIntro, body["contract_intro_text"])
		// The account email has no non-empty default — it has no intro
		// paragraph at all until an admin adds one.
		require.Equal(t, "", body["account_intro_text"])
	})

	t.Run("preview reflects an unsaved draft, never what's actually saved", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/internal/onboarding/email-settings/preview", map[string]string{
			"kind": "start", "intro_text": "Hey {{first_name}}, welcome aboard!",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, emailcontent.StartSubject, body["subject"])
		require.Contains(t, body["body"], "Hey "+emailcontent.PreviewFirstName+", welcome aboard!")

		// Never persisted by the preview call.
		getRec := get()
		var settings map[string]string
		require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &settings))
		require.Equal(t, emailcontent.DefaultStartIntro, settings["start_intro_text"])
	})

	t.Run("confirm preview", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/internal/onboarding/email-settings/preview", map[string]string{
			"kind": "confirm", "intro_text": "Almost there, {{first_name}}!",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, emailcontent.ConfirmSubject, body["subject"])
		require.Contains(t, body["body"], "Almost there, "+emailcontent.PreviewFirstName+"!")
	})

	t.Run("account preview uses sample credentials, never real data", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/internal/onboarding/email-settings/preview", map[string]string{
			"kind": "account", "intro_text": "Welcome to the team!",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, emailcontent.AccountSubject, body["subject"])
		require.Contains(t, body["body"], "Welcome to the team!")
		require.Contains(t, body["body"], "Temporary password:")
	})

	t.Run("mattermost preview", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/internal/onboarding/email-settings/preview", map[string]string{
			"kind": "mattermost", "intro_text": "Come say hi in #general!",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, emailcontent.MattermostSubject, body["subject"])
		require.Contains(t, body["body"], "Come say hi in #general!")
	})

	t.Run("contract preview reflects the request's own contract/bylaws/Luma links, not saved settings", func(t *testing.T) {
		// Deliberately save different links than the ones the preview
		// request below sends — proves the preview renders the caller's
		// live draft, not whatever's already persisted (Greptile flagged
		// the opposite behavior as a bug: an admin editing a link and
		// clicking Preview before Save must see their edit, not the stale
		// saved value).
		saveRec := doJSON(t, engine, "PUT", "/internal/onboarding/email-settings", map[string]string{
			"contract_url":     "https://drive.google.com/file/d/stale-saved-contract/view",
			"bylaws_url":       "https://kthais.com/stale-bylaws.pdf",
			"luma_kickoff_url": "https://lu.ma/stale-kickoff",
			"updated_by_email": "admin@kthais.com",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, saveRec.Code)

		rec := doJSON(t, engine, "POST", "/internal/onboarding/email-settings/preview", map[string]string{
			"kind":             "contract",
			"intro_text":       "Please read ahead of the kick-off, {{first_name}}!",
			"contract_url":     "https://drive.google.com/file/d/draft-contract/view",
			"bylaws_url":       "https://kthais.com/draft-bylaws.pdf",
			"luma_kickoff_url": "https://lu.ma/draft-kickoff",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, emailcontent.ContractSubject, body["subject"])
		require.Contains(t, body["body"], "Please read ahead of the kick-off, "+emailcontent.PreviewFirstName+"!")
		require.Contains(t, body["body"], "https://drive.google.com/file/d/draft-contract/view")
		require.Contains(t, body["body"], "https://kthais.com/draft-bylaws.pdf")
		require.NotContains(t, body["body"], "stale-contract")
		require.NotContains(t, body["body"], "stale-bylaws")
		// The button is the kick-off RSVP link, not the contract — just
		// whatever link the request sent, same as mattermost's fixed
		// button — no per-record token minted.
		require.Equal(t, "https://lu.ma/draft-kickoff", body["button_url"])
		require.NotEqual(t, "https://lu.ma/stale-kickoff", body["button_url"])
		require.Equal(t, emailcontent.KickoffRSVPButtonText, body["button_text"])
	})

	t.Run("contract preview never renders a dangling label for a whitespace-only link", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/internal/onboarding/email-settings/preview", map[string]string{
			"kind":         "contract",
			"intro_text":   "whatever",
			"contract_url": "   ",
			"bylaws_url":   "   ",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.NotContains(t, body["body"], "Contract:")
		require.NotContains(t, body["body"], "Bylaws:")
	})

	t.Run("unknown kind is rejected", func(t *testing.T) {
		rec := doJSON(t, engine, "POST", "/internal/onboarding/email-settings/preview", map[string]string{
			"kind": "bogus", "intro_text": "whatever",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("saving takes effect for the next real sends", func(t *testing.T) {
		saveRec := doJSON(t, engine, "PUT", "/internal/onboarding/email-settings", map[string]string{
			"start_intro_text":      "Hi {{first_name}}, so glad you're joining us!",
			"confirm_intro_text":    "One more step, {{first_name}}!",
			"account_intro_text":    "Welcome aboard, {{first_name}}!",
			"mattermost_intro_text": "Say hi in #general, {{first_name}}.",
			"contract_url":          "https://drive.google.com/file/d/contract-2026/view",
			"luma_kickoff_url":      "https://lu.ma/kickoff-2026",
			"updated_by_email":      "admin@kthais.com",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, saveRec.Code)
		var saved map[string]string
		require.NoError(t, json.Unmarshal(saveRec.Body.Bytes(), &saved))
		require.Equal(t, "Hi {{first_name}}, so glad you're joining us!", saved["start_intro_text"])

		emailsBefore := len(fake.sentEmails)
		rec := doJSON(t, engine, "POST", "/notify", map[string]string{
			"first_name":     "Margaret",
			"last_name":      "Hamilton",
			"personal_email": "margaret@example.com",
			"assigned_team":  "IT",
		}, cfg.OnboardingServiceSecret)
		require.Equal(t, http.StatusOK, rec.Code)

		require.Eventually(t, func() bool { return len(fake.sentEmails) > emailsBefore }, waitFor, tick)
		startEmail := fake.sentEmails[len(fake.sentEmails)-1]
		require.Contains(t, startEmail["body"], "Hi Margaret, so glad you're joining us!")
		// The fixed next-steps list is still appended after the custom intro.
		require.Contains(t, startEmail["body"], "To get started:")

		t.Run("the account and Mattermost emails pick it up too, once provisioning completes", func(t *testing.T) {
			var record models.OnboardingRecord
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &record))
			startToken := extractToken(t, startEmail["button_url"])

			submitRec := doJSON(t, engine, "POST", "/portal/submit-email", map[string]string{
				"token": startToken, "kth_email": "margaret@kth.se",
			}, "")
			require.Equal(t, http.StatusOK, submitRec.Code)
			require.Eventually(t, func() bool { return len(fake.sentEmails) > emailsBefore+1 }, waitFor, tick)
			confirmEmail := fake.sentEmails[len(fake.sentEmails)-1]
			require.Contains(t, confirmEmail["body"], "One more step, Margaret!")
			confirmToken := extractToken(t, confirmEmail["button_url"])

			confirmRec := doJSON(t, engine, "POST", "/portal/confirm", map[string]string{"token": confirmToken}, "")
			require.Equal(t, http.StatusOK, confirmRec.Code)

			accountEmail := fake.sentEmails[len(fake.sentEmails)-3]
			mattermostEmail := fake.sentEmails[len(fake.sentEmails)-2]
			contractEmail := fake.sentEmails[len(fake.sentEmails)-1]
			require.Contains(t, accountEmail["body"], "Welcome aboard, Margaret!")
			require.Contains(t, mattermostEmail["body"], "Say hi in #general, Margaret.")
			require.Equal(t, emailcontent.ContractSubject, contractEmail["subject"])
			// The button is the kick-off RSVP link, straight from settings,
			// no per-record token minted for it. The contract link itself
			// travels as a plain (linkified) URL in the body instead.
			require.Equal(t, "https://lu.ma/kickoff-2026", contractEmail["button_url"])
			require.Contains(t, contractEmail["body"], "https://drive.google.com/file/d/contract-2026/view")
		})
	})
}
