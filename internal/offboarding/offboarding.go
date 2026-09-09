// Package offboarding is the reverse of internal/provisioning: given a
// @kthais.com email address, it suspends or permanently deletes the
// Google Workspace account and the Mattermost account that belong to it.
// Deliberately independent of internal/provisioning and of
// models.OnboardingRecord — this must work for any real member, including
// ones onboarded long before this whole system existed and who therefore
// have no OnboardingRecord row at all. The email address alone is enough
// to act against both external systems.
package offboarding

import (
	"context"
	"errors"
	"fmt"

	"onboarding-service/internal/googleworkspace"
	"onboarding-service/internal/mattermost"
)

type Service struct {
	google     googleworkspace.Deprovisioner
	mattermost *mattermost.Client
}

func NewService(google googleworkspace.Deprovisioner, mm *mattermost.Client) *Service {
	return &Service{google: google, mattermost: mm}
}

// Deactivate suspends the Google Workspace account and deactivates the
// Mattermost account for email. Both steps are attempted even if one
// fails, and both errors (if any) are returned together via errors.Join —
// a failure in one system must never silently mask what happened in the
// other. Reversible on both sides from each system's own admin console.
func (s *Service) Deactivate(ctx context.Context, email string) error {
	var errs []error
	if err := s.google.SuspendUser(ctx, email); err != nil {
		errs = append(errs, fmt.Errorf("google: %w", err))
	}
	if err := s.mattermost.DeactivateUser(email); err != nil {
		errs = append(errs, fmt.Errorf("mattermost: %w", err))
	}
	return errors.Join(errs...)
}

// Delete permanently deletes the Google Workspace account and attempts to
// permanently delete the Mattermost account for email. Cannot be undone
// from this system. See mattermost.Client.DeleteUserPermanently's own doc
// comment for why the Mattermost half may fail given this bot's
// deliberately limited (Team Admin, not System Admin) role — that failure
// is reported like any other, never silently swallowed, so the caller
// still knows the Google side succeeded and Mattermost needs manual
// follow-up.
func (s *Service) Delete(ctx context.Context, email string) error {
	var errs []error
	if err := s.google.DeleteUser(ctx, email); err != nil {
		errs = append(errs, fmt.Errorf("google: %w", err))
	}
	if err := s.mattermost.DeleteUserPermanently(email); err != nil {
		errs = append(errs, fmt.Errorf("mattermost: %w", err))
	}
	return errors.Join(errs...)
}
