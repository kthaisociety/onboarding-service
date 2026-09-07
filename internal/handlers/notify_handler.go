package handlers

import (
	"log"
	"net/http"
	"time"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/config"
	"onboarding-service/internal/models"
	"onboarding-service/internal/utils"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// NotifyHandler receives the hand-off from landingpage-backend when an
// applicant is accepted (see general_application_finalize.go's
// notifyOnboardingService) and starts this service's side of onboarding.
type NotifyHandler struct {
	db      *gorm.DB
	cfg     *config.Config
	backend *backendclient.Client
}

func NewNotifyHandler(db *gorm.DB, cfg *config.Config, backend *backendclient.Client) *NotifyHandler {
	return &NotifyHandler{db: db, cfg: cfg, backend: backend}
}

func (h *NotifyHandler) Register(r *gin.RouterGroup) {
	r.POST("/notify", requireServiceSecret(h.cfg), h.Notify)
}

type notifyRequest struct {
	ApplicationID string `json:"application_id" binding:"required"`
	FirstName     string `json:"first_name" binding:"required"`
	LastName      string `json:"last_name" binding:"required"`
	PersonalEmail string `json:"personal_email" binding:"required"`
	AssignedTeam  string `json:"assigned_team" binding:"required"`
}

// Notify creates the OnboardingRecord and sends the "start your onboarding"
// email. Idempotent on ApplicationID: since the backend's own call to this
// endpoint is fire-and-forget with no retry logic today, a duplicate call
// is more likely a mistake than a legitimate re-notify — it returns the
// existing record instead of creating a second one or re-issuing a token.
func (h *NotifyHandler) Notify(c *gin.Context) {
	var req notifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "application_id, first_name, last_name, personal_email, and assigned_team are required"})
		return
	}

	var existing models.OnboardingRecord
	if err := h.db.Where("application_id = ?", req.ApplicationID).First(&existing).Error; err == nil {
		c.JSON(http.StatusOK, existing)
		return
	} else if err != gorm.ErrRecordNotFound {
		log.Printf("notify: database error checking for existing record for %s: %v", req.ApplicationID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	record := models.OnboardingRecord{
		ApplicationID: req.ApplicationID,
		FirstName:     req.FirstName,
		LastName:      req.LastName,
		PersonalEmail: req.PersonalEmail,
		AssignedTeam:  req.AssignedTeam,
		State:         models.StateNotified,
	}

	raw, hash, err := utils.GenerateToken()
	if err != nil {
		log.Printf("notify: failed to generate token for %s: %v", req.ApplicationID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		token := models.OnboardingToken{
			OnboardingRecordID: record.ID,
			Purpose:            models.PurposeStartPortal,
			TokenHash:          hash,
			ExpiresAt:          time.Now().Add(config.StartPortalTokenValidity),
		}
		return tx.Create(&token).Error
	})
	if err != nil {
		log.Printf("notify: failed to save record/token for %s: %v", req.ApplicationID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save"})
		return
	}

	go func() {
		startLink := h.cfg.PortalBaseURL + "/start?token=" + raw
		body := "Congratulations on being accepted to KTH AI Society! To get started, click below and " +
			"tell us your kth.se email address so we can set up your kthais.com account and Mattermost access."
		if err := h.backend.SendEmail(req.PersonalEmail, "Start your KTH AI Society onboarding", body, startLink, "Start onboarding"); err != nil {
			log.Printf("notify: failed to send start-portal email for %s: %v", req.ApplicationID, err)
		}
	}()

	c.JSON(http.StatusOK, record)
}
