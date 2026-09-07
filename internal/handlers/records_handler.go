package handlers

import (
	"net/http"

	"onboarding-service/internal/config"
	"onboarding-service/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// RecordsHandler exposes onboarding-service's own records to
// landingpage-backend so admins can see who's been sent an onboarding link
// and hasn't finished it. onboarding-service is the source of truth for
// this data — the backend proxies this read-only, never persists it.
type RecordsHandler struct {
	db  *gorm.DB
	cfg *config.Config
}

func NewRecordsHandler(db *gorm.DB, cfg *config.Config) *RecordsHandler {
	return &RecordsHandler{db: db, cfg: cfg}
}

func (h *RecordsHandler) Register(r *gin.RouterGroup) {
	r.GET("/internal/onboarding/records", requireServiceSecret(h.cfg), h.List)
}

// List returns every OnboardingRecord, newest first. No pagination — this
// service sees a few dozen onboardings per recruiting cycle at most.
func (h *RecordsHandler) List(c *gin.Context) {
	var records []models.OnboardingRecord
	if err := h.db.Order("created_at DESC").Find(&records).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	c.JSON(http.StatusOK, records)
}
