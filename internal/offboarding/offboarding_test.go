package offboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"onboarding-service/internal/backendclient"
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
// internal/provisioning does), so a real HTTP fake is simplest. Tracks
// deactivated state (like a real Mattermost server's delete_at) rather
// than just counting calls, so a test can drive the real
// mattermost.Client.DeactivateUser through its own "already deactivated"
// no-op path — see TestDeactivateRetryAfterPartialFailureSucceeds.
type fakeMattermostServer struct {
	failLookup  bool
	failDelete  bool
	deleteCalls int
	deactivated bool
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
			deleteAt := 0
			if f.deactivated {
				deleteAt = 1700000000000
			}
			_, _ = fmt.Fprintf(w, `{"id":"mm-user-1","delete_at":%d}`, deleteAt)
		case r.Method == http.MethodDelete:
			f.deleteCalls++
			if f.failDelete {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"id":"api.context.permissions.app_error","message":"You do not have the appropriate permissions."}`))
				return
			}
			f.deactivated = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)
	return mattermost.New(&config.Config{MattermostURL: server.URL, MattermostBotToken: "test-bot-token"})
}

// fakeBackendServer stands up a real backendclient.Client against an
// httptest.Server, matching the pattern used for fakeMattermostServer —
// this package uses *backendclient.Client concretely, so a real HTTP fake
// is simplest. Only implements /internal/onboarding/remove-from-luma,
// the one endpoint this package calls.
type fakeBackendServer struct {
	failRemoveFromLuma bool
	// lumaRouteNotDeployed simulates the companion backend route not
	// existing yet during a staged rollout: unlike failRemoveFromLuma
	// (502, a real failure), the handler never even matches the path, so
	// the fake never records the call, matching a genuinely-missing route.
	lumaRouteNotDeployed bool
	removeFromLumaCalls  []string
}

func (f *fakeBackendServer) client(t *testing.T) *backendclient.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/onboarding/remove-from-luma" || f.lumaRouteNotDeployed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.removeFromLumaCalls = append(f.removeFromLumaCalls, body["email"])
		if f.failRemoveFromLuma {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return backendclient.New(&config.Config{BackendURL: server.URL, OnboardingServiceSecret: "test-secret"})
}

func TestDeactivateHappyPath(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{}
	backend := &fakeBackendServer{}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.NoError(t, err)
	require.Equal(t, []string{"grace@kthais.com"}, google.suspended)
	require.Equal(t, 1, mm.deleteCalls)
	require.Equal(t, []string{"grace@kthais.com"}, backend.removeFromLumaCalls, "luma removal must also be attempted")
}

func TestDeactivatePartialFailureReportsBoth(t *testing.T) {
	google := &fakeDeprovisioner{failSuspend: true}
	mm := &fakeMattermostServer{failDelete: true}
	backend := &fakeBackendServer{}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "google:")
	require.Contains(t, err.Error(), "mattermost:")
}

func TestDeactivateNoMattermostAccountIsNotAFailure(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{failLookup: true}
	backend := &fakeBackendServer{}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.NoError(t, err, "no Mattermost account to deactivate is success, not an error")
	require.Equal(t, []string{"grace@kthais.com"}, google.suspended, "the google side must still run")
	require.Equal(t, 0, mm.deleteCalls, "nothing to delete after a 404 lookup")
}

// TestDeactivateGoogleFailureStillChecksMattermostAndLuma pins down the
// exact scenario Deactivate must never regress to: a Google failure (e.g.
// a 403 from a revoked/expired credential) must not short-circuit the
// Mattermost and Luma steps. Unlike TestDeactivateLumaFailureJoinsWithout
// SuppressingOthers, this only fails Google, so it can also assert the
// Mattermost/Luma steps actually *succeeded* (removeFromLumaCalls
// populated), not just that they were attempted.
func TestDeactivateGoogleFailureStillChecksMattermostAndLuma(t *testing.T) {
	google := &fakeDeprovisioner{failSuspend: true}
	mm := &fakeMattermostServer{}
	backend := &fakeBackendServer{}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "google:")
	require.Equal(t, 1, mm.deleteCalls, "a google failure must not skip the mattermost step")
	require.Equal(t, []string{"grace@kthais.com"}, backend.removeFromLumaCalls, "a google failure must not skip the luma step")
}

// TestDeactivateRetryAfterPartialFailureSucceeds proves Deactivate is safe
// to call again after a partial failure: retrying with the previously
// failing step now fixed must reach a clean success, not re-report the
// steps that already succeeded on the first attempt as new failures. Then
// retries a third time against the now-fully-deactivated account, and
// checks deleteCalls stops climbing — proving the retry reaches success
// because mattermost.Client.DeactivateUser's own already-deactivated
// no-op kicked in (see fakeMattermostServer's delete_at tracking), not
// just because the fakes happen to always succeed when told to. The
// Google leg can't be driven through this same real-idempotency path:
// fakeDeprovisioner is a hand-written Deprovisioner stub, not the real
// googleworkspace.Client, which is where SuspendUser's own already-
// suspended check actually lives (see its doc comment) — there's no
// httptest-server-backed fake for the Admin SDK client anywhere in this
// codebase to exercise that through.
func TestDeactivateRetryAfterPartialFailureSucceeds(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{failDelete: true}
	backend := &fakeBackendServer{}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "mattermost:")
	require.Equal(t, 1, mm.deleteCalls, "the failed DELETE attempt still counts as a call")

	mm.failDelete = false
	err = svc.Deactivate(context.Background(), "grace@kthais.com")
	require.NoError(t, err, "retrying after the failing step is fixed must succeed cleanly")
	require.Equal(t, 2, mm.deleteCalls, "this retry's DELETE actually went through and deactivated the account")

	err = svc.Deactivate(context.Background(), "grace@kthais.com")
	require.NoError(t, err, "retrying an already-fully-deactivated account must still succeed")
	require.Equal(t, 2, mm.deleteCalls, "mattermost's own already-deactivated check must have no-op'd this DELETE")
}

// TestDeactivateLumaFailureJoinsWithoutSuppressingOthers covers the same
// "attempt every step, join every error" contract Deactivate already had
// for Google/Mattermost, now extended to the third Luma step: a Luma
// failure alongside a Google failure must surface both, not just one.
func TestDeactivateLumaFailureJoinsWithoutSuppressingOthers(t *testing.T) {
	google := &fakeDeprovisioner{failSuspend: true}
	mm := &fakeMattermostServer{}
	backend := &fakeBackendServer{failRemoveFromLuma: true}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "google:")
	require.Contains(t, err.Error(), "luma:")
	require.Equal(t, 1, mm.deleteCalls, "mattermost must still be attempted even though google and luma both failed")
}

// TestDeactivateLumaRouteNotDeployedIsNotAFailure covers the staged-rollout
// case: the companion backend route may not be deployed yet when this
// service is, and that must not turn an otherwise-successful Google/
// Mattermost deactivation into a reported failure. Mirrors
// TestDeactivateNoMattermostAccountIsNotAFailure's "specific condition is a
// benign no-op" shape, one layer over in backendclient.
func TestDeactivateLumaRouteNotDeployedIsNotAFailure(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{}
	backend := &fakeBackendServer{lumaRouteNotDeployed: true}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Deactivate(context.Background(), "grace@kthais.com")
	require.NoError(t, err, "a 404 from a not-yet-deployed Luma route is a rollout no-op, not a failure")
	require.Equal(t, []string{"grace@kthais.com"}, google.suspended, "the google side must still run")
	require.Equal(t, 1, mm.deleteCalls, "the mattermost side must still run")
}

func TestDeleteHappyPath(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{}
	backend := &fakeBackendServer{}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Delete(context.Background(), "grace@kthais.com")
	require.NoError(t, err)
	require.Equal(t, []string{"grace@kthais.com"}, google.deleted)
	require.Equal(t, []string{"grace@kthais.com"}, backend.removeFromLumaCalls, "luma removal must also be attempted")
}

func TestDeleteGoogleSucceedsEvenIfMattermostLacksPermission(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{failDelete: true}
	backend := &fakeBackendServer{}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Delete(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "mattermost:")
	require.Contains(t, err.Error(), "appropriate permissions")
	require.Equal(t, []string{"grace@kthais.com"}, google.deleted, "the google deletion must still have gone through")
}

func TestDeleteGoogleFailureDoesNotSkipMattermost(t *testing.T) {
	google := &fakeDeprovisioner{failDelete: true}
	mm := &fakeMattermostServer{}
	backend := &fakeBackendServer{}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Delete(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "google:")
	require.Equal(t, 1, mm.deleteCalls, "mattermost deletion must still be attempted even if google failed")
}

// TestDeleteLumaFailureJoinsWithoutSuppressingOthers mirrors
// TestDeactivateLumaFailureJoinsWithoutSuppressingOthers for Delete.
func TestDeleteLumaFailureJoinsWithoutSuppressingOthers(t *testing.T) {
	google := &fakeDeprovisioner{failDelete: true}
	mm := &fakeMattermostServer{}
	backend := &fakeBackendServer{failRemoveFromLuma: true}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Delete(context.Background(), "grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "google:")
	require.Contains(t, err.Error(), "luma:")
	require.Equal(t, 1, mm.deleteCalls, "mattermost must still be attempted even though google and luma both failed")
}

// TestDeleteLumaRouteNotDeployedIsNotAFailure mirrors
// TestDeactivateLumaRouteNotDeployedIsNotAFailure for Delete — this is also
// the case that matters for OffboardingHandler.Delete's own record
// bookkeeping (see offboarding_handler_test.go): Delete must return nil
// here so the caller still marks the onboarding record offboarded.
func TestDeleteLumaRouteNotDeployedIsNotAFailure(t *testing.T) {
	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{}
	backend := &fakeBackendServer{lumaRouteNotDeployed: true}
	svc := NewService(google, mm.client(t), backend.client(t))

	err := svc.Delete(context.Background(), "grace@kthais.com")
	require.NoError(t, err, "a 404 from a not-yet-deployed Luma route is a rollout no-op, not a failure")
	require.Equal(t, []string{"grace@kthais.com"}, google.deleted, "the google side must still run")
	require.Equal(t, 1, mm.deleteCalls, "the mattermost side must still run")
}

// TestDeactivateLumaRequestStopsOnContextCancellation proves ctx is
// actually threaded through to the Luma HTTP call (not just accepted and
// ignored): a canceled context must abort the request quickly rather than
// running it to completion against a slow backend.
func TestDeactivateLumaRequestStopsOnContextCancellation(t *testing.T) {
	// t.Cleanup runs in LIFO order, so server.Close (registered second, run
	// first) must never be left waiting on a handler that's still blocked
	// on this channel — register close(block) last so it runs first.
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never responds until the test cleans up
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(block) })
	backend := backendclient.New(&config.Config{BackendURL: server.URL, OnboardingServiceSecret: "test-secret"})

	google := &fakeDeprovisioner{}
	mm := &fakeMattermostServer{}
	svc := NewService(google, mm.client(t), backend)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Deactivate(ctx, "grace@kthais.com") }()

	cancel()

	select {
	case err := <-done:
		require.Error(t, err, "a canceled Luma request must surface as an error, not hang or silently succeed")
		require.Contains(t, err.Error(), "luma:")
	case <-time.After(2 * time.Second):
		t.Fatal("Deactivate did not return promptly after its context was canceled — ctx is not actually threaded through to the Luma request")
	}
}
