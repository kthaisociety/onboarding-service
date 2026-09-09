// Package emailcontent builds the three emails sent over the course of an
// onboarding (start, account-credentials, Mattermost getting-started) and
// owns the admin-editable settings behind them. It lives on its own,
// separate from both internal/handlers and internal/provisioning, since
// both of those need it and handlers already imports provisioning — put
// this here instead of in either and there's no cycle.
package emailcontent

import (
	"strings"

	"onboarding-service/internal/models"

	"gorm.io/gorm"
)

// PreviewFirstName is a sample name for admin-panel previews, matching
// landingpage-backend's own "Alex"/"Jones" convention for its
// interview-invite preview.
const PreviewFirstName = "Alex"

// previewKthaisEmail/previewTempPassword are fake sample values used only
// when rendering an admin preview of the account-credentials email — never
// real data.
const (
	previewKthaisEmail  = "alex.jones@kthais.com"
	previewTempPassword = "Xy7#kP2m9Qw!"
)

const StartSubject = "Welcome to KTH AI Society"
const DefaultStartIntro = "Congratulations on being accepted to KTH AI Society!"

// StartButtonText labels the start email's button. Not paired with a
// StartButtonURL constant — like ConfirmButtonText, that link is a
// per-record portal token, not a fixed value.
const StartButtonText = "Start onboarding"

const ConfirmSubject = "Confirm your KTH email"
const DefaultConfirmIntro = "Please confirm this is your KTH email address to continue setting up your KTH AI Society account."

// ConfirmButtonText labels the confirm email's button. Not paired with a
// ConfirmButtonURL constant like Account/Mattermost get — that link is a
// per-record portal token (see PortalHandler.SubmitEmail), so it doesn't
// exist as a fixed value; the admin-panel preview substitutes its own
// placeholder the same way it already does for the start email's button.
const ConfirmButtonText = "Continue to confirm"

const AccountSubject = "Your KTH AI Society account"

// AccountButtonURL/AccountButtonText take the recipient straight to Google's
// sign-in flow so they can activate the new account, rather than the
// generic "Contact us" fallback every other onboarding email gets. Not
// admin-editable — same reasoning as the credentials list in BuildAccount
// below, this is something every recipient needs regardless of what an
// admin's intro text says.
const AccountButtonURL = "https://accounts.google.com/"
const AccountButtonText = "Sign in with Google"

// PasskeySetupURL is Google's own account-security page for managing
// passkeys — mentioned as a plain-text link in BuildAccount's body (not a
// second button; the template only has one button slot, reserved for
// activating the account itself) since most email clients auto-linkify a
// bare URL even without an anchor tag.
const PasskeySetupURL = "https://myaccount.google.com/signinoptions/passkeys"

const MattermostSubject = "Getting started with Mattermost"

// MattermostButtonText labels the button that points at the Mattermost
// server itself (provisioning.Service passes the actual URL, since that's
// already the configured MattermostURL — no need to duplicate it here).
const MattermostButtonText = "Open Mattermost"

// DefaultMattermostIntro deliberately does not promise an invite email:
// Mattermost's own email-invite delivery has proven unreliable in practice
// (the "mattermost invite" API call can succeed while the notification
// email itself never arrives), so this points the recipient straight at the
// server instead of telling them to wait for something that may not show
// up. Says "kthais.com account", not "your account": this email is sent to
// the kth.se address (the only inbox reachable before the
// account-credentials email above gives them kthais.com access), but the
// sign-in identity is the new @kthais.com address.
const DefaultMattermostIntro = "You've been added to the KTH AI Society Mattermost workspace. Click the button below and sign in with your new @kthais.com account to get started."

// substitute replaces the one placeholder admin-edited intro text may
// contain — plain string substitution, never executed as a template, since
// this text is saved by an admin, not a developer.
func substitute(text, firstName string) string {
	return strings.ReplaceAll(text, "{{first_name}}", firstName)
}

// BuildStart composes the "start your onboarding" email body from an
// admin-editable intro paragraph plus the fixed greeting and next-steps
// list every recipient needs, regardless of what the intro says.
func BuildStart(introText, firstName string) (subject, body string) {
	if strings.TrimSpace(introText) == "" {
		introText = DefaultStartIntro
	}
	intro := substitute(introText, firstName)

	body = "Hi " + firstName + ",\n\n" +
		intro + "\n\n" +
		"To get started:\n" +
		"1. Click the button below to open the onboarding portal\n" +
		"2. Enter your kth.se email address\n" +
		"3. Confirm your kth.se address via the link we send you\n" +
		"4. We'll set up your kthais.com account and Mattermost access and email you the details"
	return StartSubject, body
}

// BuildConfirm composes the "confirm your KTH email" email from an
// admin-editable intro paragraph plus a fixed trailing explanation of why
// the flow needs a second click on the page the button opens — that
// explanation describes real security behavior (defending against mail
// scanners prefetching the link), so it's never admin-editable, same
// reasoning as BuildAccount's credentials list below.
func BuildConfirm(introText, firstName string) (subject, body string) {
	if strings.TrimSpace(introText) == "" {
		introText = DefaultConfirmIntro
	}
	intro := substitute(introText, firstName)

	body = "Hi " + firstName + ",\n\n" +
		intro + "\n\n" +
		"Click below, then confirm once more on the page that opens — that extra click keeps your account " +
		"safe from automated email link scanners."
	return ConfirmSubject, body
}

// BuildAccount composes the account-credentials email. introText is an
// optional personal note before the credentials; unlike the other two
// emails it has no non-empty default — the credentials list below it is
// the one part of this email every recipient actually needs, and it's
// always appended in code, never something admin-edited text can replace
// or push out.
func BuildAccount(introText, firstName, kthaisEmail, tempPassword string) (subject, body string) {
	var lead string
	if trimmed := strings.TrimSpace(introText); trimmed != "" {
		lead = substitute(trimmed, firstName) + "\n\n"
	}

	body = "Hi " + firstName + ",\n\n" +
		lead +
		"Here is your KTH AI Society account:\n" +
		"1. Email: " + kthaisEmail + "\n" +
		"2. Temporary password: " + tempPassword + "\n" +
		"3. You'll be asked to set a new password the first time you log in\n" +
		"4. Click the button below to sign in with Google and activate your account\n" +
		"5. Once signed in, we recommend setting up a passkey for faster, more secure sign-in at " + PasskeySetupURL
	return AccountSubject, body
}

// PreviewAccount renders BuildAccount with sample credentials — never real
// data — for the admin panel's preview.
func PreviewAccount(introText string) (subject, body string) {
	return BuildAccount(introText, PreviewFirstName, previewKthaisEmail, previewTempPassword)
}

// BuildMattermost composes the Mattermost getting-started email.
func BuildMattermost(introText, firstName string) (subject, body string) {
	if strings.TrimSpace(introText) == "" {
		introText = DefaultMattermostIntro
	}
	intro := substitute(introText, firstName)

	body = "Hi " + firstName + ",\n\n" + intro
	return MattermostSubject, body
}

// Load returns the current settings, or a zero-valued struct if none has
// ever been saved — callers must fall back to each Build function's own
// default themselves, so a database with no row behaves exactly as it did
// before this feature existed.
func Load(db *gorm.DB) (models.OnboardingEmailSettings, error) {
	var settings models.OnboardingEmailSettings
	err := db.First(&settings).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return settings, err
	}
	return settings, nil
}

// Save creates the singleton row on first save, or updates it thereafter.
func Save(db *gorm.DB, startIntro, confirmIntro, accountIntro, mattermostIntro, updatedByEmail string) (models.OnboardingEmailSettings, error) {
	settings, err := Load(db)
	if err != nil {
		return settings, err
	}
	settings.StartIntroText = startIntro
	settings.ConfirmIntroText = confirmIntro
	settings.AccountIntroText = accountIntro
	settings.MattermostIntroText = mattermostIntro
	settings.UpdatedByEmail = updatedByEmail

	if settings.ID == 0 {
		err = db.Create(&settings).Error
	} else {
		err = db.Model(&settings).Updates(map[string]any{
			"start_intro_text":      startIntro,
			"confirm_intro_text":    confirmIntro,
			"account_intro_text":    accountIntro,
			"mattermost_intro_text": mattermostIntro,
			"updated_by_email":      updatedByEmail,
		}).Error
	}
	return settings, err
}
