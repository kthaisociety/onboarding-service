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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"onboarding-service/internal/config"
)

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
// which already holds the org's Luma API key for its own newsletter-signup
// integration (internal/luma) — this service deliberately doesn't get its
// own separate Luma credential, same reasoning as reusing the backend's SES
// setup for emails above.
func (c *Client) AddToLumaMembers(email string) error {
	return c.post("/internal/onboarding/add-to-luma", addToLumaRequest{Email: email})
}

func (c *Client) post(path string, body any) error {
	if c.cfg.BackendURL == "" {
		return fmt.Errorf("BACKEND_URL is not configured")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.cfg.BackendURL+path, bytes.NewReader(payload))
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

	if resp.StatusCode >= 300 {
		return fmt.Errorf("backend returned %d for %s", resp.StatusCode, path)
	}
	return nil
}
