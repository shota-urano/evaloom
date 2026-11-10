package runner

import (
	"context"
	"net/http"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestReportResultReturnsConflictInsteadOfAcceptingAmbiguousAcknowledgement(t *testing.T) {
	process := newPullRunner(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	err := process.ReportResult(context.Background(), logExecution, ExecutionResult{
		FromStatus: model.ExecutionRunning, Status: model.ExecutionTimedOut, ErrorClass: model.ErrorTimeout,
	})
	if err == nil || err.Error() != "runner: result returned HTTP 409" {
		t.Fatalf("ambiguous acknowledgement was accepted: %v", err)
	}
}
