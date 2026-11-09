package linearfeedback_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearfeedback"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

const stateKey = "linear-1:state_waiting_approval:linear-1-e1"

// stateLinear uses the real client's GraphQL boundary, alongside the existing
// comment server. Reads and updates can fail independently.
type stateLinear struct {
	fakeLinear
	current                               linear.WorkflowState
	teams                                 [][]linear.WorkflowState
	teamReads                             int
	stateReads                            int
	updates                               []map[string]any
	stateStatus, teamStatus, updateStatus int
	updateBody                            string
	cutUpdates                            int
	applyBeforeCut                        bool
	failRefresh                           bool
	onUpdate                              func()
	holdUpdate                            bool
	updateStarted                         chan struct{}
	archived, trashed, removed            bool
}

func newStateLinear() *stateLinear {
	return &stateLinear{
		fakeLinear: fakeLinear{commentStatus: http.StatusOK, commentBody: okComment, issueBody: issueJSON(false)},
		current:    linear.WorkflowState{ID: "progress", Name: "In Progress", Type: "started"},
		teams: [][]linear.WorkflowState{{
			{ID: "progress", Name: "In Progress", Type: "started"},
			{ID: "review", Name: "PR Review", Type: "started"},
		}},
	}
}

func (f *stateLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Query     string
		Variables map[string]any
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !strings.Contains(req.Query, "EvaloomIssueState") && !strings.Contains(req.Query, "EvaloomTeamStates") &&
		!strings.Contains(req.Query, "issueUpdate") {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		f.fakeLinear.ServeHTTP(w, r)
		return
	}
	if strings.Contains(req.Query, "issueUpdate") && f.onUpdate != nil {
		f.onUpdate()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(req.Query, "issueUpdate"):
		f.updates = append(f.updates, req.Variables)
		if f.holdUpdate {
			f.updateStarted <- struct{}{}
			<-r.Context().Done()
			return
		}
		if f.cutUpdates > 0 {
			f.cutUpdates--
			if f.applyBeforeCut {
				f.current = f.teams[0][1]
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		if f.updateStatus != 0 {
			w.WriteHeader(f.updateStatus)
		}
		if f.updateBody != "" {
			io.WriteString(w, f.updateBody)
		} else {
			io.WriteString(w, `{"data":{"issueUpdate":{"success":true}}}`)
		}
	case strings.Contains(req.Query, "EvaloomIssueState"):
		f.stateReads++
		if f.stateStatus != 0 {
			w.WriteHeader(f.stateStatus)
			return
		}
		if f.removed {
			io.WriteString(w, `{"data":{"issue":null}}`)
			return
		}
		var archivedAt any
		if f.archived {
			archivedAt = testTime
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue": map[string]any{
			"id": req.Variables["id"], "team": map[string]string{"id": "team-a"}, "state": f.current,
			"archivedAt": archivedAt, "trashed": f.trashed}}})
	case strings.Contains(req.Query, "EvaloomTeamStates"):
		index := min(f.teamReads, len(f.teams)-1)
		f.teamReads++
		if req.Variables["id"] != "team-a" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.failRefresh && f.teamReads%2 == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if f.teamStatus != 0 {
			w.WriteHeader(f.teamStatus)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"team": map[string]any{
			"id": "team-a", "states": map[string]any{"nodes": f.teams[index],
				"pageInfo": map[string]any{"hasNextPage": false}}}}})
	}
}

func setupState(t *testing.T, fake *stateLinear) (*sql.DB, *linearfeedback.Worker, *clock) {
	t.Helper()
	db, worker, c, _ := setup(t, fake)
	mustExec(t, db, `UPDATE linear_outbox SET status = 'sent' WHERE action = 'comment'`)
	mustExec(t, db, `UPDATE linear_outbox SET status = 'pending', kind = 'waiting_approval',
		state_name = 'PR Review', dedupe_key = ? WHERE action = 'set_state'`, stateKey)
	return db, worker, c
}

// 3.6.2: the two started states must be distinguished by exact name.
func TestStateSelectsExactNameAndSendsOnce(t *testing.T) {
	fake := newStateLinear()
	db, worker, c := setupState(t, fake)
	sendDue(t, worker)
	c.advance(time.Hour)
	sendDue(t, worker)
	if len(fake.updates) != 1 || fake.updates[0]["stateId"] != "review" || fake.updates[0]["id"] != "issue-1" {
		t.Fatalf("issueUpdate = %v; want review once", fake.updates)
	}
	if r := outboxRow(t, db, stateKey); r.status != "sent" || r.attempts != 1 || r.commentID.Valid || !r.sentAt.Valid || r.outcomeUnknown != 0 {
		t.Fatalf("state row = %+v", r)
	}
	events := notificationEvents(t, db)
	if len(events) != 1 || events[0].kind != "notification_sent" || events[0].payload["kind"] != "waiting_approval" ||
		events[0].payload["channel"] != "linear" {
		t.Fatalf("events = %+v", events)
	}
}

func TestStateMissingNameRefreshesThenFails(t *testing.T) {
	for _, name := range []string{"pr review", "PR REVIEW", "PR Review ", " PR Review", "Review"} {
		for _, withExecution := range []bool{true, false} {
			t.Run(name+"/execution="+strconv.FormatBool(withExecution), func(t *testing.T) {
				fake := newStateLinear()
				fake.teams[0][1].Name = name
				db, worker, c := setupState(t, fake)
				if !withExecution {
					mustExec(t, db, `UPDATE linear_outbox SET execution_id = NULL WHERE action = 'set_state'`)
				}
				before := taskStates(t, db)
				sendDue(t, worker)
				c.advance(time.Hour)
				sendDue(t, worker)
				if len(fake.updates) != 0 || fake.teamReads != 2 {
					t.Fatalf("updates=%v reads=%d", fake.updates, fake.teamReads)
				}
				r := outboxRow(t, db, stateKey)
				fr := readFailureRecord(t, db, stateKey)
				if r.status != "failed" || r.attempts != 1 || r.nextAttemptAt.Valid || r.outcomeUnknown != 0 ||
					fr.failureReason.String != "state_not_found" || !fr.failedAt.Valid {
					t.Fatalf("row=%+v failure=%+v", r, fr)
				}
				events := notificationEvents(t, db)
				if withExecution {
					if len(events) != 1 || events[0].kind != "notification_failed" || len(events[0].payload) != 3 ||
						events[0].payload["channel"] != "linear" || events[0].payload["kind"] != "waiting_approval" ||
						events[0].payload["reason"] != "state_not_found" {
						t.Fatalf("events=%+v", events)
					}
				} else if len(events) != 0 {
					t.Fatalf("events without execution=%+v", events)
				}
				failures, err := store.ListNotificationFailures(context.Background(), db, nil, 10)
				if err != nil || len(failures) != 1 || failures[0].Reason != "state_not_found" || failures[0].Channel != "linear" {
					t.Fatalf("failures=%+v err=%v", failures, err)
				}
				if after := taskStates(t, db); after != before {
					t.Fatalf("task changed: %+v -> %+v", before, after)
				}
			})
		}
	}
}

func TestStateFoundOnRefreshIsUsed(t *testing.T) {
	fake := newStateLinear()
	fake.teams = append([][]linear.WorkflowState{{fake.teams[0][0]}}, fake.teams...)
	db, worker, _ := setupState(t, fake)
	sendDue(t, worker)
	if fake.teamReads != 2 || len(fake.updates) != 1 || fake.updates[0]["stateId"] != "review" || outboxRow(t, db, stateKey).status != "sent" {
		t.Fatalf("reads=%d updates=%v row=%+v", fake.teamReads, fake.updates, outboxRow(t, db, stateKey))
	}
}

func TestStateAlreadySetOrTerminalIsSentWithoutUpdate(t *testing.T) {
	for _, state := range []linear.WorkflowState{
		{ID: "review", Name: "PR Review", Type: "started"},
		{ID: "done", Name: "Done", Type: "completed"},
		{ID: "canceled", Name: "Canceled", Type: "canceled"},
	} {
		t.Run(state.Name, func(t *testing.T) {
			fake := newStateLinear()
			fake.current = state
			db, worker, _ := setupState(t, fake)
			sendDue(t, worker)
			if r := outboxRow(t, db, stateKey); r.status != "sent" || r.commentID.Valid || r.outcomeUnknown != 0 || !r.sentAt.Valid {
				t.Fatalf("row=%+v", r)
			}
			if len(fake.updates) != 0 || fake.teamReads != 0 {
				t.Fatalf("updates=%v reads=%d", fake.updates, fake.teamReads)
			}
		})
	}
}

func TestStateTemporaryFailuresRetryThenExhaust(t *testing.T) {
	for _, stage := range []string{"issue", "team", "update", "update rejected", "refresh"} {
		t.Run(stage, func(t *testing.T) {
			fake := newStateLinear()
			switch stage {
			case "issue":
				fake.stateStatus = http.StatusInternalServerError
			case "team":
				fake.teamStatus = http.StatusTooManyRequests
			case "update":
				fake.updateStatus = http.StatusInternalServerError
			case "update rejected":
				fake.updateBody = `{"data":{"issueUpdate":{"success":false}}}`
			case "refresh":
				fake.teams[0] = fake.teams[0][:1]
				fake.failRefresh = true
			}
			db, worker, c := setupState(t, fake)
			before := taskStates(t, db)
			for attempt := 1; attempt <= linearfeedback.MaxAttempts; attempt++ {
				sendDue(t, worker)
				r := outboxRow(t, db, stateKey)
				if r.attempts != attempt || r.outcomeUnknown != 0 {
					t.Fatalf("attempt %d row=%+v", attempt, r)
				}
				if attempt < linearfeedback.MaxAttempts {
					delay := linearfeedback.RetryDelays[attempt-1]
					next, err := time.Parse(time.RFC3339Nano, r.nextAttemptAt.String)
					if r.status != "pending" || err != nil || !next.Equal(c.t.Add(delay)) {
						t.Fatalf("row=%+v", r)
					}
					c.advance(delay - time.Second)
					sendDue(t, worker)
					if outboxRow(t, db, stateKey).attempts != attempt {
						t.Fatal("retry before due")
					}
					c.advance(time.Second)
				}
			}
			if fr := readFailureRecord(t, db, stateKey); fr.status != "failed" || fr.failureReason.String != "retries_exhausted" {
				t.Fatalf("failure=%+v", fr)
			}
			c.advance(time.Hour)
			sendDue(t, worker)
			events := notificationEvents(t, db)
			if len(events) != 1 || events[0].payload["reason"] != "retries_exhausted" {
				t.Fatalf("events=%+v", events)
			}
			if after := taskStates(t, db); after != before {
				t.Fatalf("task changed: %+v -> %+v", before, after)
			}
		})
	}
}

func TestStateAuthSharesSuppressionAndSuccessReleasesIt(t *testing.T) {
	for _, stage := range []string{"issue", "team", "update"} {
		t.Run(stage, func(t *testing.T) {
			fake := newStateLinear()
			fake.commentStatus = http.StatusUnauthorized
			switch stage {
			case "issue":
				fake.stateStatus = http.StatusUnauthorized
			case "team":
				fake.teamStatus = http.StatusUnauthorized
			case "update":
				fake.updateStatus = http.StatusUnauthorized
			}
			db, worker, c := setupState(t, fake)
			alerter, gate := &recordingAuthAlerter{}, &linear.AuthAlertGate{}
			worker.Alerter, worker.AuthGate = alerter, gate
			mustExec(t, db, `UPDATE linear_outbox SET status = 'pending' WHERE action = 'comment'`)
			sendDue(t, worker)
			c.advance(time.Hour)
			sendDue(t, worker)
			if n := countStatus(t, db, "failed", "auth"); n != 2 {
				t.Fatalf("auth rows=%d", n)
			}
			if r := outboxRow(t, db, stateKey); r.attempts != 1 || r.outcomeUnknown != 0 {
				t.Fatalf("row=%+v", r)
			}
			alerter.want(t, "comment")
			poller := sharedPoller(t, db, &pollClient{class: linear.ClassAuth}, gate, alerter)
			pollOnce(t, poller)
			alerter.want(t, "comment")
			// A successful state update re-enables both comment and poll alerts.
			fake.mu.Lock()
			fake.stateStatus, fake.teamStatus, fake.updateStatus = 0, 0, 0
			fake.mu.Unlock()
			mustExec(t, db, `UPDATE linear_outbox SET status = 'pending', attempts = 0 WHERE action = 'set_state'`)
			sendDue(t, worker)
			if outboxRow(t, db, stateKey).status != "sent" || len(fake.updates) == 0 {
				t.Fatal("state was not sent")
			}
			queueRows(t, db, "after-state", 1)
			sendDue(t, worker)
			pollOnce(t, poller)
			alerter.want(t, "comment", "comment")
		})
	}
}

func TestStateLostResponseNeverReconcilesComments(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(strconv.FormatBool(applied), func(t *testing.T) {
			fake := newStateLinear()
			fake.cutUpdates, fake.applyBeforeCut = 1, applied
			db, worker, c := setupState(t, fake)
			fake.onUpdate = func() {
				if r := outboxRow(t, db, stateKey); r.outcomeUnknown != 0 {
					t.Errorf("unknown during update=%d", r.outcomeUnknown)
				}
			}
			sendDue(t, worker)
			if r := outboxRow(t, db, stateKey); r.status != "pending" || r.attempts != 1 || r.outcomeUnknown != 0 {
				t.Fatalf("row=%+v", r)
			}
			worker = restart(worker, c)
			c.advance(time.Minute)
			sendDue(t, worker)
			want := 2
			if applied {
				want = 1
			}
			if len(fake.updates) != want || fake.commentReads != 0 || fake.commentCount() != 0 {
				t.Fatalf("updates=%v comment reads=%d", fake.updates, fake.commentReads)
			}
			if r := outboxRow(t, db, stateKey); r.status != "sent" || r.outcomeUnknown != 0 || r.commentID.Valid {
				t.Fatalf("row=%+v", r)
			}
		})
	}
}

func TestStateShutdownDoesNotCountAttempt(t *testing.T) {
	fake := newStateLinear()
	fake.holdUpdate, fake.updateStarted = true, make(chan struct{}, 1)
	db, worker, _ := setupState(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.SendDue(ctx) }()
	select {
	case <-fake.updateStarted:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("no update request")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	if r := outboxRow(t, db, stateKey); r.status != "pending" || r.attempts != 0 || r.outcomeUnknown != 0 {
		t.Fatalf("row=%+v", r)
	}
}

func TestSkippedStateIsNotSentOrListedAsFailure(t *testing.T) {
	fake := newStateLinear()
	db, worker, _ := setupState(t, fake)
	mustExec(t, db, `UPDATE linear_outbox SET status = 'skipped' WHERE action = 'set_state'`)
	// Cover both event-backed and outbox-backed triggers.
	mustExec(t, db, `INSERT INTO linear_outbox (id, task_id, linear_issue_id, kind, action, state_name,
		dedupe_key, status, created_at) VALUES ('skipped', 'linear-1', 'issue-1', 'cancelled',
		'set_state', 'Canceled', 'skipped-state', 'skipped', '1999-01-01T00:00:00Z')`)
	// A skipped row must not prevent comments from being delivered.
	mustExec(t, db, `UPDATE linear_outbox SET status = 'pending' WHERE action = 'comment'`)
	sendDue(t, worker)
	if fake.stateReads != 0 || len(fake.updates) != 0 || fake.commentCount() != 1 {
		t.Fatal("skipped row sent or comment blocked")
	}
	failures, err := store.ListNotificationFailures(context.Background(), db, nil, 10)
	if err != nil || len(failures) != 0 {
		t.Fatalf("failures=%+v err=%v", failures, err)
	}
}

func TestStateFailureLeavesTaskAndCommentDeliveryUnchanged(t *testing.T) {
	fake := newStateLinear()
	fake.updateStatus = http.StatusServiceUnavailable
	db, worker, _ := setupState(t, fake)
	before := taskStates(t, db)
	mustExec(t, db, `UPDATE linear_outbox SET created_at = '1999-01-01T00:00:00Z' WHERE action = 'set_state'`)
	mustExec(t, db, `UPDATE linear_outbox SET status = 'pending' WHERE action = 'comment'`)
	sendDue(t, worker)
	if outboxRow(t, db, stateKey).status != "pending" || outboxRow(t, db, dedupeKey).status != "sent" || fake.commentCount() != 1 {
		t.Fatal("failed state update affected comment delivery")
	}
	if after := taskStates(t, db); after != before {
		t.Fatalf("task changed: %+v -> %+v", before, after)
	}
}

func TestRemovedStateIssueFailsImmediately(t *testing.T) {
	for _, mode := range []string{"removed", "archived", "trashed"} {
		t.Run(mode, func(t *testing.T) {
			fake := newStateLinear()
			fake.removed, fake.archived, fake.trashed = mode == "removed", mode == "archived", mode == "trashed"
			db, worker, c := setupState(t, fake)
			sendDue(t, worker)
			c.advance(time.Hour)
			sendDue(t, worker)
			if fr := readFailureRecord(t, db, stateKey); fr.status != "failed" || fr.failureReason.String != "issue_removed" {
				t.Fatalf("failure=%+v", fr)
			}
			if len(fake.updates) != 0 || fake.teamReads != 0 || fake.stateReads != 1 {
				t.Fatal("removed Issue was retried or updated")
			}
		})
	}
}

func TestStateFailureDoesNotReleaseAuthSuppression(t *testing.T) {
	for _, mode := range []string{"missing name", "temporary failure", "removed Issue"} {
		t.Run(mode, func(t *testing.T) {
			fake := newStateLinear()
			switch mode {
			case "missing name":
				fake.teams[0] = fake.teams[0][:1]
			case "temporary failure":
				fake.updateStatus = http.StatusServiceUnavailable
			case "removed Issue":
				fake.removed = true
			}
			db, worker, _ := setupState(t, fake)
			gate, alerter := &linear.AuthAlertGate{}, &recordingAuthAlerter{}
			gate.Acquire() // A previous comment/poll auth failure already alerted.
			worker.AuthGate, worker.Alerter = gate, alerter
			sendDue(t, worker)
			fake.answer(http.StatusUnauthorized, unauthorized)
			queueRows(t, db, "still-auth", 1)
			sendDue(t, worker)
			alerter.want(t)
		})
	}
}
