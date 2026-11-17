package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// ProfileRequiresAcceptanceCriteria reads the declaration of a registered
// Profile. Project settings cannot override it (spec 19 3.9, contract 5.2).
func ProfileRequiresAcceptanceCriteria(ctx context.Context, reader projectReader, profileID string) (bool, error) {
	profile, err := GetHarnessProfileByID(ctx, reader, profileID)
	if err != nil {
		return false, err
	}
	var definition struct {
		AcceptanceCriteria string `json:"acceptanceCriteria"`
	}
	if err := json.Unmarshal(profile.Definition, &definition); err != nil {
		return false, err
	}
	return definition.AcceptanceCriteria == "required", nil
}

// LinearIntakeBlockCause checks a decided destination, never a routing task.
// Projects use their pinned Profile; direct harness routings use the default
// Profile. A missing default is checked before criteria (spec 15 3.6 steps 4-5).
func LinearIntakeBlockCause(ctx context.Context, reader projectReader, projectID, harnessID, criteria string) (model.IntakeBlockCause, error) {
	var profileID string
	if projectID != "" {
		project, err := GetProject(ctx, reader, projectID)
		if err != nil {
			return "", err
		}
		profileID = project.ProfileID
	} else {
		harness, err := GetHarness(ctx, reader, harnessID)
		if err != nil {
			return "", err
		}
		if harness.DefaultProfileID == nil {
			return model.IntakeConfigMissingProfile, nil
		}
		profileID = *harness.DefaultProfileID
	}
	required, err := ProfileRequiresAcceptanceCriteria(ctx, reader, profileID)
	if err != nil {
		return "", err
	}
	if required && strings.TrimSpace(criteria) == "" {
		return model.IntakeMissingAcceptanceCriteria, nil
	}
	return "", nil
}
