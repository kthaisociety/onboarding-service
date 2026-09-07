package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/config"
	"onboarding-service/internal/googleworkspace"
	"onboarding-service/internal/handlers"
	"onboarding-service/internal/mattermost"
	"onboarding-service/internal/models"
	"onboarding-service/internal/provisioning"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/joho/godotenv"
	"gorm.io/gorm"
)

func main() {
	// Best-effort — fine if it doesn't exist (e.g. real env vars set
	// directly in the container), same convention as landingpage-backend.
	_ = godotenv.Load()

	cfg := config.LoadConfig()
	if cfg.OnboardingServiceSecret == "" {
		log.Fatal("ONBOARDING_SERVICE_SECRET must be set")
	}
	if cfg.BackendURL == "" {
		log.Fatal("BACKEND_URL must be set")
	}
	if cfg.GoogleServiceAccountJSON == "" {
		log.Fatal("GOOGLE_ADMIN_SERVICE_ACCOUNT_JSON (or _PATH) must be set")
	}
	if cfg.GoogleImpersonateAs == "" {
		log.Fatal("GOOGLE_ADMIN_IMPERSONATE_AS must be set")
	}
	if cfg.MattermostURL == "" {
		log.Fatal("MATTERMOST_URL must be set")
	}
	if cfg.MattermostBotToken == "" {
		log.Fatal("MATTERMOST_BOT_TOKEN must be set")
	}
	if cfg.MattermostTeamID == "" {
		log.Fatal("MATTERMOST_TEAM_ID must be set")
	}

	db, err := gorm.Open(sqlite.Open(cfg.DBPath), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	if err := db.AutoMigrate(&models.OnboardingRecord{}, &models.OnboardingToken{}); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	backend := backendclient.New(cfg)

	// Fatal on failure, unlike the backend check below: a broken Google
	// credential means this service cannot do the one thing it exists for.
	// Construction itself makes no network call (the JWT exchange is lazy),
	// so this doesn't slow down or risk boot.
	googleCtx, cancelGoogle := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelGoogle()
	googleClient, err := googleworkspace.NewClient(googleCtx, []byte(cfg.GoogleServiceAccountJSON), cfg.GoogleImpersonateAs)
	if err != nil {
		log.Fatalf("failed to initialize Google Workspace client: %v", err)
	}

	mattermostClient := mattermost.New(cfg)
	// Non-fatal, logged only — same idiom as checkBackendConnectivity below.
	if err := mattermostClient.Ping(); err != nil {
		log.Printf("startup check: could not reach Mattermost at %s: %v", cfg.MattermostURL, err)
	} else {
		log.Printf("startup check: Mattermost at %s reachable", cfg.MattermostURL)
	}

	provisioningService := provisioning.NewService(db, googleClient, mattermostClient, backend)

	// One-shot reachability check against the backend, logged at boot only —
	// not a recurring healthcheck. Purpose is to surface a misconfigured
	// BACKEND_URL (wrong internal DNS name, wrong port) immediately in the
	// deploy logs rather than silently at the first real onboarding, since
	// notifyOnboardingService-style calls to this service are fire-and-forget
	// by design. Deliberately non-fatal: this service has no hard runtime
	// dependency on the backend being reachable at any given instant.
	checkBackendConnectivity(cfg.BackendURL)

	r := gin.Default()
	// This service never reads c.ClientIP() (checked: no call sites), so
	// there's nothing relying on forwarded-header trust today — but Gin's
	// default trusts every proxy for X-Forwarded-For, which is a footgun for
	// whoever adds IP-based logic later. Lock it down now rather than leave
	// the insecure default in place.
	if err := r.SetTrustedProxies(nil); err != nil {
		log.Fatalf("failed to set trusted proxies: %v", err)
	}
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "onboarding-service"})
	})

	api := r.Group("/")
	handlers.NewNotifyHandler(db, cfg, backend).Register(api)
	handlers.NewPortalHandler(db, cfg, backend, provisioningService).Register(api)
	handlers.NewRetryHandler(db, cfg, provisioningService).Register(api)

	addr := cfg.Host + ":" + cfg.Port
	log.Printf("onboarding-service listening on %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func checkBackendConnectivity(backendURL string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(backendURL + "/health")
	if err != nil {
		log.Printf("startup check: could not reach backend at %s/health: %v", backendURL, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("startup check: backend at %s/health returned %d", backendURL, resp.StatusCode)
		return
	}
	log.Printf("startup check: backend at %s/health reachable (200 OK)", backendURL)
}
