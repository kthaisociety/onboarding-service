package googleworkspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeProvisioner is an in-memory Provisioner used by this package's own
// tests and by internal/provisioning's tests — no network, no real Google
// credential involved anywhere in the test suite.
type fakeProvisioner struct {
	users              map[string]bool
	createCalls        int
	resetPasswordCalls int
	groupMembers       map[string]map[string]bool
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{
		users:        map[string]bool{},
		groupMembers: map[string]map[string]bool{},
	}
}

func (f *fakeProvisioner) UserExists(ctx context.Context, primaryEmail string) (bool, error) {
	return f.users[primaryEmail], nil
}

func (f *fakeProvisioner) CreateUser(ctx context.Context, in NewUser) error {
	f.createCalls++
	f.users[in.PrimaryEmail] = true
	return nil
}

func (f *fakeProvisioner) ResetPassword(ctx context.Context, primaryEmail, tempPassword string) error {
	f.resetPasswordCalls++
	return nil
}

func (f *fakeProvisioner) AddToGroup(ctx context.Context, groupEmail, memberEmail string) error {
	if f.groupMembers[groupEmail] == nil {
		f.groupMembers[groupEmail] = map[string]bool{}
	}
	f.groupMembers[groupEmail][memberEmail] = true
	return nil
}

func TestResolvePrimaryEmail(t *testing.T) {
	ctx := context.Background()

	t.Run("no collision", func(t *testing.T) {
		p := newFakeProvisioner()
		email, err := ResolvePrimaryEmail(ctx, p, "kthais.com", "Ada", "Lovelace")
		require.NoError(t, err)
		require.Equal(t, "ada.lovelace@kthais.com", email)
	})

	t.Run("one collision appends 2", func(t *testing.T) {
		p := newFakeProvisioner()
		p.users["erik.svensson@kthais.com"] = true
		email, err := ResolvePrimaryEmail(ctx, p, "kthais.com", "Erik", "Svensson")
		require.NoError(t, err)
		require.Equal(t, "erik.svensson2@kthais.com", email)
	})

	t.Run("several collisions increments further", func(t *testing.T) {
		p := newFakeProvisioner()
		p.users["erik.svensson@kthais.com"] = true
		p.users["erik.svensson2@kthais.com"] = true
		p.users["erik.svensson3@kthais.com"] = true
		email, err := ResolvePrimaryEmail(ctx, p, "kthais.com", "Erik", "Svensson")
		require.NoError(t, err)
		require.Equal(t, "erik.svensson4@kthais.com", email)
	})

	t.Run("non-ASCII names are simple-folded", func(t *testing.T) {
		p := newFakeProvisioner()
		email, err := ResolvePrimaryEmail(ctx, p, "kthais.com", "Åsa", "Öberg")
		require.NoError(t, err)
		require.Equal(t, "asa.oberg@kthais.com", email)
	})

	t.Run("never calls CreateUser itself", func(t *testing.T) {
		p := newFakeProvisioner()
		_, err := ResolvePrimaryEmail(ctx, p, "kthais.com", "Ada", "Lovelace")
		require.NoError(t, err)
		require.Equal(t, 0, p.createCalls, "ResolvePrimaryEmail should only check, never insert")
	})
}
