package publisher

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shota-urano/evaloom/backend/internal/gate"
	"github.com/shota-urano/evaloom/backend/internal/model"
)

var ErrPRReport = errors.New("publisher: execution report is incomplete or inconsistent")

// Report is trusted Runner metadata for the spec 11, 3.3 PR body and result
// comment. Summary comes from HarnessResult.summary. ContinuedBlocks contains
// the contents of Blocks the owner actually approved, not harness assertions.
// This is a local boundary, not a new API representation or database schema.
type Report struct {
	ExecutionID, TaskID, Title, AcceptanceCriteria string
	Summary                                        string
	HarnessID, HarnessVersion                      string
	ProfileVersion, ProfileCommitSHA               string
	Gate2Result, Gate3Result                       string
	Gate2Findings, Gate3Findings                   []model.Finding
	VerifyRun                                      model.TestRun
	ContinuedBlocks                                []string
}

func (r Report) body(verifiedSHA string) (string, error) {
	for _, value := range []string{r.ExecutionID, r.TaskID, r.Title, r.AcceptanceCriteria, r.Summary,
		r.HarnessID, r.HarnessVersion, r.ProfileVersion, r.ProfileCommitSHA} {
		if strings.TrimSpace(value) == "" {
			return "", ErrPRReport
		}
	}
	if !objectID.MatchString(r.ProfileCommitSHA) || !objectID.MatchString(verifiedSHA) ||
		!gate.CheckCommit(r.ExecutionID, verifiedSHA, verifiedSHA, &r.VerifyRun) ||
		r.Gate3Result != "pass" || (r.Gate2Result != "pass" && r.Gate2Result != "blocked") ||
		(r.Gate2Result == "blocked" && len(r.ContinuedBlocks) == 0) {
		return "", ErrPRReport
	}
	for _, block := range r.ContinuedBlocks {
		if strings.TrimSpace(block) == "" {
			return "", ErrPRReport
		}
	}
	method := "ID による照合"
	for _, finding := range r.Gate2Findings {
		if finding.Rule == "count_only_check" {
			method = "件数による確認"
		}
	}
	for _, finding := range r.Gate2Findings {
		if finding.Rule == "test_report_missing" {
			method = "照合できない（本人の続行判断あり）"
		}
	}
	var body strings.Builder
	fmt.Fprintf(&body, "## タスク\n\n%s: %s\n\n### 受け入れ条件\n\n%s\n\n", r.TaskID, r.Title, r.AcceptanceCriteria)
	fmt.Fprintf(&body, "## 作業概要\n\n%s\n\n", r.Summary)
	fmt.Fprintf(&body, "## 実行情報\n\n- Execution: %s\n- ハーネス: %s\n- ハーネスの版: %s\n- Profile の版: %s\n- Profile commit SHA: %s\n- 検証・push commit SHA: %s\n\n",
		r.ExecutionID, r.HarnessID, r.HarnessVersion, r.ProfileVersion, r.ProfileCommitSHA, verifiedSHA)
	fmt.Fprintf(&body, "## Gate の結果\n\n- Gate 2: %s\n- 実行数: %d\n- skip 数: %d\n- 確認方法: %s\n- Gate 3: %s\n",
		r.Gate2Result, *r.VerifyRun.Total-*r.VerifyRun.Skipped, *r.VerifyRun.Skipped, method, r.Gate3Result)
	for _, findings := range [][]model.Finding{r.Gate2Findings, r.Gate3Findings} {
		for _, finding := range findings {
			fmt.Fprintf(&body, "- %s: %s\n", finding.Rule, finding.Message)
		}
	}
	if len(r.ContinuedBlocks) > 0 {
		body.WriteString("\n## 本人が続行を選んだ Block\n\n")
		for _, block := range r.ContinuedBlocks {
			fmt.Fprintf(&body, "- %s\n", block)
		}
	}
	body.WriteString("\nハーネスが書いたテストの成功だけを要件充足の証明としない。\n")
	return body.String(), nil
}
