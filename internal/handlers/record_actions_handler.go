package handlers

import (
	"context"
	"log"
	"net/http"
	"time"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/config"
	"onboarding-service/internal/models"
	"onboarding-service/internal/provisioning"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// RecordActionsHandler owns every admin action that operates on an
// existing OnboardingRecord: retrying a stuck provisioning attempt,
// cancelling an onboarding outright, and restarting one from scratch. All
// three identify the record the same way (see recordLookupRequest) and are
// secret-gated the same way — grouping them here rather than splitting
// across differently-shaped handlers stopped making sense once there were
// three.
type RecordActionsHandler struct {
	db           *gorm.DB
	cfg          *config.Config
	backend      *backendclient.Client
	provisioning *provisioning.Service
}

func NewRecordActionsHandler(db *gorm.DB, cfg *config.Config, backend *backendclient.Client, provisioning *provisioning.Service) *RecordActionsHandler {
	return &RecordActionsHandler{db: db, cfg: cfg, backend: backend, provisioning: provisioning}
}

func (h *RecordActionsHandler) Register(r *gin.RouterGroup) {
	r.POST("/internal/onboarding/retry-provisioning", requireServiceSecret(h.cfg), h.Retry)
	r.POST("/internal/onboarding/cancel", requireServiceSecret(h.cfg), h.Cancel)
	r.POST("/internal/onboarding/restart", requireServiceSecret(h.cfg), h.Restart)
}

// invalidateOutstandingTokens marks every unused token for a record as
// used, so an old emailed link can never be actioned after a cancel or
// restart — a restart's fresh token replaces it; a cancel leaves nothing
// valid at all.
func (h *RecordActionsHandler) invalidateOutstandingTokens(recordID uint) error {
	return h.db.Model(&models.OnboardingToken{}).
		Where("onboarding_record_id = ? AND used_at IS NULL", recordID).
		Update("used_at", time.Now()).Error
}

func (h *RecordActionsHandler) findRecord(c *gin.Context) (*models.OnboardingRecord, bool) {
	var req recordLookupRequest
	if err := c.ShouldBindJSON(&req); err != nil || (req.ApplicationID == "" && req.ID == 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "application_id or id is required"})
		return nil, false
	}
	record, err := lookupOnboardingRecord(h.db, req)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no matching onboarding record"})
		return nil, false
	}
	return record, true
}

// Retry re-runs the same idempotent Provision() call used on the happy
// path against a record stuck in StateFailed (e.g. the Google account was
// created but a later step failed) — so a stuck record never requires
// direct SQLite access. See onboarding-service-plan.md's
// retry-provisioning section.
func (h *RecordActionsHandler) Retry(c *gin.Context) {
	record, ok := h.findRecord(c)
	if !ok {
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
	if err := h.provisioning.Provision(ctx, record); err != nil {
		log.Printf("retry-provisioning: record %d still failing: %v", record.ID, err)
	}

	c.JSON(http.StatusOK, record)
}

// Cancel marks a record cancelled — a deliberate admin decision, not
// something that needs follow-up (unlike StateFailed) — and invalidates
// any outstanding tokens so old emailed links stop working. Deliberately
// does not touch any Google Workspace/Mattermost account that may already
// exist: cleanup there is manual, by design.
func (h *RecordActionsHandler) Cancel(c *gin.Context) {
	record, ok := h.findRecord(c)
	if !ok {
		return
	}

	if record.State == models.StateComplete || record.State == models.StateCancelled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "record is already " + string(record.State)})
		return
	}

	if err := h.invalidateOutstandingTokens(record.ID); err != nil {
		log.Printf("cancel: failed to invalidate tokens for record %d: %v", record.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel"})
		return
	}
	record.State = models.StateCancelled
	if err := h.db.Model(record).Update("state", models.StateCancelled).Error; err != nil {
		log.Printf("cancel: failed to save record %d: %v", record.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel"})
		return
	}

	c.JSON(http.StatusOK, record)
}

// Restart resets a record back to StateNotified and sends a fresh "start
// your onboarding" email with a new token, invalidating any old one(s).
// KthaisEmail is deliberately left untouched: clearing it would make
// ensureAccount (internal/provisioning/provisioning.go) think no account
// exists yet and create a second one on the next provisioning run, since
// the first address is already taken. Only allowed when not already
// complete — nothing left to restart at that point.
func (h *RecordActionsHandler) Restart(c *gin.Context) {
	record, ok := h.findRecord(c)
	if !ok {
		return
	}

	if record.State == models.StateComplete {
		c.JSON(http.StatusBadRequest, gin.H{"error": "record is already complete"})
		return
	}

	if err := h.invalidateOutstandingTokens(record.ID); err != nil {
		log.Printf("restart: failed to invalidate tokens for record %d: %v", record.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restart"})
		return
	}

	record.KthEmail = ""
	record.FailureReason = ""
	record.State = models.StateNotified
	if err := h.db.Model(record).Updates(map[string]any{
		"kth_email":      "",
		"failure_reason": "",
		"state":          models.StateNotified,
	}).Error; err != nil {
		log.Printf("restart: failed to save record %d: %v", record.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restart"})
		return
	}

	raw, err := issueStartPortalToken(h.db, record)
	if err != nil {
		log.Printf("restart: failed to issue a new start-portal token for record %d: %v", record.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "restarted, but failed to send a new email"})
		return
	}
	sendStartOnboardingEmailAsync(h.cfg, h.backend, record, raw)

	c.JSON(http.StatusOK, record)
}
