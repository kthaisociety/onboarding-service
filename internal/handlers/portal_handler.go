package handlers

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/config"
	"onboarding-service/internal/models"
	"onboarding-service/internal/provisioning"
	"onboarding-service/internal/utils"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// PortalHandler serves the two public, token-authenticated steps a new
// member goes through after clicking the emailed onboarding link: submit
// their kth.se address, then confirm it via a second link (see
// onboarding-service-plan.md's anti-prefetch note — the actual page that
// renders the "Confirm" button and issues this POST lives on
// landingpage-frontend, not here; this handler is just the API it calls).
// No shared-secret gate on these routes — the single-use token in the URL
// is the auth.
type PortalHandler struct {
	db           *gorm.DB
	cfg          *config.Config
	backend      *backendclient.Client
	provisioning *provisioning.Service
}

func NewPortalHandler(db *gorm.DB, cfg *config.Config, backend *backendclient.Client, provisioning *provisioning.Service) *PortalHandler {
	return &PortalHandler{db: db, cfg: cfg, backend: backend, provisioning: provisioning}
}

func (h *PortalHandler) Register(r *gin.RouterGroup) {
	portal := r.Group("/portal")
	{
		portal.POST("/submit-email", h.SubmitEmail)
		portal.POST("/confirm", h.Confirm)
	}
}

var invalidLinkError = gin.H{"error": "this link is invalid or has expired"}

type submitEmailRequest struct {
	Token    string `json:"token" binding:"required"`
	KthEmail string `json:"kth_email" binding:"required"`
}

// SubmitEmail consumes a start_portal token and issues a
// confirm_kth_email one. Nothing is ever emailed to an address before it's
// been confirmed via the second step — see the plan doc's kth.se
// confirmation rationale.
func (h *PortalHandler) SubmitEmail(c *gin.Context) {
	var req submitEmailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token and kth_email are required"})
		return
	}

	kthEmail := strings.ToLower(strings.TrimSpace(req.KthEmail))
	if !strings.HasSuffix(kthEmail, "@kth.se") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "must be a kth.se address"})
		return
	}

	token, err := lookupToken(h.db, models.PurposeStartPortal, req.Token)
	if err != nil {
		c.JSON(http.StatusNotFound, invalidLinkError)
		return
	}

	var record models.OnboardingRecord
	if err := h.db.First(&record, token.OnboardingRecordID).Error; err != nil {
		c.JSON(http.StatusNotFound, invalidLinkError)
		return
	}

	raw, hash, err := utils.GenerateToken()
	if err != nil {
		log.Printf("submit-email: failed to generate token for record %d: %v", record.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	err = h.db.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		token.UsedAt = &now
		if err := tx.Save(token).Error; err != nil {
			return err
		}

		record.KthEmail = kthEmail
		record.State = models.StateKthEmailSubmitted
		if err := tx.Save(&record).Error; err != nil {
			return err
		}

		confirmToken := models.OnboardingToken{
			OnboardingRecordID: record.ID,
			Purpose:            models.PurposeConfirmKthEmail,
			TokenHash:          hash,
			ExpiresAt:          now.Add(config.ConfirmKthEmailTokenValidity),
		}
		return tx.Create(&confirmToken).Error
	})
	if err != nil {
		log.Printf("submit-email: failed to save record %d: %v", record.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save"})
		return
	}

	go func() {
		confirmLink := h.cfg.PortalBaseURL + "/confirm?token=" + raw
		body := "Hi " + record.FirstName + ",\n\n" +
			"Please confirm this is your KTH email address to continue setting up your KTH AI Society account.\n\n" +
			"Click below, then confirm once more on the page that opens — that extra click keeps your account " +
			"safe from automated email link scanners."
		// Deliberately not "Confirm this is me" here too: this link is only
		// ever a GET navigation (email clients strip JS/forms, so nothing in
		// the email itself can perform the real confirm), and KTH's mail
		// prefetches links (Safe Links or equivalent) — see
		// ConfirmOnboarding's own comment for the other half of this. Using
		// identical wording on both buttons made the flow look like a
		// broken duplicate rather than two intentional steps.
		if err := h.backend.SendEmail(kthEmail, "Confirm your KTH email", body, confirmLink, "Continue to confirm"); err != nil {
			log.Printf("submit-email: failed to send confirmation email for record %d: %v", record.ID, err)
		}
	}()

	c.JSON(http.StatusOK, record)
}

type confirmRequest struct {
	Token string `json:"token" binding:"required"`
}

// Confirm consumes a confirm_kth_email token. This is deliberately only
// ever called from a POST triggered by an explicit button click on the
// frontend's confirm page — never auto-run on the link's GET — since KTH's
// Microsoft-hosted mail likely prefetches URLs in emails (Safe Links or
// equivalent), which would silently "confirm" a naive auto-GET link before
// the actual person ever saw it.
//
// Once the record reaches kth_email_confirmed, this synchronously runs
// account provisioning (Google Workspace + Mattermost + final emails) —
// synchronous rather than this file's usual fire-and-forget email pattern,
// since this is the one step where the person needs to actually learn
// something went wrong (via the returned record.state) rather than an
// account silently never appearing. Always responds 200 — record.state
// carries the real outcome, and a failure here is recoverable via
// RetryHandler rather than the request needing to itself succeed or fail.
func (h *PortalHandler) Confirm(c *gin.Context) {
	var req confirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}

	token, err := lookupToken(h.db, models.PurposeConfirmKthEmail, req.Token)
	if err != nil {
		c.JSON(http.StatusNotFound, invalidLinkError)
		return
	}

	var record models.OnboardingRecord
	if err := h.db.First(&record, token.OnboardingRecordID).Error; err != nil {
		c.JSON(http.StatusNotFound, invalidLinkError)
		return
	}

	err = h.db.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		token.UsedAt = &now
		if err := tx.Save(token).Error; err != nil {
			return err
		}

		record.State = models.StateKthEmailConfirmed
		return tx.Save(&record).Error
	})
	if err != nil {
		log.Printf("confirm: failed to save record %d: %v", record.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save"})
		return
	}

	// Detached context, not c.Request.Context(): provisioning should always
	// run to completion (or its own timeout) rather than aborting if the
	// person's connection drops mid-request (e.g. closing a laptop lid).
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := h.provisioning.Provision(ctx, &record); err != nil {
		log.Printf("confirm: provisioning failed for record %d: %v", record.ID, err)
	}

	c.JSON(http.StatusOK, record)
}
