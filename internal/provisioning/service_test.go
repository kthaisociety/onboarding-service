package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/config"
	"onboarding-service/internal/googleworkspace"
	"onboarding-service/internal/mattermost"
	"onboarding-service/internal/models"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeGoogle is an in-memory googleworkspace.Provisioner used only by this
// package's tests — no network, no real Google credential involved.
type fakeGoogle struct {
	mu           sync.Mutex
	users        map[string]bool
	createCalls  int
	resetCalls   int
	groupMembers map[string][]string
	failCreate   bool
}

func newFakeGoogle() *fakeGoogle {
	return &fakeGoogle{users: map[string]bool{}, groupMembers: map[string][]string{}}
}

func (f *fakeGoogle) UserExists(ctx context.Context, primaryEmail string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[primaryEmail], nil
}

func (f *fakeGoogle) CreateUser(ctx context.Context, in googleworkspace.NewUser) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate {
		return fmt.Errorf("simulated create-user failure")
	}
	f.createCalls++
	f.users[in.PrimaryEmail] = true
	return nil
}

func (f *fakeGoogle) ResetPassword(ctx context.Context, primaryEmail, tempPassword string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls++
	return nil
}

func (f *fakeGoogle) AddToGroup(ctx context.Context, groupEmail, memberEmail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupMembers[groupEmail] = append(f.groupMembers[groupEmail], memberEmail)
	return nil
}

// testHarness wires a Service against fakeGoogle plus httptest.Server fakes
// for Mattermost and the backend, following the same fake-at-the-HTTP-layer
// pattern already used for backendclient in the handlers package tests.
type testHarness struct {
	service           *Service
	google            *fakeGoogle
	failMattermost    bool
	invitedEmails     []string
	sentEmails        []map[string]string
	recordedAccounts  []map[string]string
	failRecordAccount bool
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	h := &testHarness{google: newFakeGoogle()}

	mmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.failMattermost {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body []string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		h.invitedEmails = append(h.invitedEmails, body...)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mmServer.Close)

	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/onboarding/send-email":
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			h.sentEmails = append(h.sentEmails, body)
			w.WriteHeader(http.StatusOK)
		case "/internal/onboarding/record-account":
			if h.failRecordAccount {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			h.recordedAccounts = append(h.recordedAccounts, body)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backendServer.Close)

	cfg := &config.Config{
		OnboardingServiceSecret: "test-secret",
		BackendURL:              backendServer.URL,
		MattermostURL:           mmServer.URL,
		MattermostBotToken:      "test-bot-token",
		MattermostTeamID:        "team-123",
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	// ":memory:" gives every pooled connection its own separate database —
	// pinned to one connection so every query (including sendFinalEmails'
	// own settings lookup) sees the same AutoMigrate. See the same fix in
	// internal/handlers/onboarding_flow_test.go for the full explanation.
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&models.OnboardingRecord{}, &models.OnboardingToken{}, &models.OnboardingEmailSettings{}))

	backend := backendclient.New(cfg)
	mm := mattermost.New(cfg)
	h.service = NewService(db, h.google, mm, backend)
	return h
}

func strPtr(s string) *string { return &s }

func newTestRecord(db *gorm.DB, t *testing.T) *models.OnboardingRecord {
	t.Helper()
	record := &models.OnboardingRecord{
		ApplicationID: strPtr("app-1"),
		FirstName:     "Grace",
		LastName:      "Hopper",
		PersonalEmail: "grace@example.com",
		AssignedTeam:  "Development",
		State:         models.StateKthEmailConfirmed,
		KthEmail:      "grace@kth.se",
	}
	require.NoError(t, db.Create(record).Error)
	return record
}

// newTestManualRecord has a nil ApplicationID, matching what an admin's
// manual onboarding action produces — no backend GeneralApplication exists
// for it.
func newTestManualRecord(db *gorm.DB, t *testing.T) *models.OnboardingRecord {
	t.Helper()
	record := &models.OnboardingRecord{
		ApplicationID: nil,
		FirstName:     "Ada",
		LastName:      "Lovelace",
		PersonalEmail: "ada@example.com",
		AssignedTeam:  "IT",
		State:         models.StateKthEmailConfirmed,
		KthEmail:      "ada@kth.se",
	}
	require.NoError(t, db.Create(record).Error)
	return record
}

func TestProvisionManualOnboardingSkipsRecordAccount(t *testing.T) {
	h := newTestHarness(t)
	record := newTestManualRecord(h.service.db, t)

	err := h.service.Provision(context.Background(), record)
	require.NoError(t, err)

	require.Equal(t, models.StateComplete, record.State)
	require.Equal(t, "ada.lovelace@kthais.com", record.KthaisEmail)
	require.Empty(t, h.recordedAccounts, "no backend application exists for a manual onboarding — record-account must never be called")
}

func TestProvisionHappyPath(t *testing.T) {
	h := newTestHarness(t)
	record := newTestRecord(h.service.db, t)

	err := h.service.Provision(context.Background(), record)
	require.NoError(t, err)

	require.Equal(t, models.StateComplete, record.State)
	require.Equal(t, "grace.hopper@kthais.com", record.KthaisEmail)
	require.Equal(t, 1, h.google.createCalls)
	require.Equal(t, []string{"grace.hopper@kthais.com"}, h.google.groupMembers["development@kthais.com"])
	require.Equal(t, []string{"grace.hopper@kthais.com"}, h.invitedEmails, "the Mattermost invite must go to the new @kthais.com address, not the personal kth.se one")
	require.Len(t, h.sentEmails, 2)
	require.Len(t, h.recordedAccounts, 1)
	require.Equal(t, "grace.hopper@kthais.com", h.recordedAccounts[0]["kthais_email"])
}

func TestProvisionUnknownTeamFailsWithoutExternalCalls(t *testing.T) {
	h := newTestHarness(t)
	record := newTestRecord(h.service.db, t)
	record.AssignedTeam = "Marketing"
	require.NoError(t, h.service.db.Save(record).Error)

	err := h.service.Provision(context.Background(), record)
	require.Error(t, err)

	require.Equal(t, models.StateFailed, record.State)
	require.NotEmpty(t, record.FailureReason)
	require.Equal(t, 0, h.google.createCalls)
	require.Empty(t, h.invitedEmails)
}

func TestProvisionGoogleFailureStopsBeforeMattermost(t *testing.T) {
	h := newTestHarness(t)
	h.google.failCreate = true
	record := newTestRecord(h.service.db, t)

	err := h.service.Provision(context.Background(), record)
	require.Error(t, err)

	require.Equal(t, models.StateFailed, record.State)
	require.Empty(t, record.KthaisEmail)
	require.Empty(t, h.invitedEmails, "mattermost should never be called if the google step failed")
}

// TestProvisionMattermostFailureDoesNotBlockCompletion covers the
// deliberate design in Provision: the Mattermost invite call is
// best-effort, not fatal, because its own email delivery has proven
// unreliable in practice and the getting-started email no longer depends on
// it succeeding (it always links straight to the Mattermost server — see
// emailcontent.DefaultMattermostIntro). A Mattermost hiccup must not throw
// away the real, useful Google provisioning work already done.
func TestProvisionMattermostFailureDoesNotBlockCompletion(t *testing.T) {
	h := newTestHarness(t)
	h.failMattermost = true
	record := newTestRecord(h.service.db, t)

	err := h.service.Provision(context.Background(), record)
	require.NoError(t, err)

	require.Equal(t, models.StateComplete, record.State)
	require.NotEmpty(t, record.KthaisEmail, "the google account should have been created before mattermost failed")
	require.Equal(t, 1, h.google.createCalls)
	require.Empty(t, h.invitedEmails, "the failed invite call never registered on the fake mattermost server")
	require.Len(t, h.sentEmails, 2, "final emails must still send even when the mattermost invite fails")
}

func TestProvisionRetryAfterGoogleFailureReachesComplete(t *testing.T) {
	h := newTestHarness(t)
	h.google.failCreate = true
	record := newTestRecord(h.service.db, t)

	err := h.service.Provision(context.Background(), record)
	require.Error(t, err)
	require.Equal(t, models.StateFailed, record.State)
	require.Empty(t, record.KthaisEmail)

	// Simulate an operator fixing whatever made Google account creation
	// fail, then retrying — same idempotent Provision() call, same record.
	h.google.failCreate = false
	err = h.service.Provision(context.Background(), record)
	require.NoError(t, err)

	require.Equal(t, models.StateComplete, record.State)
	require.Equal(t, 1, h.google.createCalls)
	require.Equal(t, []string{"grace.hopper@kthais.com"}, h.invitedEmails)
}

// TestProvisionRetryAfterAccountDeletedExternallyRecreatesIt covers a real
// production case: an admin restarted a record (which deliberately preserves
// KthaisEmail — see RecordActionsHandler.Restart) after also manually
// deleting the Google account itself (e.g. to reset a test user). The
// account no longer existing must not be treated as fatal — it must be
// recreated at the same address, not error out with no recovery path.
func TestProvisionRetryAfterAccountDeletedExternallyRecreatesIt(t *testing.T) {
	h := newTestHarness(t)
	record := newTestRecord(h.service.db, t)

	err := h.service.Provision(context.Background(), record)
	require.NoError(t, err)
	require.Equal(t, models.StateComplete, record.State)
	require.Equal(t, "grace.hopper@kthais.com", record.KthaisEmail)
	require.Equal(t, 1, h.google.createCalls)

	// Simulate an admin deleting the account directly in Workspace, then
	// restarting the record (which resets state but keeps KthaisEmail —
	// see models.OnboardingRecord and RecordActionsHandler.Restart).
	delete(h.google.users, "grace.hopper@kthais.com")
	record.State = models.StateKthEmailConfirmed
	record.FailureReason = ""
	require.NoError(t, h.service.db.Save(record).Error)

	err = h.service.Provision(context.Background(), record)
	require.NoError(t, err)

	require.Equal(t, models.StateComplete, record.State)
	require.Equal(t, "grace.hopper@kthais.com", record.KthaisEmail, "must recreate at the same reserved address, not mint a numbered duplicate")
	require.Equal(t, 2, h.google.createCalls, "the missing account must be recreated, not treated as a fatal error")
}
