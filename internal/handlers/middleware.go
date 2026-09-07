package handlers

import (
	"crypto/subtle"
	"net/http"

	"onboarding-service/internal/config"

	"github.com/gin-gonic/gin"
)

// requireServiceSecret rejects before touching any handler logic if the
// caller doesn't present the exact shared secret — mirrors
// landingpage-backend's own onboarding-service auth check
// (OnboardingHandler.requireOnboardingServiceSecret) so both sides fail
// closed the same way. An unset secret must never be treated as "no secret
// required". Shared by every internal (backend-to-this-service) endpoint.
func requireServiceSecret(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := c.GetHeader("X-Service-Secret")
		if cfg.OnboardingServiceSecret == "" || provided == "" ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.OnboardingServiceSecret)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}
