package handlers

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"onboarding-service/internal/config"
	"onboarding-service/internal/models"
	"onboarding-service/internal/offboarding"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// OffboardingHandler is the mechanism half of member offboarding —
// suspending/deleting the Google Workspace + Mattermost accounts for a
// @kthais.com email. Policy (who may call this, and the "type this exact
// phrase to confirm" gate for the irreversible delete) lives entirely in
// landingpage-backend's own proxy, same separation as every other
// onboarding-service handler: this service only ever trusts the shared
// secret, never re-derives admin identity itself.
type OffboardingHandler struct {
	db          *gorm.DB
	cfg         *config.Config
	offboarding *offboarding.Service
}

func NewOffboardingHandler(db *gorm.DB, cfg *config.Config, svc *offboarding.Service) *OffboardingHandler {
	return &OffboardingHandler{db: db, cfg: cfg, offboarding: svc}
}

func (h *OffboardingHandler) Register(r *gin.RouterGroup) {
	r.POST("/internal/offboarding/deactivate", requireServiceSecret(h.cfg), h.Deactivate)
	r.POST("/internal/offboarding/delete", requireServiceSecret(h.cfg), h.Delete)
}

type offboardingRequest struct {
	Email string `json:"email" binding:"required"`
}

// isKthaisEmail is a second, defense-in-depth check on top of whatever the
// caller already validated — this service holds the credentials, so it
// never trusts a caller's validation alone for an action this destructive.
func isKthaisEmail(email string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(email)), "@kthais.com")
}

// Deactivate suspends the Google account and deactivates the Mattermost
// account for email — reversible on both sides from each system's own
// admin console.
func (h *OffboardingHandler) Deactivate(c *gin.Context) {
	var req offboardingRequest
	if err := c.ShouldBindJSON(&req); err != nil || !isKthaisEmail(req.Email) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid @kthais.com email is required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.offboarding.Deactivate(ctx, req.Email); err != nil {
		log.Printf("offboarding: failed to fully deactivate %s: %v", req.Email, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deactivated"})
}

// Delete permanently deletes the Google account and attempts to
// permanently delete the Mattermost account for email. Cannot be undone.
// Also marks any OnboardingRecord for this email as offboarded — without
// that, a member who completed onboarding and was later deleted here would
// leave their record stuck at "complete" forever, with no state that
// accurately reflects they're gone and no admin action available on it.
func (h *OffboardingHandler) Delete(c *gin.Context) {
	var req offboardingRequest
	if err := c.ShouldBindJSON(&req); err != nil || !isKthaisEmail(req.Email) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid @kthais.com email is required"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.offboarding.Delete(ctx, req.Email); err != nil {
		log.Printf("offboarding: failed to fully delete %s: %v", req.Email, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	h.markOnboardingRecordOffboarded(req.Email)

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// markOnboardingRecordOffboarded is best-effort and never affects the
// response: the real, irreversible work (deleting the actual Google
// Workspace/Mattermost accounts) already succeeded by the time this runs,
// so a failure to update our own record-keeping shouldn't be reported as
// an offboarding failure — it's logged instead. Matches on KthaisEmail,
// not PersonalEmail: that's the address that was actually deleted.
func (h *OffboardingHandler) markOnboardingRecordOffboarded(email string) {
	result := h.db.Model(&models.OnboardingRecord{}).
		Where("kthais_email = ? AND state NOT IN ?", email, []models.OnboardingState{
			models.StateCancelled, models.StateOffboarded,
		}).
		Update("state", models.StateOffboarded)
	if result.Error != nil {
		log.Printf("offboarding: deleted %s but failed to update its onboarding record: %v", email, result.Error)
		return
	}
	if result.RowsAffected > 0 {
		log.Printf("offboarding: marked the onboarding record for %s as offboarded", email)
	}
}
