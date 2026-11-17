package api

import (
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Spec 19 3.10: the second Execution on a Project must remain pending even
// with a free global slot, throughout every active stage of the first one.
func TestClaimSameProjectWaitsWithFreeGlobalSlot(t *testing.T) {
	f, token, _ := claimFixture(t)
	a := createClaimTask(t, f, "project-a", "route-slot-a1")
	next := createClaimTask(t, f, "project-a", "route-slot-a2")
	setExecutionCreatedAt(t, f, a, next)
	status, body := requestClaim(t, f, token)
	first := requireClaim(t, status, body)
	if first.Execution.TaskID != a.ID {
		t.Fatalf("claimed %s; want %s", first.Execution.TaskID, a.ID)
	}
	for _, state := range []string{"claimed", "preparing", "working", "checking", "publishing"} {
		t.Run(state, func(t *testing.T) {
			setExecutionState(t, f, first.Execution.ID, state)
			status, body := requestClaim(t, f, token)
			requireNoJob(t, status, body)
			assertUnclaimed(t, f, next.ID)
			if active, perRoute := capacity(t, f); active != 1 || perRoute != 1 {
				t.Fatalf("active=%d perRoute=%d; want 1 each", active, perRoute)
			}
		})
	}
}

// Spec 19 3.10: direct routes are limited by harness, across Profile versions.
// Another harness can run while the oldest pending task waits on a busy one;
// waiting_approval and blocked release the corresponding route's slot.
func TestClaimDirectHarnessSlotsAcrossVersionsAndStates(t *testing.T) {
	f, token, profiles := claimFixture(t)
	enableDirectRouting(t, f, profiles[0].ID, "claim-runner")
	a1 := createDirectTask(t, f, "direct-slot-a1")
	a2 := createDirectTask(t, f, "direct-slot-a2")
	setExecutionCreatedAt(t, f, a1, a2)
	status, body := requestClaim(t, f, token)
	first := requireClaim(t, status, body)
	if first.Execution.TaskID != a1.ID || first.Profile.ID != profiles[0].ID {
		t.Fatalf("first claim=%+v profile=%+v", first.Execution, first.Profile)
	}
	// A new default version must not create another slot on harness-a.
	enableDirectRouting(t, f, profiles[1].ID, "claim-runner")
	for _, state := range []string{"claimed", "preparing", "working", "checking", "publishing"} {
		t.Run(state, func(t *testing.T) {
			setExecutionState(t, f, first.Execution.ID, state)
			status, body := requestClaim(t, f, token)
			requireNoJob(t, status, body)
			assertUnclaimed(t, f, a2.ID)
		})
	}

	status, body = projectRequest(t, f, "POST", "/api/harnesses", harnessInput(t, "harness-b"))
	h := requireHarness(t, status, body, 201)
	commitProfile(t, h.RepoPath, "1.2.0", validProfileYAML, false)
	status, body = projectRequest(t, f, "POST", "/api/harness-profiles", map[string]any{
		"harnessId": h.ID, "version": "1.2.0",
	})
	p := requireProfile(t, status, body, 201)
	status, body = projectRequest(t, f, "PATCH", "/api/harnesses/"+h.ID, map[string]any{"defaultProfileId": p.ID})
	requireHarness(t, status, body, 200)
	var pendingB []model.Task
	for _, key := range []string{"direct-slot-b1", "direct-slot-b2"} {
		input := manualTaskInput()
		delete(input, "projectId")
		input["harnessId"] = h.ID
		status, body = taskRequest(t, f, key, input, nil)
		pendingB = append(pendingB, requireTask(t, status, body, 201))
	}
	setExecutionCreatedAt(t, f, a1, a2, pendingB[0], pendingB[1])
	status, body = requestClaim(t, f, token)
	other := requireClaim(t, status, body)
	if other.Execution.TaskID != pendingB[0].ID || other.Profile.HarnessID != h.ID {
		t.Fatalf("claimed %+v; want first task on %s", other, h.ID)
	}
	assertUnclaimed(t, f, a2.ID)
	assertUnclaimed(t, f, pendingB[1].ID)
	status, body = requestClaim(t, f, token)
	requireNoJob(t, status, body)

	setExecutionState(t, f, first.Execution.ID, "waiting_approval")
	status, body = requestClaim(t, f, token)
	second := requireClaim(t, status, body)
	if second.Execution.TaskID != a2.ID || second.Profile.ID != profiles[1].ID {
		t.Fatalf("released harness-a claim=%+v profile=%+v", second.Execution, second.Profile)
	}
	setExecutionState(t, f, other.Execution.ID, "blocked")
	status, body = requestClaim(t, f, token)
	if got := requireClaim(t, status, body); got.Execution.TaskID != pendingB[1].ID {
		t.Fatalf("claimed %+v; want second task on %s", got.Execution, h.ID)
	}
	if active, perRoute := capacity(t, f); active != 2 || perRoute != 1 {
		t.Fatalf("active=%d perRoute=%d; want 2 and 1", active, perRoute)
	}
}

// Spec 19 3.10: a direct route never occupies the slot of a Project that uses
// the same harness, including when the direct route was claimed first.
func TestClaimProjectSlotIndependentOfActiveDirectHarness(t *testing.T) {
	f, token, profiles := claimFixture(t)
	enableDirectRouting(t, f, profiles[0].ID, "claim-runner")
	h := createDirectTask(t, f, "direct-before-project")
	a := createClaimTask(t, f, "project-a", "project-after-direct")
	setExecutionCreatedAt(t, f, h, a)
	status, body := requestClaim(t, f, token)
	direct := requireClaim(t, status, body)
	status, body = requestClaim(t, f, token)
	project := requireClaim(t, status, body)
	if direct.Execution.TaskID != h.ID || project.Execution.TaskID != a.ID ||
		direct.Route.Kind != model.RouteHarness || project.Route.Kind != model.RouteProject ||
		direct.Profile.HarnessID != project.Profile.HarnessID {
		t.Fatalf("direct=%+v project=%+v", direct, project)
	}
	if active, perRoute := capacity(t, f); active != 2 || perRoute != 1 {
		t.Fatalf("active=%d perRoute=%d; want 2 and 1", active, perRoute)
	}
}
