// Package mattermost invites new members to the KTHAIS Mattermost team.
// Hand-rolled net/http rather than the official Mattermost SDK: this
// service only ever needs two REST calls, which doesn't justify pulling in
// the full server-model dependency tree — and a plain net/http client stays
// fakeable with httptest.Server, matching the pattern already used for
// backendclient (see internal/backendclient/client.go).
package mattermost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"onboarding-service/internal/config"
)

// mattermostErrorBody is the shape of Mattermost's own JSON error responses
// (e.g. {"id":"...","message":"...","status_code":400}) — read on any
// non-2xx response so the real reason (not just the HTTP status) ends up
// in FailureReason and this package's own logs, instead of just "returned
// 400" with no way to tell why.
type mattermostErrorBody struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// describeError reads and summarizes a non-2xx Mattermost response body.
// Bounded to 4KB — Mattermost's own error bodies are always small JSON, so
// anything larger is almost certainly not one and not worth logging in
// full.
func describeError(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || len(body) == 0 {
		return ""
	}
	var parsed mattermostErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		if parsed.ID != "" {
			return fmt.Sprintf(" (%s: %s)", parsed.ID, parsed.Message)
		}
		return fmt.Sprintf(" (%s)", parsed.Message)
	}
	return fmt.Sprintf(" (%s)", string(body))
}

type Client struct {
	baseURL    string
	botToken   string
	teamID     string
	httpClient *http.Client
}

func New(cfg *config.Config) *Client {
	return &Client{
		baseURL:    cfg.MattermostURL,
		botToken:   cfg.MattermostBotToken,
		teamID:     cfg.MattermostTeamID,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// InviteToTeam invites email to the configured team via Mattermost's
// standard email-invite mechanism. This works regardless of the instance's
// eventual sign-in method (password or SSO) — the invite just gets the
// address onto the team's allowed list and sends a join link.
//
// FLAG (per onboarding-service-plan.md, unverified as of this writing):
// confirm this is actually how Sam's Mattermost instance is configured, and
// that the bot account has team-level invite permission, before relying on
// this in production — see the plan doc's Mattermost section.
func (c *Client) InviteToTeam(email string) error {
	if c.teamID == "" {
		return fmt.Errorf("MATTERMOST_TEAM_ID is not configured")
	}

	payload, err := json.Marshal([]string{email})
	if err != nil {
		return fmt.Errorf("failed to marshal invite request: %w", err)
	}

	path := fmt.Sprintf("/api/v4/teams/%s/invite/email", c.teamID)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build invite request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("invite request for %s failed: %w", email, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("mattermost returned %d inviting %s%s", resp.StatusCode, email, describeError(resp))
	}
	return nil
}

// Ping is a one-shot, non-fatal reachability check logged at boot only —
// same idiom as cmd/api/main.go's checkBackendConnectivity for the backend.
func (c *Client) Ping() error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/api/v4/users/me", nil)
	if err != nil {
		return fmt.Errorf("failed to build ping request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach mattermost at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("mattermost ping returned %d%s", resp.StatusCode, describeError(resp))
	}
	return nil
}
