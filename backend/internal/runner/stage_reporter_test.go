package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

func TestStageReporterAgainstControlPlane(t *testing.T) {
	f := newLogControlPlane(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	process.pollInterval = time.Millisecond
	if _, err := f.db.Exec(`UPDATE harness_profiles SET definition = '{"stages":{}}' WHERE id = 'fixture-profile';
		UPDATE executions SET status = 'claimed', profile_id = 'fixture-profile', route_kind = 'project',
		project_id = 'project', harness_id = 'fixture-harness' WHERE id = ?`, logExecution); err != nil {
		t.Fatal(err)
	}
	// Lose the acknowledgement after the real API commits each kind of report.
	// Only the network response is disrupted; the handler and SQLite are real.
	handler := f.server.Config.Handler
	var lostStart, lostFinish atomic.Bool
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/result") {
			handler.ServeHTTP(w, req)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code == 200 && ((bytes.Contains(body, []byte(`"stage_started"`)) && !lostStart.Swap(true)) ||
			(bytes.Contains(body, []byte(`"stage_finished"`)) && !lostFinish.Swap(true))) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wantNext := []string{"check", "publish", "stop"}
	for i, stage := range []model.Stage{model.StageWork, model.StageCheck, model.StagePublish} {
		request := []byte(`{"stage":"` + string(stage) + `"}`)
		attempt, err := process.StartStage(ctx, logExecution, stage, request)
		if err != nil || attempt != 1 {
			t.Fatalf("start %s: attempt=%d err=%v", stage, attempt, err)
		}
		// Open through the artifact store to verify the bytes uploaded before start.
		var artifactID string
		if err := f.db.QueryRow(`SELECT request_artifact_id FROM stage_runs WHERE execution_id = ? AND stage = ?`, logExecution, stage).Scan(&artifactID); err != nil {
			t.Fatal(err)
		}
		_, file, err := f.files.Open(ctx, logExecution, artifactID)
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil || !bytes.Equal(got, request) {
			t.Fatalf("request artifact: %s err=%v", got, readErr)
		}
		outcome := successfulStage(stage)
		if stage == model.StageCheck {
			outcome.Result = json.RawMessage(`{"contractVersion":"1","stage":"check","status":"succeeded","message":"checked","contentId":"checked-v1"}`)
		}
		next, err := process.FinishStage(ctx, logExecution, stage, attempt, outcome)
		if err != nil || next.Next != wantNext[i] {
			t.Fatalf("finish %s: %+v err=%v", stage, next, err)
		}
		if stage == model.StageCheck && (next.CheckedContentID == nil || *next.CheckedContentID != "checked-v1") {
			t.Fatalf("lost API's publish content pin: %+v", next)
		}
		// A second delivery returns the original response without another row.
		again, err := process.FinishStage(ctx, logExecution, stage, attempt, outcome)
		if err != nil || !reflect.DeepEqual(again, next) {
			t.Fatalf("replayed finish: %+v err=%v; want %+v", again, err, next)
		}
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM stage_runs`:                                     3,
		`SELECT count(*) FROM usage_records`:                                  3,
		`SELECT count(*) FROM artifacts WHERE kind = 'request_json'`:          3,
		`SELECT count(*) FROM execution_events WHERE type = 'stage_started'`:  3,
		`SELECT count(*) FROM execution_events WHERE type = 'stage_finished'`: 3,
		`SELECT count(*) FROM executions WHERE status = 'succeeded'`:          1,
		`SELECT count(*) FROM tasks WHERE status = 'completed'`:               1,
	} {
		var got int
		if err := f.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got=%d want=%d err=%v", query, got, want, err)
		}
	}
	if !lostStart.Load() || !lostFinish.Load() {
		t.Fatal("acknowledgement loss was not exercised")
	}
}

func TestStageReporterDoesNotRetryRejectedFinish(t *testing.T) {
	f := newLogControlPlane(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	// There is no matching stage_started. A permanent conflict must return,
	// even with a live delivery context, rather than holding capacity forever.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = process.FinishStage(ctx, logExecution, model.StageWork, 1, successfulStage(model.StageWork))
	if err == nil || !strings.Contains(err.Error(), "HTTP 409") || ctx.Err() != nil {
		t.Fatalf("rejected finish: %v context=%v", err, ctx.Err())
	}
	var count int
	if err := f.db.QueryRow(`SELECT count(*) FROM stage_runs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected finish changed stages: count=%d err=%v", count, err)
	}
}

func TestStageReporterRejectsInvalidIdentityBeforeWriting(t *testing.T) {
	config := testConfig(t, "http://127.0.0.1:1")
	process, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id    string
		stage model.Stage
	}{{"../escape", model.StageWork}, {logExecution, model.Stage("unknown")}} {
		if _, err := process.StartStage(t.Context(), test.id, test.stage, []byte(`{}`)); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
	if _, err := os.Stat(config.DataDir); !os.IsNotExist(err) {
		t.Fatalf("invalid identity wrote storage: %v", err)
	}
}
