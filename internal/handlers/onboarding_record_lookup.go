package handlers

import (
	"onboarding-service/internal/models"

	"gorm.io/gorm"
)

// recordLookupRequest identifies an OnboardingRecord one of two ways:
// application_id for a record tied to a real backend application, or id
// (this service's own record ID) for a manual onboarding — which has no
// application_id to look up by at all (see OnboardingRecord's doc comment
// on nullable ApplicationID). Shared by every admin action that operates
// on an existing record (retry, cancel, restart).
type recordLookupRequest struct {
	ApplicationID string `json:"application_id"`
	ID            uint   `json:"id"`
}

func lookupOnboardingRecord(db *gorm.DB, req recordLookupRequest) (*models.OnboardingRecord, error) {
	var record models.OnboardingRecord
	var err error
	switch {
	case req.ApplicationID != "":
		err = db.Where("application_id = ?", req.ApplicationID).First(&record).Error
	case req.ID != 0:
		err = db.First(&record, req.ID).Error
	default:
		return nil, gorm.ErrRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}
