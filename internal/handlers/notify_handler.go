package handlers

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/config"
	"onboarding-service/internal/emailcontent"
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
	// ApplicationID is nil for an admin's manual onboarding action (outside
	// the recruitment pipeline, no backend GeneralApplication exists) —
	// deliberately not binding:"required".
	ApplicationID *string `json:"application_id"`
	FirstName     string  `json:"first_name" binding:"required"`
	LastName      string  `json:"last_name" binding:"required"`
	PersonalEmail string  `json:"personal_email" binding:"required"`
	AssignedTeam  string  `json:"assigned_team" binding:"required"`
}

// logID renders an *string for a log line without printing a pointer
// address — "manual" when nil, matching how such records are described
// elsewhere (no backend application to reference).
func logID(id *string) string {
	if id == nil {
		return "manual"
	}
	return *id
}

// Notify creates the OnboardingRecord and sends the "start your onboarding"
// email. Idempotent on ApplicationID when one is present: since the
// backend's own call to this endpoint is fire-and-forget with no retry
// logic today, a duplicate call is more likely a mistake than a legitimate
// re-notify — it returns the existing record instead of creating a second
// one or re-issuing a token. A nil ApplicationID (manual onboarding) skips
// this check entirely and always creates a new record — there's no
// real-world event to deduplicate against the way there is for a backend
// accept-click.
func (h *NotifyHandler) Notify(c *gin.Context) {
	var req notifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "first_name, last_name, personal_email, and assigned_team are required"})
		return
	}

	if req.ApplicationID != nil {
		var existing models.OnboardingRecord
		if err := h.db.Where("application_id = ?", *req.ApplicationID).First(&existing).Error; err == nil {
			c.JSON(http.StatusOK, existing)
			return
		} else if err != gorm.ErrRecordNotFound {
			log.Printf("notify: database error checking for existing record for %s: %v", logID(req.ApplicationID), err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
			return
		}
	}

	record := models.OnboardingRecord{
		ApplicationID: req.ApplicationID,
		FirstName:     req.FirstName,
		LastName:      req.LastName,
		PersonalEmail: req.PersonalEmail,
		AssignedTeam:  req.AssignedTeam,
		State:         models.StateNotified,
	}

	var raw string
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		var tokenErr error
		raw, tokenErr = issueStartPortalToken(tx, &record)
		return tokenErr
	})
	if err != nil {
		log.Printf("notify: failed to save record/token for %s: %v", logID(req.ApplicationID), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save"})
		return
	}

	// Only after the transaction above has actually committed — the
	// recipient could click the link before an earlier-fired goroutine's
	// send even completes, let alone before the token it references exists.
	sendStartOnboardingEmailAsync(h.db, h.cfg, h.backend, &record, raw)

	c.JSON(http.StatusOK, record)
}

// issueStartPortalToken creates a fresh start_portal token for record
// within db (a transaction handle, or the plain *gorm.DB when there's no
// companion write to keep atomic with), returning the raw token to embed
// in the emailed link — only its hash is persisted. Shared by Notify (a
// newly created record) and RecordActionsHandler.Restart (an existing one
// starting over).
func issueStartPortalToken(db *gorm.DB, record *models.OnboardingRecord) (string, error) {
	raw, hash, err := utils.GenerateToken()
	if err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}
	token := models.OnboardingToken{
		OnboardingRecordID: record.ID,
		Purpose:            models.PurposeStartPortal,
		TokenHash:          hash,
		ExpiresAt:          time.Now().Add(config.StartPortalTokenValidity),
	}
	if err := db.Create(&token).Error; err != nil {
		return "", fmt.Errorf("failed to save token: %w", err)
	}
	return raw, nil
}

// sendStartOnboardingEmailAsync fires the "start your onboarding" email in
// the background. Call only once the token (and, for a new record, the
// record itself) is durably committed — see the ordering note at Notify's
// call site. The intro paragraph comes from whatever an admin has saved via
// EmailSettingsHandler (falling back to a default if none has) — read fresh
// here rather than passed in, since the goroutine may run well after the
// caller's own request.
func sendStartOnboardingEmailAsync(db *gorm.DB, cfg *config.Config, backend *backendclient.Client, record *models.OnboardingRecord, rawToken string) {
	go func() {
		settings, err := emailcontent.Load(db)
		if err != nil {
			log.Printf("start-onboarding email: failed to load email settings for record %d, using default: %v", record.ID, err)
		}
		startLink := cfg.PortalBaseURL + "/start?token=" + rawToken
		subject, body := emailcontent.BuildStart(settings.StartIntroText, record.FirstName)
		if err := backend.SendEmail(record.PersonalEmail, subject, body, startLink, emailcontent.StartButtonText); err != nil {
			log.Printf("start-onboarding email: failed to send for record %d: %v", record.ID, err)
		}
	}()
}
