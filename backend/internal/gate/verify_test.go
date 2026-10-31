package gate

import (
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestVerifyExistingTests(t *testing.T) {
	for _, tc := range []struct {
		name      string
		report    *TestReport
		exit      int
		countOnly bool
		result    string
		cause     model.BlockCause
		class     model.ErrorClass
	}{
		{"MissingIDDespiteEqualCount", &TestReport{Total: 3, Passed: 2, Skipped: 1, IDsAvailable: true, TestIDs: []string{"a", "new"}}, 0, false, "blocked", model.BlockGate2MissingTests, ""},
		{"MoreSkippedDespiteMoreTests", &TestReport{Total: 6, Passed: 4, Skipped: 2, IDsAvailable: true, TestIDs: []string{"a", "b", "new", "new2"}}, 0, false, "blocked", model.BlockGate2MissingTests, ""},
		{"AddedTestsPass", &TestReport{Total: 4, Passed: 3, Skipped: 1, IDsAvailable: true, TestIDs: []string{"a", "b", "new"}}, 0, false, "pass", "", ""},
		{"MissingReport", nil, 0, false, "blocked", model.BlockGate2Unverifiable, ""},
		{"NonzeroExitTakesPriority", nil, 1, false, "failed", "", model.ErrorTestFailed},
		{"FailureWithZeroExit", &TestReport{Total: 3, Failed: 1, Passed: 1, Skipped: 1}, 0, false, "failed", "", model.ErrorTestFailed},
		{"CountsExecutedDecrease", &TestReport{Total: 2, Passed: 1, Skipped: 1}, 0, true, "blocked", model.BlockGate2MissingTests, ""},
		{"CountsSkipIncrease", &TestReport{Total: 5, Passed: 3, Skipped: 2}, 0, true, "blocked", model.BlockGate2MissingTests, ""},
		{"CountsAddedTestsPass", &TestReport{Total: 5, Passed: 4, Skipped: 1}, 0, true, "pass", "", ""},
		{"IDsUnavailableUsesCounts", &TestReport{Total: 3, Passed: 2, Skipped: 1}, 0, false, "pass", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &model.TestRun{Total: new(3), Skipped: new(1), TestIDsArtifactID: new("artifact")}
			if tc.countOnly {
				base.TestIDsArtifactID = nil
			}
			got := Verify(base, []string{"a", "b"}, model.TestRun{ExitCode: &tc.exit}, tc.report)
			if got.Result != tc.result || got.Cause != tc.cause || got.ErrorClass != tc.class {
				t.Fatalf("decision=%+v", got)
			}
			if tc.report != nil && tc.class == "" && got.CountOnly != (tc.countOnly || !tc.report.IDsAvailable) {
				t.Fatalf("count fallback=%+v", got)
			}
		})
	}
}

func TestVerifyCannotReplaceUnreadableIDsWithCounts(t *testing.T) {
	base := &model.TestRun{Total: new(2), Skipped: new(0), TestIDsArtifactID: new("artifact")}
	report := &TestReport{Total: 3, Passed: 3, IDsAvailable: true, TestIDs: []string{"a", "b", "c"}}
	for _, ids := range [][]string{nil, {"a"}} {
		if got := Verify(base, ids, model.TestRun{ExitCode: new(0)}, report); got.Cause != model.BlockGate2Unverifiable {
			t.Fatalf("decision=%+v", got)
		}
	}
	base.Total = new(2)
	got := Verify(base, []string{"a", "a"}, model.TestRun{ExitCode: new(0)}, report)
	if got.Cause != model.BlockGate2MissingTests {
		t.Fatalf("duplicate ID disappearance=%+v", got)
	}
}

func TestVerifyRequiresBaselineAndKnownExit(t *testing.T) {
	report := &TestReport{Total: 1, Passed: 1}
	base := &model.TestRun{Total: new(1), Skipped: new(0)}
	for _, got := range []Verification{Verify(nil, nil, model.TestRun{ExitCode: new(0)}, report), Verify(base, nil, model.TestRun{}, report)} {
		if got.Cause != model.BlockGate2Unverifiable {
			t.Fatalf("decision=%+v", got)
		}
	}
}
