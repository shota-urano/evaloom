package gate

import "github.com/shota-urano/evaloom/backend/internal/model"

// Verification is a mechanical decision from spec 10, 3.3. The Runner records
// the corresponding Finding rules from section 3.6.
type Verification struct {
	Result     string
	Cause      model.BlockCause
	ErrorClass model.ErrorClass
	CountOnly  bool
	MissingIDs []string
}

// Verify compares executed counts (total minus skipped), never equal totals.
// A saved ID artifact requires a complete list; inability to read it is not
// permission to silently weaken an ID comparison to a count comparison.
func Verify(baseline *model.TestRun, baselineIDs []string, run model.TestRun, report *TestReport) Verification {
	if run.ExitCode != nil && *run.ExitCode != 0 || report != nil && report.Failed > 0 {
		return Verification{Result: "failed", ErrorClass: model.ErrorTestFailed}
	}
	unverifiable := Verification{Result: "blocked", Cause: model.BlockGate2Unverifiable}
	if run.ExitCode == nil || report == nil || baseline == nil || baseline.Total == nil || baseline.Skipped == nil ||
		*baseline.Total < *baseline.Skipped || *baseline.Skipped < 0 {
		return unverifiable
	}
	decision := Verification{Result: "pass", CountOnly: baseline.TestIDsArtifactID == nil || !report.IDsAvailable}
	if baseline.TestIDsArtifactID != nil && report.IDsAvailable {
		if baselineIDs == nil {
			return unverifiable
		}
		if len(baselineIDs) != *baseline.Total-*baseline.Skipped {
			return unverifiable
		}
		// Preserve multiplicity: duplicate IDs must not hide a removed case.
		remaining := make(map[string]int, len(report.TestIDs))
		for _, id := range report.TestIDs {
			remaining[id]++
		}
		for _, id := range baselineIDs {
			if remaining[id] == 0 {
				decision.MissingIDs = append(decision.MissingIDs, id)
			} else {
				remaining[id]--
			}
		}
	}
	if len(decision.MissingIDs) > 0 || report.Executed() < *baseline.Total-*baseline.Skipped || report.Skipped > *baseline.Skipped {
		decision.Result, decision.Cause = "blocked", model.BlockGate2MissingTests
	}
	return decision
}
