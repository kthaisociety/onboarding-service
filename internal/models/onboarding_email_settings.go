package models

import "gorm.io/gorm"

// OnboardingEmailSettings is a singleton row (the first/only one ever
// created) holding the admin-editable intro text of each of the three
// emails sent over the course of an onboarding. The greeting, numbered
// next-steps list, credentials block, and sign-off are always appended in
// code (see internal/emailcontent) — only the paragraph in between is
// editable, so an admin can add a personal touch without being able to
// break the parts every recipient needs to see.
type OnboardingEmailSettings struct {
	gorm.Model
	// StartIntroText is the "start your onboarding" email's intro.
	StartIntroText string `gorm:"type:text;not null;default:''"`
	// AccountIntroText is an optional note before the account-credentials
	// email's fixed credentials list.
	AccountIntroText string `gorm:"type:text;not null;default:''"`
	// MattermostIntroText is the Mattermost getting-started email's message.
	MattermostIntroText string `gorm:"type:text;not null;default:''"`
	UpdatedByEmail      string `gorm:"type:text;not null;default:''"`
}
