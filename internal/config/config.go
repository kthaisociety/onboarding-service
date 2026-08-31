package config

import (
	"os"
	"strings"
	"time"
)

// Config holds every env-driven setting this service needs. Loaded once at
// boot by LoadConfig — see landingpage-backend/internal/config for the
// sibling pattern this mirrors.
type Config struct {
	// Host is the bind address for this service's own HTTP listener. Empty
	// (the default) binds all interfaces, which Docker needs since the
	// container's own network namespace has no other interface to bind. Set
	// to "127.0.0.1" for local dev — this dev Mac has a direct public IP, no
	// NAT, so anything bound to 0.0.0.0 outside a container is reachable
	// from the open internet (see landingpage-backend's SERVER_HOST comment
	// and the earlier exposed-Postgres incident it references).
	Host string
	Port string

	// DBPath is where the SQLite database file lives. Traffic here is a few
	// dozen onboardings per recruiting cycle — SQLite is deliberately chosen
	// over standing up a second Postgres instance for this.
	DBPath string

	// OnboardingServiceSecret authenticates calls in both directions between
	// this service and landingpage-backend: checked on this service's own
	// POST /notify (backend calling in), and sent as X-Service-Secret on
	// this service's outbound calls to the backend's
	// /internal/onboarding/send-email and /record-account (this service
	// calling out). Must match landingpage-backend's
	// ONBOARDING_SERVICE_SECRET exactly.
	OnboardingServiceSecret string

	// BackendURL is landingpage-backend's base URL, e.g.
	// https://api.kthais.com/api/v1 (no trailing slash). This service calls
	// {BackendURL}/internal/onboarding/send-email etc.
	BackendURL string

	// PortalBaseURL is the public onboarding portal's base URL — the
	// frontend pages a person clicks through to submit and confirm their
	// kth.se address (see onboarding-service-plan.md, "Where does the portal
	// live?"). Those frontend pages don't exist yet as of this service's
	// initial build; the links this service emails out are wired to this
	// pattern now so the frontend work has a fixed contract to build
	// against. No trailing slash.
	PortalBaseURL string
}

// StartPortalTokenValidity governs how long a "start your onboarding" link
// stays valid. Longer than the confirm-step token below since a new member
// might not act on it immediately after acceptance.
const StartPortalTokenValidity = 14 * 24 * time.Hour

// ConfirmKthEmailTokenValidity governs how long a "confirm this is your KTH
// email" link stays valid. Deliberately short — this is a link sent
// immediately after the member just submitted that address, so there's no
// reason for it to sit unused for long, and a short window limits how long a
// prefetch-then-never-clicked scanner hit (see the anti-prefetch note in
// onboarding-service-plan.md) leaves a stale unconfirmed token around.
const ConfirmKthEmailTokenValidity = 24 * time.Hour

func LoadConfig() *Config {
	cfg := &Config{
		Host:                    getEnv("HOST", ""),
		Port:                    getEnv("PORT", "8000"),
		DBPath:                  getEnv("DB_PATH", "./onboarding.db"),
		OnboardingServiceSecret: getEnv("ONBOARDING_SERVICE_SECRET", ""),
		BackendURL:              strings.TrimSuffix(getEnv("BACKEND_URL", ""), "/"),
		PortalBaseURL:           strings.TrimSuffix(getEnv("PORTAL_BASE_URL", ""), "/"),
	}
	return cfg
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}
