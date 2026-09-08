package handlers

import (
	"net/http"
	"strings"

	"onboarding-service/internal/config"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// EmailSettingsHandler owns the admin-editable intro text of the "start
// your onboarding" email. Like RecordsHandler and RecordActionsHandler,
// this only ever sees requests from landingpage-backend's own admin-JWT-gated
// proxy (see manual_onboarding_handler.go there) — onboarding-service has no
// notion of admin identity itself, so the caller passes UpdatedByEmail along.
type EmailSettingsHandler struct {
	db  *gorm.DB
	cfg *config.Config
}

func NewEmailSettingsHandler(db *gorm.DB, cfg *config.Config) *EmailSettingsHandler {
	return &EmailSettingsHandler{db: db, cfg: cfg}
}

func (h *EmailSettingsHandler) Register(r *gin.RouterGroup) {
	r.GET("/internal/onboarding/email-settings", requireServiceSecret(h.cfg), h.Get)
	r.PUT("/internal/onboarding/email-settings", requireServiceSecret(h.cfg), h.Update)
	r.POST("/internal/onboarding/email-settings/preview", requireServiceSecret(h.cfg), h.Preview)
}

func (h *EmailSettingsHandler) Get(c *gin.Context) {
	settings, err := loadEmailSettings(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load email settings"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"intro_text": introOrDefault(settings.IntroText)})
}

type emailSettingsRequest struct {
	IntroText      string `json:"intro_text"`
	UpdatedByEmail string `json:"updated_by_email"`
}

func (h *EmailSettingsHandler) Update(c *gin.Context) {
	var req emailSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	settings, err := saveEmailSettings(h.db, strings.TrimSpace(req.IntroText), strings.TrimSpace(req.UpdatedByEmail))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save email settings"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"intro_text": introOrDefault(settings.IntroText)})
}

// previewFirstName is a sample name, matching landingpage-backend's own
// "Alex"/"Jones" convention for its interview-invite preview.
const previewFirstName = "Alex"

// Preview builds the exact subject/body buildStartOnboardingEmail would send
// for the given (possibly unsaved) intro text, so the admin panel's preview
// can never drift from what a real send would produce.
func (h *EmailSettingsHandler) Preview(c *gin.Context) {
	var req emailSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	subject, body := buildStartOnboardingEmail(req.IntroText, previewFirstName)
	c.JSON(http.StatusOK, gin.H{"subject": subject, "body": body})
}
