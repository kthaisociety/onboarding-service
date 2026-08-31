package main

import (
	"log"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/config"
	"onboarding-service/internal/handlers"
	"onboarding-service/internal/models"

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

	db, err := gorm.Open(sqlite.Open(cfg.DBPath), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	if err := db.AutoMigrate(&models.OnboardingRecord{}, &models.OnboardingToken{}); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	backend := backendclient.New(cfg)

	r := gin.Default()
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "onboarding-service"})
	})

	api := r.Group("/")
	handlers.NewNotifyHandler(db, cfg, backend).Register(api)
	handlers.NewPortalHandler(db, cfg, backend).Register(api)

	addr := cfg.Host + ":" + cfg.Port
	log.Printf("onboarding-service listening on %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
