package handlers

import (
	"net/http"
	"strings"

	"onboarding-service/internal/config"
	"onboarding-service/internal/emailcontent"
	"onboarding-service/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// EmailSettingsHandler owns the admin-editable intro text of all three
// onboarding emails (start, account-credentials, Mattermost
// getting-started). Like RecordsHandler and RecordActionsHandler, this
// only ever sees requests from landingpage-backend's own admin-JWT-gated
// proxy (see manual_onboarding_handler.go there) — onboarding-service has
// no notion of admin identity itself, so the caller passes UpdatedByEmail
// along.
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

type emailSettingsResponse struct {
	StartIntroText      string `json:"start_intro_text"`
	AccountIntroText    string `json:"account_intro_text"`
	MattermostIntroText string `json:"mattermost_intro_text"`
}

// orDefault is what any reader (the real sender, the GET/PUT response)
// sees for a field that has a non-empty default when no admin-set value is
// saved yet — the account email's intro deliberately has no such default
// (see emailcontent.BuildAccount), so it's never passed through this.
func orDefault(text, def string) string {
	if strings.TrimSpace(text) == "" {
		return def
	}
	return text
}

func settingsResponse(settings models.OnboardingEmailSettings) emailSettingsResponse {
	return emailSettingsResponse{
		StartIntroText:      orDefault(settings.StartIntroText, emailcontent.DefaultStartIntro),
		AccountIntroText:    settings.AccountIntroText,
		MattermostIntroText: orDefault(settings.MattermostIntroText, emailcontent.DefaultMattermostIntro),
	}
}

func (h *EmailSettingsHandler) Get(c *gin.Context) {
	settings, err := emailcontent.Load(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load email settings"})
		return
	}
	c.JSON(http.StatusOK, settingsResponse(settings))
}

type updateEmailSettingsRequest struct {
	StartIntroText      string `json:"start_intro_text"`
	AccountIntroText    string `json:"account_intro_text"`
	MattermostIntroText string `json:"mattermost_intro_text"`
	UpdatedByEmail      string `json:"updated_by_email"`
}

func (h *EmailSettingsHandler) Update(c *gin.Context) {
	var req updateEmailSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	settings, err := emailcontent.Save(h.db,
		strings.TrimSpace(req.StartIntroText),
		strings.TrimSpace(req.AccountIntroText),
		strings.TrimSpace(req.MattermostIntroText),
		strings.TrimSpace(req.UpdatedByEmail),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save email settings"})
		return
	}
	c.JSON(http.StatusOK, settingsResponse(settings))
}

type previewEmailSettingsRequest struct {
	Kind      string `json:"kind"`
	IntroText string `json:"intro_text"`
}

// Preview builds the exact subject/body the matching Build* function would
// produce for the given (possibly unsaved) intro text, so the admin
// panel's preview can never drift from what a real send would produce.
// kind selects which of the three onboarding emails to render; the
// account email is rendered with sample credentials, never real data.
func (h *EmailSettingsHandler) Preview(c *gin.Context) {
	var req previewEmailSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	var subject, body string
	switch req.Kind {
	case "start":
		subject, body = emailcontent.BuildStart(req.IntroText, emailcontent.PreviewFirstName)
	case "account":
		subject, body = emailcontent.PreviewAccount(req.IntroText)
	case "mattermost":
		subject, body = emailcontent.BuildMattermost(req.IntroText, emailcontent.PreviewFirstName)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be one of: start, account, mattermost"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"subject": subject, "body": body})
}
