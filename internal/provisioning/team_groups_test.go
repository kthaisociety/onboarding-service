package provisioning

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTeamGroupsMatchBackend is the drift tripwire onboarding-service-plan.md
// calls for: teamGroups' keys must exactly match landingpage-backend's
// allowedApplicationTeams (internal/handlers/general_application_handler.go)
// — hardcoded on both sides deliberately (this mapping changes rarely, and a
// silent mismatch here is worse than a loud one), so this test is what
// catches the two lists drifting apart instead of a real cross-service
// lookup.
func TestTeamGroupsMatchBackend(t *testing.T) {
	want := map[string]bool{
		"Business":    true,
		"Development": true,
		"Research":    true,
		"Growth":      true,
		"IT":          true,
	}

	require.Len(t, teamGroups, len(want))
	for team := range want {
		_, ok := teamGroups[team]
		require.True(t, ok, "teamGroups is missing team %q", team)
	}
	for team := range teamGroups {
		require.True(t, want[team], "teamGroups has unexpected team %q not in the backend's allowedApplicationTeams", team)
	}
}
