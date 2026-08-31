package handlers

import (
	"time"

	"onboarding-service/internal/models"
	"onboarding-service/internal/utils"

	"gorm.io/gorm"
)

// lookupToken finds the single valid (unused, unexpired) token for the
// given raw value and purpose — same semantics as landingpage-backend's
// TeamQuestionsHandler.lookupToken. purpose is part of the WHERE clause
// (not checked after the fact) so a start_portal token can never be used to
// satisfy a confirm_kth_email check or vice versa.
func lookupToken(db *gorm.DB, purpose models.TokenPurpose, raw string) (*models.OnboardingToken, error) {
	if raw == "" {
		return nil, gorm.ErrRecordNotFound
	}

	var token models.OnboardingToken
	err := db.Where("token_hash = ? AND purpose = ? AND used_at IS NULL AND expires_at > ?",
		utils.HashToken(raw), purpose, time.Now()).
		First(&token).Error
	if err != nil {
		return nil, err
	}
	return &token, nil
}
