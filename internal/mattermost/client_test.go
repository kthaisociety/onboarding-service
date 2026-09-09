package mattermost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"onboarding-service/internal/config"

	"github.com/stretchr/testify/require"
)

func TestInviteToTeam(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := New(&config.Config{
		MattermostURL:      server.URL,
		MattermostBotToken: "test-bot-token",
		MattermostTeamID:   "team-123",
	})

	err := client.InviteToTeam("grace@kth.se")
	require.NoError(t, err)
	require.Equal(t, "/api/v4/teams/team-123/invite/email", gotPath)
	require.Equal(t, "Bearer test-bot-token", gotAuth)
	require.Equal(t, []string{"grace@kth.se"}, gotBody)
}

func TestInviteToTeamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	client := New(&config.Config{
		MattermostURL:      server.URL,
		MattermostBotToken: "test-bot-token",
		MattermostTeamID:   "team-123",
	})

	err := client.InviteToTeam("grace@kth.se")
	require.Error(t, err)
}

func TestInviteToTeamFailureIncludesMattermostsOwnErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"id":"api.team.invite_members.invalid_domain","message":"Invalid domain for invitation, valid domains are: kthais.com","status_code":400}`))
	}))
	t.Cleanup(server.Close)

	client := New(&config.Config{
		MattermostURL:      server.URL,
		MattermostBotToken: "test-bot-token",
		MattermostTeamID:   "team-123",
	})

	err := client.InviteToTeam("grace@kth.se")
	require.Error(t, err)
	require.Contains(t, err.Error(), "Invalid domain for invitation")
	require.Contains(t, err.Error(), "api.team.invite_members.invalid_domain")
}

func TestInviteToTeamMissingTeamID(t *testing.T) {
	client := New(&config.Config{MattermostURL: "http://example.invalid", MattermostBotToken: "t"})
	err := client.InviteToTeam("grace@kth.se")
	require.Error(t, err)
}

func TestDeactivateUser(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/email/grace@kthais.com":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"user-123"}`))
		case r.Method == http.MethodDelete:
			gotMethod, gotPath = r.Method, r.URL.String()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client := New(&config.Config{MattermostURL: server.URL, MattermostBotToken: "test-bot-token"})
	err := client.DeactivateUser("grace@kthais.com")
	require.NoError(t, err)
	require.Equal(t, "Bearer test-bot-token", gotAuth)
	require.Equal(t, http.MethodDelete, gotMethod)
	require.Equal(t, "/api/v4/users/user-123", gotPath, "deactivate must not pass permanent=true")
}

func TestDeactivateUserNoSuchUserIsANoOp(t *testing.T) {
	var deleteCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client := New(&config.Config{MattermostURL: server.URL, MattermostBotToken: "test-bot-token"})
	err := client.DeactivateUser("nobody@kthais.com")
	require.NoError(t, err)
	require.False(t, deleteCalled, "there's nothing to delete if the lookup found no user")
}

func TestDeleteUserPermanently(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"user-123"}`))
		case r.Method == http.MethodDelete:
			gotPath = r.URL.String()
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)

	client := New(&config.Config{MattermostURL: server.URL, MattermostBotToken: "test-bot-token"})
	err := client.DeleteUserPermanently("grace@kthais.com")
	require.NoError(t, err)
	require.Equal(t, "/api/v4/users/user-123?permanent=true", gotPath)
}

func TestDeleteUserPermanentlyIncludesMattermostsOwnErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"user-123"}`))
		case r.Method == http.MethodDelete:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"id":"api.context.permissions.app_error","message":"You do not have the appropriate permissions."}`))
		}
	}))
	t.Cleanup(server.Close)

	client := New(&config.Config{MattermostURL: server.URL, MattermostBotToken: "test-bot-token"})
	err := client.DeleteUserPermanently("grace@kthais.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "You do not have the appropriate permissions.")
}

func TestPing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v4/users/me", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := New(&config.Config{MattermostURL: server.URL, MattermostBotToken: "t"})
	require.NoError(t, client.Ping())
}
