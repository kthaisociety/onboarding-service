package models

import "gorm.io/gorm"

type OnboardingState string

const (
	StateNotified          OnboardingState = "notified"
	StateKthEmailSubmitted OnboardingState = "kth_email_submitted"
	StateKthEmailConfirmed OnboardingState = "kth_email_confirmed"
	StateProvisioned       OnboardingState = "provisioned"
	StateEmailed           OnboardingState = "emailed"
	StateComplete          OnboardingState = "complete"
	StateFailed            OnboardingState = "failed"
)

// OnboardingRecord tracks one accepted applicant through the onboarding
// flow, from the backend's initial /notify call through account
// provisioning. ApplicationID references landingpage-backend's
// GeneralApplication.Id — not a local foreign key, since that row lives in
// a different service's database entirely (see
// onboarding-service-plan.md's isolation reasoning).
//
// States beyond kth_email_confirmed (provisioned, emailed, complete,
// failed) are defined here but not yet driven by any handler — Google
// Workspace/Mattermost provisioning is a later build-order step. A record
// currently tops out at kth_email_confirmed.
type OnboardingRecord struct {
	gorm.Model
	ApplicationID string          `gorm:"uniqueIndex;not null" json:"application_id"`
	FirstName     string          `gorm:"not null" json:"first_name"`
	LastName      string          `gorm:"not null" json:"last_name"`
	PersonalEmail string          `gorm:"not null" json:"personal_email"`
	AssignedTeam  string          `gorm:"not null" json:"assigned_team"`
	State         OnboardingState `gorm:"not null;default:'notified'" json:"state"`
	// KthEmail is the kth.se address the member submitted through the
	// portal, filled once State reaches kth_email_submitted.
	KthEmail string `gorm:"default:''" json:"kth_email"`
	// KthaisEmail is the provisioned @kthais.com address, filled once
	// State reaches provisioned. Reported back to landingpage-backend via
	// POST /internal/onboarding/record-account once set.
	KthaisEmail string `gorm:"default:''" json:"kthais_email"`
}
