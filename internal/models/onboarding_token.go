package models

import (
	"time"

	"gorm.io/gorm"
)

type TokenPurpose string

const (
	PurposeStartPortal     TokenPurpose = "start_portal"
	PurposeConfirmKthEmail TokenPurpose = "confirm_kth_email"
)

// OnboardingToken is a single-use, hashed access token gating one step of
// the portal flow — mirrors landingpage-backend's TeamQuestionsToken
// pattern exactly (see internal/utils/token.go). Only TokenHash is
// persisted; the raw token is only ever embedded in the emailed link.
// Multiple rows may exist per OnboardingRecordID across the two purposes
// (and a resend would issue another one of the same purpose); lookups only
// consider the most recent unused, unexpired row for the requested purpose.
type OnboardingToken struct {
	gorm.Model
	OnboardingRecordID uint         `gorm:"not null;index" json:"onboarding_record_id"`
	Purpose            TokenPurpose `gorm:"not null" json:"purpose"`
	TokenHash          string       `gorm:"not null;uniqueIndex" json:"-"`
	ExpiresAt          time.Time    `gorm:"not null" json:"expires_at"`
	UsedAt             *time.Time   `json:"used_at"`
}
