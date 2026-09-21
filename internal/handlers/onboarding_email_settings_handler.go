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

// EmailSettingsHandler owns the admin-editable intro text of all four
// onboarding emails (start, confirm-kth-email, account-credentials,
// Mattermost getting-started). Like RecordsHandler and RecordActionsHandler, this
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
	ConfirmIntroText    string `json:"confirm_intro_text"`
	AccountIntroText    string `json:"account_intro_text"`
	MattermostIntroText string `json:"mattermost_intro_text"`
	ContractIntroText   string `json:"contract_intro_text"`
	ContractURL         string `json:"contract_url"`
	BylawsURL           string `json:"bylaws_url"`
	LumaKickoffURL      string `json:"luma_kickoff_url"`
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
		ConfirmIntroText:    orDefault(settings.ConfirmIntroText, emailcontent.DefaultConfirmIntro),
		AccountIntroText:    settings.AccountIntroText,
		MattermostIntroText: orDefault(settings.MattermostIntroText, emailcontent.DefaultMattermostIntro),
		ContractIntroText:   orDefault(settings.ContractIntroText, emailcontent.DefaultContractIntro),
		ContractURL:         settings.ContractURL,
		BylawsURL:           settings.BylawsURL,
		LumaKickoffURL:      settings.LumaKickoffURL,
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
	ConfirmIntroText    string `json:"confirm_intro_text"`
	AccountIntroText    string `json:"account_intro_text"`
	MattermostIntroText string `json:"mattermost_intro_text"`
	ContractIntroText   string `json:"contract_intro_text"`
	ContractURL         string `json:"contract_url"`
	BylawsURL           string `json:"bylaws_url"`
	LumaKickoffURL      string `json:"luma_kickoff_url"`
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
		strings.TrimSpace(req.ConfirmIntroText),
		strings.TrimSpace(req.AccountIntroText),
		strings.TrimSpace(req.MattermostIntroText),
		strings.TrimSpace(req.ContractIntroText),
		strings.TrimSpace(req.ContractURL),
		strings.TrimSpace(req.BylawsURL),
		strings.TrimSpace(req.LumaKickoffURL),
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
// kind selects which of the five onboarding emails to render; the account
// email is rendered with sample credentials, never real data; contract is
// rendered with the currently saved ContractURL/BylawsURL/LumaKickoffURL,
// since those are real admin-set links, not per-preview fakes.
//
// Also returns the button this email would actually carry — account,
// mattermost, and contract all have a fixed button once configured (see
// emailcontent.AccountButtonURL/Text, MattermostButtonText, and
// ContractURL/ContractButtonText), so the preview can show the real thing
// instead of leaving it for the caller to guess or fall back to a default.
// start and confirm return only a button text, no URL: their real ones are
// per-record portal token URLs that don't exist yet for a preview —
// landingpage-backend's proxy substitutes its own placeholder for those two.
func (h *EmailSettingsHandler) Preview(c *gin.Context) {
	var req previewEmailSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	var subject, body, buttonURL, buttonText string
	switch req.Kind {
	case "start":
		subject, body = emailcontent.BuildStart(req.IntroText, emailcontent.PreviewFirstName, emailcontent.PreviewAssignedTeam)
		buttonText = emailcontent.StartButtonText
	case "confirm":
		subject, body = emailcontent.BuildConfirm(req.IntroText, emailcontent.PreviewFirstName)
		buttonText = emailcontent.ConfirmButtonText
	case "account":
		subject, body = emailcontent.PreviewAccount(req.IntroText)
		buttonURL, buttonText = emailcontent.AccountButtonURL, emailcontent.AccountButtonText
	case "mattermost":
		subject, body = emailcontent.BuildMattermost(req.IntroText, emailcontent.PreviewFirstName)
		buttonURL, buttonText = h.cfg.MattermostURL, emailcontent.MattermostButtonText
	case "contract":
		settings, err := emailcontent.Load(h.db)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load email settings"})
			return
		}
		subject, body = emailcontent.BuildContract(req.IntroText, emailcontent.PreviewFirstName, settings.BylawsURL, settings.LumaKickoffURL)
		buttonURL, buttonText = settings.ContractURL, emailcontent.ContractButtonText
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be one of: start, confirm, account, mattermost, contract"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"subject": subject, "body": body, "button_url": buttonURL, "button_text": buttonText})
}
