package offboarding

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"onboarding-service/internal/config"
	"onboarding-service/internal/mattermost"

	"github.com/stretchr/testify/require"
)

// fakeDeprovisioner is an in-memory googleworkspace.Deprovisioner used only
// by this package's tests — no network, no real Google credential.
type fakeDeprovisioner struct {
	suspended   []string
	deleted     []string
	failSuspend bool
	failDelete  bool
}

func (f *fakeDeprovisioner) SuspendUser(ctx context.Context, primaryEmail string) error {
	if f.failSuspend {
		return errors.New("simulated suspend failure")
	}
	f.suspended = append(f.suspended, primaryEmail)
	return nil
}

func (f *fakeDeprovisioner) DeleteUser(ctx context.Context, primaryEmail string) error {
	if f.failDelete {
		return errors.New("simulated delete failure")
	}
	f.deleted = append(f.deleted, primaryEmail)
	return nil
}

// fakeMattermostServer stands up a real mattermost.Client against an
// httptest.Server, matching the pattern used in internal/mattermost's own
// tests — this package uses *mattermost.Client concretely (same as
// internal/provisioning does), so a real HTTP fake is simplest.
type fakeMattermostServer struct {
	failLookup  bool
	failDelete  bool
	deleteCalls int
}

func (f *fakeMattermostServer) client(t *testing.T) *mattermost.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			if f.failLookup {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"mm-user-1"}`))
		case r.Method == http.MethodDelete:
			f.deleteCalls++
			if f.failDelete {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"id":"api.context.permissions.app_error","message":"You do not have the appropriate permissions."}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)
	return mattermost.New(&config.Config{MattermostURL: server.URL, MattermostBotToken: "test-bot-token"})
}

func TestDeactivateHappyPath(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{}
	svc := NewService(google, mm.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.NoError(t, err)
	require.Equal(t, []string{"grace@kthais.com"}, google.suspended)
	require.Equal(t, 1, mm.deleteCalls)
}

func TestDeactivatePartialFailureReportsBoth(t *testing.T) {
	google := &fakeDeprovisioner{failSuspend: true}
	mm := &fakeMattermostServer{failDelete: true}
	svc := NewService(google, mm.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "google:")
	require.Contains(t, err.Error(), "mattermost:")
}

func TestDeactivateNoMattermostAccountIsNotAFailure(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{failLookup: true}
	svc := NewService(google, mm.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.NoError(t, err, "no Mattermost account to deactivate is success, not an error")
	require.Equal(t, []string{"grace@kthais.com"}, google.suspended, "the google side must still run")
	require.Equal(t, 0, mm.deleteCalls, "nothing to delete after a 404 lookup")
}

func TestDeleteHappyPath(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{}
	svc := NewService(google, mm.client(t))

	err := svc.Delete(context.Background(), "grace@kthais.com")
	require.NoError(t, err)
	require.Equal(t, []string{"grace@kthais.com"}, google.deleted)
}

func TestDeleteGoogleSucceedsEvenIfMattermostLacksPermission(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{failDelete: true}
	svc := NewService(google, mm.client(t))

	err := svc.Delete(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "mattermost:")
	require.Contains(t, err.Error(), "appropriate permissions")
	require.Equal(t, []string{"grace@kthais.com"}, google.deleted, "the google deletion must still have gone through")
}

func TestDeleteGoogleFailureDoesNotSkipMattermost(t *testing.T) {
	google := &fakeDeprovisioner{failDelete: true}
	mm := &fakeMattermostServer{}
	svc := NewService(google, mm.client(t))

	err := svc.Delete(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "google:")
	require.Equal(t, 1, mm.deleteCalls, "mattermost deletion must still be attempted even if google failed")
}
