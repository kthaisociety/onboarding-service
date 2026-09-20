// Package backendclient calls landingpage-backend's
// /api/v1/internal/onboarding/* endpoints — this service has no SES/email
// setup of its own and reuses the backend's, and reports provisioning
// results back to the backend as pure bookkeeping. See
// onboarding-service-plan.md for the full design and
// landingpage-backend/internal/handlers/onboarding_handler.go for what's on
// the other end of these calls.
package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"onboarding-service/internal/config"
)

// ErrRouteNotFound wraps the error post returns when the backend responds
// 404, distinguishable via errors.Is from any other failure — lets a caller
// whose companion backend route may not be deployed yet (a staged rollout)
// choose to tolerate it, unlike a real failure.
var ErrRouteNotFound = errors.New("backend route not found")

type Client struct {
	cfg        *config.Config
	httpClient *http.Client
}

func New(cfg *config.Config) *Client {
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

type sendEmailRequest struct {
	To         string `json:"to"`
	Subject    string `json:"subject"`
	Body       string `json:"body"`
	ButtonURL  string `json:"button_url,omitempty"`
	ButtonText string `json:"button_text,omitempty"`
}

// SendEmail sends one member-facing email through the backend's SES setup.
// body is plain text (newlines become <br>, escaped server-side on the
// backend) — never a Go template, since the backend treats it as untrusted
// input from this service.
func (c *Client) SendEmail(to, subject, body, buttonURL, buttonText string) error {
	return c.post("/internal/onboarding/send-email", sendEmailRequest{
		To:         to,
		Subject:    subject,
		Body:       body,
		ButtonURL:  buttonURL,
		ButtonText: buttonText,
	})
}

type recordAccountRequest struct {
	ApplicationID string `json:"application_id"`
	KthaisEmail   string `json:"kthais_email"`
}

// RecordAccount reports the newly provisioned @kthais.com address back to
// the backend so it's visible on the GeneralApplication row. Not yet called
// anywhere in this service — account provisioning (Google/Mattermost) is a
// later build-order step — but the client method is ready for when it is.
func (c *Client) RecordAccount(applicationID, kthaisEmail string) error {
	return c.post("/internal/onboarding/record-account", recordAccountRequest{
		ApplicationID: applicationID,
		KthaisEmail:   kthaisEmail,
	})
}

type addToLumaRequest struct {
	Email string `json:"email"`
}

// AddToLumaMembers adds email to Luma's "Members" tier via the backend,
// which already holds the org's Luma API key (internal/luma) — this
// service deliberately doesn't get its own separate Luma credential, same
// reasoning as reusing the backend's SES setup for emails above.
func (c *Client) AddToLumaMembers(email string) error {
	return c.post("/internal/onboarding/add-to-luma", addToLumaRequest{Email: email})
}

type removeFromLumaRequest struct {
	Email string `json:"email"`
}

// RemoveFromLumaMembers removes email from Luma's "Members" tier via the
// backend, same credential-sharing reasoning as AddToLumaMembers. Called by
// offboarding.Service.Deactivate/Delete, attempted alongside the Google/
// Mattermost steps there regardless of whether those succeed. Takes ctx
// (unlike this file's other methods) so cancelling the caller actually
// stops the request instead of only being bounded by httpClient's own
// blanket timeout. Tolerates ErrRouteNotFound as a rollout-compatible
// no-op: if the companion backend route isn't deployed yet, that's not a
// reason to fail an otherwise-successful Google/Mattermost offboarding —
// any other status (a route that exists but is erroring) still returns as
// a real error.
func (c *Client) RemoveFromLumaMembers(ctx context.Context, email string) error {
	err := c.postCtx(ctx, "/internal/onboarding/remove-from-luma", removeFromLumaRequest{Email: email})
	if errors.Is(err, ErrRouteNotFound) {
		return nil
	}
	return err
}

func (c *Client) post(path string, body any) error {
	return c.postCtx(context.Background(), path, body)
}

// postCtx is post with an explicit context, for callers (currently just
// RemoveFromLumaMembers) whose caller may cancel or time out and expects
// that to actually stop the request, not just bound it by httpClient's own
// blanket timeout.
func (c *Client) postCtx(ctx context.Context, path string, body any) error {
	if c.cfg.BackendURL == "" {
		return fmt.Errorf("BACKEND_URL is not configured")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BackendURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Secret", c.cfg.OnboardingServiceSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("backend returned 404 for %s: %w", path, ErrRouteNotFound)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("backend returned %d for %s", resp.StatusCode, path)
	}
	return nil
}
