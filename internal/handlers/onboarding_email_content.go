package handlers

import (
	"strings"

	"onboarding-service/internal/models"

	"gorm.io/gorm"
)

// startOnboardingSubject is fixed — only the intro paragraph is
// admin-editable, see buildStartOnboardingEmail.
const startOnboardingSubject = "Welcome to KTH AI Society"

const defaultStartOnboardingIntro = "Congratulations on being accepted to KTH AI Society!"

// buildStartOnboardingEmail composes the "start your onboarding" email body
// from an admin-editable intro paragraph plus the fixed greeting and
// next-steps list every recipient needs, regardless of what the intro says.
// introText may contain a {{first_name}} placeholder, replaced by plain
// string substitution — never executed as a template, matching the
// {{first_name}}-placeholder convention used for the interview invite and
// Team Questions templates in landingpage-backend.
func buildStartOnboardingEmail(introText, firstName string) (subject, body string) {
	if strings.TrimSpace(introText) == "" {
		introText = defaultStartOnboardingIntro
	}
	intro := strings.ReplaceAll(introText, "{{first_name}}", firstName)

	body = "Hi " + firstName + ",\n\n" +
		intro + "\n\n" +
		"To get started:\n" +
		"1. Click the button below to open the onboarding portal\n" +
		"2. Enter your kth.se email address\n" +
		"3. Confirm your kth.se address via the link we send you\n" +
		"4. We'll set up your kthais.com account and Mattermost access and email you the details"
	return startOnboardingSubject, body
}

// loadEmailSettings returns the current settings, or a zero-valued struct
// if none has ever been saved — callers must fall back to
// defaultStartOnboardingIntro themselves (see buildStartOnboardingEmail),
// so a database with no row behaves exactly as it did before this feature
// existed.
func loadEmailSettings(db *gorm.DB) (models.OnboardingEmailSettings, error) {
	var settings models.OnboardingEmailSettings
	err := db.First(&settings).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return settings, err
	}
	return settings, nil
}

// saveEmailSettings creates the singleton row on first save, or updates it
// thereafter.
func saveEmailSettings(db *gorm.DB, introText, updatedByEmail string) (models.OnboardingEmailSettings, error) {
	settings, err := loadEmailSettings(db)
	if err != nil {
		return settings, err
	}
	settings.IntroText = introText
	settings.UpdatedByEmail = updatedByEmail
	if settings.ID == 0 {
		err = db.Create(&settings).Error
	} else {
		err = db.Model(&settings).Updates(map[string]any{
			"intro_text":       introText,
			"updated_by_email": updatedByEmail,
		}).Error
	}
	return settings, err
}

// introOrDefault is what any reader (the real sender, the GET endpoint) sees
// when no admin-set intro is saved yet.
func introOrDefault(introText string) string {
	if strings.TrimSpace(introText) == "" {
		return defaultStartOnboardingIntro
	}
	return introText
}
