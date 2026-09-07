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

type client struct {
	svc *admin.Service
}

// NewClient authenticates via domain-wide delegation: serviceAccountJSON is
// signed to assert this service account acts as impersonateAs (a Workspace
// super-admin), scoped to exactly admin.directory.user and
// admin.directory.group.member. Does not itself make a network call (the
// JWT exchange happens lazily on first use), so it's safe to call at boot
// even if the network is briefly unavailable.
func NewClient(ctx context.Context, serviceAccountJSON []byte, impersonateAs string) (Provisioner, error) {
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

func isNotFound(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == 404
}

func isDuplicate(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && (apiErr.Code == 409 || apiErr.Code == 400)
}
