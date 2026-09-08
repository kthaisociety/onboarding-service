package handlers

import (
	"context"
	"log"
	"net/http"
	"time"

	"onboarding-service/internal/config"
	"onboarding-service/internal/models"
	"onboarding-service/internal/provisioning"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// RetryHandler exposes a manual recovery path for a record stuck in
// StateFailed (e.g. the Google account was created but a later step
// failed) — a human-triggered re-run of the same idempotent Provision()
// call used on the happy path, so a stuck record never requires direct
// SQLite access. Identifies the record by application_id or by this
// service's own record id (the latter for manual onboardings, which have
// no application_id at all). See onboarding-service-plan.md's
// retry-provisioning section.
type RetryHandler struct {
	db           *gorm.DB
	cfg          *config.Config
	provisioning *provisioning.Service
}

func NewRetryHandler(db *gorm.DB, cfg *config.Config, provisioning *provisioning.Service) *RetryHandler {
	return &RetryHandler{db: db, cfg: cfg, provisioning: provisioning}
}

func (h *RetryHandler) Register(r *gin.RouterGroup) {
	r.POST("/internal/onboarding/retry-provisioning", requireServiceSecret(h.cfg), h.Retry)
}

// retryRequest identifies the record to retry one of two ways:
// application_id for a record tied to a real backend application, or id
// (this service's own record ID) for a manual onboarding — which has no
// application_id to look up by at all (see models.OnboardingRecord's doc
// comment on nullable ApplicationID).
type retryRequest struct {
	ApplicationID string `json:"application_id"`
	ID            uint   `json:"id"`
}

func (h *RetryHandler) Retry(c *gin.Context) {
	var req retryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "application_id or id is required"})
		return
	}

	var record models.OnboardingRecord
	var err error
	switch {
	case req.ApplicationID != "":
		err = h.db.Where("application_id = ?", req.ApplicationID).First(&record).Error
	case req.ID != 0:
		err = h.db.First(&record, req.ID).Error
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "application_id or id is required"})
		return
	}
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no matching onboarding record"})
		return
	}

	if record.State != models.StateKthEmailConfirmed && record.State != models.StateFailed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "record is not in a retryable state"})
		return
	}

	// Detached context, not c.Request.Context(): provisioning should always
	// run to completion (or its own timeout) rather than aborting if the
	// caller's connection drops mid-request — same reasoning as
	// PortalHandler.Confirm.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := h.provisioning.Provision(ctx, &record); err != nil {
		log.Printf("retry-provisioning: record %d still failing: %v", record.ID, err)
	}

	c.JSON(http.StatusOK, record)
}
