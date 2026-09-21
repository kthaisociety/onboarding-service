// Package googleworkspace provisions Google Workspace accounts via the
// Admin SDK Directory API, authenticated through domain-wide delegation.
// This is one of the two most powerful credentials the whole system holds
// (see onboarding-service-plan.md, "Why split it") — scopes are kept to
// exactly what provisioning needs, never a broader admin.directory wildcard.
package googleworkspace

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/oauth2/google"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// NewUser is the input to CreateUser. TempPassword is held in memory by the
// caller only — see internal/provisioning for why it's never persisted.
type NewUser struct {
	PrimaryEmail  string
	FirstName     string
	LastName      string
	RecoveryEmail string
	TempPassword  string
}

// Provisioner is the seam tests fake — the real implementation wraps the
// Admin SDK Directory API; tests use a map-backed fake, never a real
// credential or network call.
type Provisioner interface {
	UserExists(ctx context.Context, primaryEmail string) (bool, error)
	CreateUser(ctx context.Context, in NewUser) error
	ResetPassword(ctx context.Context, primaryEmail, tempPassword string) error
	AddToGroup(ctx context.Context, groupEmail, memberEmail string) error
}

// Deprovisioner is the seam internal/offboarding tests fake — the
// opposite half of Provisioner, for suspending/deleting an account rather
// than creating one. A separate interface (not folded into Provisioner)
// since it has its own, unrelated caller.
type Deprovisioner interface {
	SuspendUser(ctx context.Context, primaryEmail string) error
	DeleteUser(ctx context.Context, primaryEmail string) error
}

// Client is both halves — provisioning and deprovisioning — of what this
// package can do against a real Workspace account. NewClient returns this
// so either narrower interface can be used without a type assertion; the
// concrete client struct satisfies both.
type Client interface {
	Provisioner
	Deprovisioner
}

type client struct {
	svc *admin.Service
}

// NewClient authenticates via domain-wide delegation: serviceAccountJSON is
// signed to assert this service account acts as impersonateAs (a Workspace
// super-admin), scoped to exactly admin.directory.user and
// admin.directory.group.member. Does not itself make a network call (the
// JWT exchange happens lazily on first use), so it's safe to call at boot
// even if the network is briefly unavailable.
func NewClient(ctx context.Context, serviceAccountJSON []byte, impersonateAs string) (Client, error) {
	if impersonateAs == "" {
		return nil, fmt.Errorf("impersonateAs must not be empty")
	}

	cfg, err := google.JWTConfigFromJSON(serviceAccountJSON,
		admin.AdminDirectoryUserScope,
		admin.AdminDirectoryGroupMemberScope,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Google service account JSON: %w", err)
	}
	cfg.Subject = impersonateAs

	svc, err := admin.NewService(ctx, option.WithHTTPClient(cfg.Client(ctx)))
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Admin SDK Directory service: %w", err)
	}

	return &client{svc: svc}, nil
}

func (c *client) UserExists(ctx context.Context, primaryEmail string) (bool, error) {
	_, err := c.svc.Users.Get(primaryEmail).Context(ctx).Do()
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking whether %s exists: %w", primaryEmail, err)
}

func (c *client) CreateUser(ctx context.Context, in NewUser) error {
	user := &admin.User{
		PrimaryEmail: in.PrimaryEmail,
		Name: &admin.UserName{
			GivenName:  in.FirstName,
			FamilyName: in.LastName,
		},
		Password:                  in.TempPassword,
		ChangePasswordAtNextLogin: true,
		RecoveryEmail:             in.RecoveryEmail,
	}
	if _, err := c.svc.Users.Insert(user).Context(ctx).Do(); err != nil {
		return fmt.Errorf("creating user %s: %w", in.PrimaryEmail, err)
	}
	return nil
}

func (c *client) ResetPassword(ctx context.Context, primaryEmail, tempPassword string) error {
	update := &admin.User{
		Password:                  tempPassword,
		ChangePasswordAtNextLogin: true,
	}
	if _, err := c.svc.Users.Update(primaryEmail, update).Context(ctx).Do(); err != nil {
		return fmt.Errorf("resetting password for %s: %w", primaryEmail, err)
	}
	return nil
}

// AddToGroup treats "already a member" as success so a retry after a
// partial failure never fails on work it already did.
func (c *client) AddToGroup(ctx context.Context, groupEmail, memberEmail string) error {
	member := &admin.Member{Email: memberEmail, Role: "MEMBER"}
	_, err := c.svc.Members.Insert(groupEmail, member).Context(ctx).Do()
	if err == nil || isDuplicate(err) {
		return nil
	}
	return fmt.Errorf("adding %s to group %s: %w", memberEmail, groupEmail, err)
}

// SuspendUser suspends (not deletes) the Workspace account for
// primaryEmail — reversible from the Admin Console at any time. Uses the
// same admin.directory.user scope already granted for provisioning, so
// this needs no new Google Cloud Console authorization.
//
// Checks the current state first rather than just calling Update and
// inspecting the error: Google's Admin SDK returns a 403 (not a clean,
// checkable "already suspended" error) for re-suspending an account that's
// already suspended, observed in production against a real member's
// account. Reading the state first turns that into a clean no-op instead
// of a surfaced failure, matching the "nothing to do here is still
// success" convention offboarding.Service applies uniformly across all
// three systems (Mattermost lookup 404, Luma no-membership) — without it,
// Deactivate would report an already-suspended member as a Google failure
// forever and never mark them deactivated locally, even though Google's
// side is already exactly the state Deactivate is trying to reach. Also
// treats "no such user" as success, same idempotency reasoning as
// DeleteUser below: an account already gone from Google (deleted directly
// from the Admin Console, or by a prior offboarding.Service.Delete call)
// must not keep failing the Google leg forever either.
//
// The initial Get and the Update below aren't atomic, so a concurrent
// Deactivate call (or an admin suspending the same account by hand) can
// suspend the account in between — Update then fails with the same
// "already suspended" 403 the pre-check exists to avoid. Rather than
// pattern-matching that specific error, a failed Update re-checks the
// account's state directly and treats "it's suspended now" as success
// regardless of why Update failed, same principle applied twice.
func (c *client) SuspendUser(ctx context.Context, primaryEmail string) error {
	existing, err := c.svc.Users.Get(primaryEmail).Context(ctx).Do()
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("checking suspension state for %s: %w", primaryEmail, err)
	}
	if existing.Suspended {
		return nil
	}

	update := &admin.User{Suspended: true}
	if _, err := c.svc.Users.Update(primaryEmail, update).Context(ctx).Do(); err != nil {
		if isNotFound(err) {
			return nil
		}
		if current, getErr := c.svc.Users.Get(primaryEmail).Context(ctx).Do(); getErr == nil && current.Suspended {
			return nil
		}
		return fmt.Errorf("suspending user %s: %w", primaryEmail, err)
	}
	return nil
}

// DeleteUser permanently deletes the Workspace account for primaryEmail.
// Cannot be undone from this system — Google itself retains a 20-day
// recovery window in the Admin Console independent of anything here.
// Treats "already gone" as success (same idempotency reasoning as
// AddToGroup above) so a retry after a partial failure — e.g. the Google
// half succeeding but the Mattermost half failing — never reports the
// already-completed Google step as an error the second time around.
func (c *client) DeleteUser(ctx context.Context, primaryEmail string) error {
	if err := c.svc.Users.Delete(primaryEmail).Context(ctx).Do(); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleting user %s: %w", primaryEmail, err)
	}
	return nil
}

func isNotFound(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == 404
}

func isDuplicate(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && (apiErr.Code == 409 || apiErr.Code == 400)
}
