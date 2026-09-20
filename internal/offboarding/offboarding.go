// Package offboarding is the reverse of internal/provisioning: given a
// @kthais.com email address, it suspends or permanently deletes the
// Google Workspace account and the Mattermost account that belong to it,
// and removes the member from Luma's "Members" tier. Deliberately
// independent of internal/provisioning and of models.OnboardingRecord —
// this must work for any real member, including ones onboarded long before
// this whole system existed and who therefore have no OnboardingRecord row
// at all. The email address alone is enough to act against all three
// systems.
package offboarding

import (
	"context"
	"errors"
	"fmt"

	"onboarding-service/internal/backendclient"
	"onboarding-service/internal/googleworkspace"
	"onboarding-service/internal/mattermost"
)

type Service struct {
	google     googleworkspace.Deprovisioner
	mattermost *mattermost.Client
	backend    *backendclient.Client
}

func NewService(google googleworkspace.Deprovisioner, mm *mattermost.Client, backend *backendclient.Client) *Service {
	return &Service{google: google, mattermost: mm, backend: backend}
}

// Deactivate suspends the Google Workspace account, deactivates the
// Mattermost account, and removes the Luma Members tier membership for
// email. All three steps are attempted even if one fails, and every error
// (if any) is returned together via errors.Join — a failure in one system
// must never silently mask what happened in the others. Google and
// Mattermost are reversible on both sides from each system's own admin
// console; the Luma removal is a status change (see
// backendclient.Client.RemoveFromLumaMembers), also admin-reversible from
// Luma's own dashboard.
func (s *Service) Deactivate(ctx context.Context, email string) error {
	var errs []error
	if err := s.google.SuspendUser(ctx, email); err != nil {
		errs = append(errs, fmt.Errorf("google: %w", err))
	}
	if err := s.mattermost.DeactivateUser(email); err != nil {
		errs = append(errs, fmt.Errorf("mattermost: %w", err))
	}
	if err := s.backend.RemoveFromLumaMembers(email); err != nil {
		errs = append(errs, fmt.Errorf("luma: %w", err))
	}
	return errors.Join(errs...)
}

// Delete permanently deletes the Google Workspace account, attempts to
// permanently delete the Mattermost account, and removes the Luma Members
// tier membership for email. Cannot be undone from this system for Google;
// see mattermost.Client.DeleteUserPermanently's own doc comment for why the
// Mattermost half may fail given this bot's deliberately limited (Team
// Admin, not System Admin) role. Every failure is reported like any other,
// never silently swallowed, so the caller still knows exactly which of the
// three succeeded and which need manual follow-up.
func (s *Service) Delete(ctx context.Context, email string) error {
	var errs []error
	if err := s.google.DeleteUser(ctx, email); err != nil {
		errs = append(errs, fmt.Errorf("google: %w", err))
	}
	if err := s.mattermost.DeleteUserPermanently(email); err != nil {
		errs = append(errs, fmt.Errorf("mattermost: %w", err))
	}
	if err := s.backend.RemoveFromLumaMembers(email); err != nil {
		errs = append(errs, fmt.Errorf("luma: %w", err))
	}
	return errors.Join(errs...)
}
