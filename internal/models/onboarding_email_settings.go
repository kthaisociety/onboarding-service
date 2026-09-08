package models

import "gorm.io/gorm"

// OnboardingEmailSettings is a singleton row (the first/only one ever
// created) holding the admin-editable intro paragraph of the "start your
// onboarding" email. The greeting, numbered next-steps list, and sign-off
// are always appended in code (see handlers.buildStartOnboardingEmail) —
// only the paragraph in between is editable, so an admin can add a personal
// touch without being able to break the parts every recipient needs to see.
type OnboardingEmailSettings struct {
	gorm.Model
	IntroText      string `gorm:"type:text;not null;default:''"`
	UpdatedByEmail string `gorm:"type:text;not null;default:''"`
}
