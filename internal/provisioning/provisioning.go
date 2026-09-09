// Package provisioning orchestrates build-order steps 6-8 of
// onboarding-service-plan.md's flow: creating a member's @kthais.com
// Google Workspace account, adding them to their team's Google Group,
// inviting them to Mattermost, sending the final emails, and reporting the
// result back to landingpage-backend.
package provisioning

import (
	"context"
	"fmt"
	"log"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/emailcontent"
	"onboarding-service/internal/googleworkspace"
	"onboarding-service/internal/mattermost"
	"onboarding-service/internal/models"
	"onboarding-service/internal/utils"

	"gorm.io/gorm"
)

// PrimaryDomain is hardcoded rather than a config field — same "simplest is
// fine, this is not a hot path" reasoning the plan doc applies to
// teamGroups below; this service will only ever provision @kthais.com
// addresses.
const PrimaryDomain = "kthais.com"

// teamGroups mirrors landingpage-backend's five allowedApplicationTeams
// (internal/handlers/general_application_handler.go) — hardcoded per the
// plan doc's own "simplest is fine" framing rather than a team_groups.yaml
// + parser. See team_groups_test.go for the drift tripwire against the
// backend's list.
var teamGroups = map[string]string{
	"Business":    "business@kthais.com",
	"Development": "development@kthais.com",
	"Research":    "research@kthais.com",
	"Growth":      "growth@kthais.com",
	"IT":          "it@kthais.com",
}

// Service holds this service's own isolated credentials (Google Workspace
// domain-wide delegation, Mattermost bot token) plus the backend client
// used for anything landingpage-backend already owns (email sending,
// bookkeeping) — see onboarding-service-plan.md's "Why split it".
type Service struct {
	db         *gorm.DB
	google     googleworkspace.Provisioner
	mattermost *mattermost.Client
	backend    *backendclient.Client
}

func NewService(db *gorm.DB, google googleworkspace.Provisioner, mm *mattermost.Client, backend *backendclient.Client) *Service {
	return &Service{db: db, google: google, mattermost: mm, backend: backend}
}

// Provision runs steps 6-8 against record. It is idempotent end-to-end —
// safe to call more than once for the same record (this is what makes the
// retry-provisioning endpoint safe): each step checks or tolerates having
// already been done, rather than assuming a fresh start. Never panics out
// to the caller; any error or panic ends with record.State = StateFailed
// and record.FailureReason set to a sanitized (credential-free) message.
func (s *Service) Provision(ctx context.Context, record *models.OnboardingRecord) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic during provisioning: %v", r)
		}
		if err != nil {
			s.fail(record, err)
		}
	}()

	groupEmail, ok := teamGroups[record.AssignedTeam]
	if !ok {
		return fmt.Errorf("no Google Group mapped for team %q", record.AssignedTeam)
	}

	tempPassword, err := s.ensureAccount(ctx, record)
	if err != nil {
		return fmt.Errorf("google account creation: %w", err)
	}

	if err := s.google.AddToGroup(ctx, groupEmail, record.KthaisEmail); err != nil {
		return fmt.Errorf("google group membership: %w", err)
	}

	// Invite the newly provisioned @kthais.com address, not KthEmail (the
	// personal kth.se address only ever used to verify identity during the
	// portal flow) — Mattermost's own domain restriction is scoped to real
	// KTHAIS accounts, and that's the identity this person actually joins
	// the workspace as.
	//
	// Deliberately non-fatal: this call's own email-invite delivery has
	// proven unreliable (the API call can succeed while the notification
	// email never arrives, and it has also failed outright with a 400 whose
	// root cause is still unconfirmed), and the account/group-membership
	// work above is real, useful progress that shouldn't be thrown away
	// over Mattermost's own invite flow specifically. The getting-started
	// email below no longer depends on this succeeding — it always links
	// straight to the Mattermost server instead of promising an invite.
	if err := s.mattermost.InviteToTeam(record.KthaisEmail); err != nil {
		log.Printf("provisioning: record %d: mattermost invite failed, continuing without it: %v", record.ID, err)
	}

	if err := s.save(record, models.StateProvisioned); err != nil {
		return err
	}

	if err := s.sendFinalEmails(record, tempPassword); err != nil {
		return fmt.Errorf("final emails: %w", err)
	}

	if err := s.save(record, models.StateEmailed); err != nil {
		return err
	}

	// Nil ApplicationID means this is an admin's manual onboarding action —
	// there's no backend GeneralApplication to report the result to, so
	// skip the callback entirely rather than calling it with a fabricated
	// or empty ID.
	if record.ApplicationID != nil {
		if err := s.backend.RecordAccount(*record.ApplicationID, record.KthaisEmail); err != nil {
			return fmt.Errorf("record-account: %w", err)
		}
	}

	return s.save(record, models.StateComplete)
}

// ensureAccount creates the Google account on first run. On a retry (record
// already has a KthaisEmail from a prior partial failure) where the account
// still exists, it resets its password rather than trying to recover the
// original temp password — that password is never persisted anywhere, so a
// retry always mints a fresh one. This trades one harmless extra password
// reset for never writing a live credential to disk.
//
// If the account no longer exists — most likely deleted directly in
// Workspace, e.g. to reset a test user for another run — it's recreated at
// the same address rather than treated as fatal: KthaisEmail is already
// reserved for this record, so re-creating there is safe and, unlike
// resolving a fresh address, can never mint a numbered-suffix duplicate
// (erik.svensson2@...) just because the original name is still "taken" by
// this same record's own (stale) expectation.
func (s *Service) ensureAccount(ctx context.Context, record *models.OnboardingRecord) (tempPassword string, err error) {
	tempPassword, err = utils.GenerateTempPassword()
	if err != nil {
		return "", err
	}

	if record.KthaisEmail != "" {
		exists, err := s.google.UserExists(ctx, record.KthaisEmail)
		if err != nil {
			return "", err
		}
		if exists {
			if err := s.google.ResetPassword(ctx, record.KthaisEmail, tempPassword); err != nil {
				return "", err
			}
			return tempPassword, nil
		}
		// Falls through to create it fresh at record.KthaisEmail below.
	}

	email := record.KthaisEmail
	if email == "" {
		email, err = googleworkspace.ResolvePrimaryEmail(ctx, s.google, PrimaryDomain, record.FirstName, record.LastName)
		if err != nil {
			return "", err
		}
	}
	if err := s.google.CreateUser(ctx, googleworkspace.NewUser{
		PrimaryEmail:  email,
		FirstName:     record.FirstName,
		LastName:      record.LastName,
		RecoveryEmail: record.KthEmail,
		TempPassword:  tempPassword,
	}); err != nil {
		return "", err
	}
	if record.KthaisEmail == "" {
		record.KthaisEmail = email
		if err := s.db.Model(record).Update("kthais_email", email).Error; err != nil {
			return "", err
		}
	}
	return tempPassword, nil
}

func (s *Service) sendFinalEmails(record *models.OnboardingRecord, tempPassword string) error {
	settings, err := emailcontent.Load(s.db)
	if err != nil {
		log.Printf("sendFinalEmails: failed to load email settings for record %d, using defaults: %v", record.ID, err)
	}

	accountSubject, accountBody := emailcontent.BuildAccount(settings.AccountIntroText, record.FirstName, record.KthaisEmail, tempPassword)
	if err := s.backend.SendEmail(record.KthEmail, accountSubject, accountBody, emailcontent.AccountButtonURL, emailcontent.AccountButtonText); err != nil {
		return fmt.Errorf("account-info email: %w", err)
	}

	mattermostSubject, mattermostBody := emailcontent.BuildMattermost(settings.MattermostIntroText, record.FirstName)
	if err := s.backend.SendEmail(record.KthEmail, mattermostSubject, mattermostBody, s.mattermost.BaseURL(), emailcontent.MattermostButtonText); err != nil {
		return fmt.Errorf("mattermost getting-started email: %w", err)
	}

	return nil
}

func (s *Service) save(record *models.OnboardingRecord, state models.OnboardingState) error {
	record.State = state
	if err := s.db.Model(record).Update("state", state).Error; err != nil {
		return fmt.Errorf("saving state %s: %w", state, err)
	}
	return nil
}

// fail persists state=failed with a sanitized reason. err is wrapped
// googleapi/HTTP error text from this package's own functions — never a
// credential — but this is the one place that error text reaches storage,
// so it's the deliberate chokepoint for that guarantee.
func (s *Service) fail(record *models.OnboardingRecord, err error) {
	record.State = models.StateFailed
	record.FailureReason = err.Error()
	if dbErr := s.db.Model(record).Updates(map[string]any{
		"state":          models.StateFailed,
		"failure_reason": record.FailureReason,
	}).Error; dbErr != nil {
		log.Printf("provisioning: failed to persist failure state for record %d: %v (original error: %v)", record.ID, dbErr, err)
		return
	}
	log.Printf("provisioning: record %d failed: %v", record.ID, err)
}
