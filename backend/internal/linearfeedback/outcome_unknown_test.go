package linearfeedback_test

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/linear"
	"github.com/shota-urano/evaloom/backend/internal/linearfeedback"
)

// Tests for spec 16 3.4.1 (Q83): the unknown result of a send is kept in
// linear_outbox.outcome_unknown, not in the Worker's memory.

func outcomeUnknownNow(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`SELECT outcome_unknown FROM linear_outbox WHERE dedupe_key = ?`, dedupeKey).Scan(&v); err != nil {
		t.Errorf("read outcome_unknown: %v", err)
	}
	return v
}

// restart returns a new Worker on the same DB and client, as evaloomd builds
// after a restart. It shares nothing in memory with the old Worker.
func restart(w *linearfeedback.Worker, c *clock) *linearfeedback.Worker {
	return &linearfeedback.Worker{DB: w.DB, Client: w.Client, Logger: w.Logger, Now: c.now}
}

// 受け入れ基準: 送信を始める前に outcome_unknown が 1 でコミットされる。
// 受け入れ基準: 成功で 0 に戻る。
func TestOutcomeUnknownCommittedBeforeSendAndClearedOnSuccess(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment}
	db, worker, _, _ := setup(t, fake)
	if got := outboxRow(t, db, dedupeKey).outcomeUnknown; got != 0 {
		t.Fatalf("new row outcome_unknown = %d; want 0", got)
	}
	var during []int
	// The handler reads the DB through its own query while the Worker waits
	// for the response, so it sees only committed data.
	fake.onCreate = func() { during = append(during, outcomeUnknownNow(t, db)) }

	sendDue(t, worker)

	if len(during) != 1 || during[0] != 1 {
		t.Fatalf("outcome_unknown while the send was open = %v; want [1]", during)
	}
	if r := outboxRow(t, db, dedupeKey); r.status != "sent" || r.outcomeUnknown != 0 || r.commentID.String != "comment-1" {
		t.Fatalf("row = %+v; want sent with outcome_unknown 0", r)
	}
}

// 受け入れ基準: Linear が応答した失敗で 0 に戻る。
func TestOutcomeUnknownClearedWhenLinearAnswersFailure(t *testing.T) {
	for name, fake := range map[string]*fakeLinear{
		"server error": {commentStatus: http.StatusInternalServerError, commentBody: `{}`, issueBody: issueJSON(false)},
		"rate limited": {commentStatus: http.StatusTooManyRequests, commentBody: `{}`, issueBody: issueJSON(false)},
		"auth":         {commentStatus: http.StatusUnauthorized, commentBody: `{}`, issueBody: issueJSON(false)},
	} {
		t.Run(name, func(t *testing.T) {
			db, worker, c, _ := setup(t, fake)
			var during []int
			fake.onCreate = func() { during = append(during, outcomeUnknownNow(t, db)) }

			sendDue(t, worker)
			r := outboxRow(t, db, dedupeKey)
			if len(during) != 1 || during[0] != 1 || r.attempts != 1 || r.outcomeUnknown != 0 {
				t.Fatalf("during = %v, row = %+v; want 1 during the send, then 0 with 1 attempt", during, r)
			}
			if r.status == "pending" {
				// The retry is sent without reading the comments, even by a
				// restarted Worker.
				c.advance(time.Hour)
				if err := restart(worker, c).SendDue(context.Background()); err != nil {
					t.Fatal(err)
				}
				if fake.commentReads != 0 || fake.commentCount() != 2 {
					t.Fatalf("reads = %d, creates = %d; want no read and a resend", fake.commentReads, fake.commentCount())
				}
			}
		})
	}
}

// 受け入れ基準: 接続前の失敗で 0 に戻る。The dialer reads outcome_unknown
// (already 1) and then fails without connecting.
func TestOutcomeUnknownClearedWhenConnectionIsNotMade(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment, issueBody: issueJSON(false)}
	db, worker, _, logs := setup(t, fake)
	var during []int
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		during = append(during, outcomeUnknownNow(t, db))
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}}
	keyPath := filepath.Join(t.TempDir(), linear.APIKeyFileName)
	if err := os.WriteFile(keyPath, []byte(keyCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := linear.NewClient(&http.Client{Transport: transport}, "https://linear.invalid/graphql", keyPath, worker.Logger)
	if err != nil {
		t.Fatal(err)
	}
	worker.Client = client

	sendDue(t, worker)

	// The 1st dial is the commentCreate; Issue() after the failure dials
	// again and also fails (not removed, so the row is retried).
	if len(during) == 0 || during[0] != 1 {
		t.Fatalf("outcome_unknown at dial = %v; want 1 first", during)
	}
	r := outboxRow(t, db, dedupeKey)
	if r.status != "pending" || r.attempts != 1 || r.outcomeUnknown != 0 {
		t.Fatalf("row = %+v; want pending, 1 attempt, outcome_unknown 0", r)
	}
	if strings.Contains(logs.String(), keyCanary) {
		t.Fatal("API key leaked into the log")
	}
}

// 受け入れ基準: 応答の前に接続を切るテストで 1 のまま残り、Worker を作り直した
// （再起動した）後の送信で先にコメントを読み、同じ dedupe_key のコメントがあれば
// 再送せず sent とそのコメント ID が記録される。
func TestCutConnectionKeepsOutcomeUnknownAndRestartedWorkerFindsComment(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment, issueBody: issueJSON(false),
		cutCreates: 1, postBeforeCut: true}
	db, worker, c, _ := setup(t, fake)

	sendDue(t, worker)
	if r := outboxRow(t, db, dedupeKey); r.status != "pending" || r.attempts != 1 || r.outcomeUnknown != 1 {
		t.Fatalf("after the cut send: row = %+v; want pending, 1 attempt, outcome_unknown 1", r)
	}

	c.advance(time.Minute)
	if err := restart(worker, c).SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.log, ",") != "create,read" || len(fake.posted) != 1 {
		t.Fatalf("requests = %v, comments on the Issue = %d; want a read and no resend", fake.log, len(fake.posted))
	}
	r := outboxRow(t, db, dedupeKey)
	if r.status != "sent" || r.commentID.String != "comment-cut" || r.outcomeUnknown != 0 || r.attempts != 1 {
		t.Fatalf("row = %+v; want sent with the found comment ID", r)
	}
	if events := notificationEvents(t, db); len(events) != 1 || events[0].kind != "notification_sent" {
		t.Fatalf("events = %+v", events)
	}
}

// 受け入れ基準: 同じ状況でコメントが無ければ1回だけ送る。
func TestCutConnectionRestartedWorkerResendsOnceWhenCommentMissing(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment, issueBody: issueJSON(false), cutCreates: 1}
	db, worker, c, _ := setup(t, fake)

	sendDue(t, worker)
	if r := outboxRow(t, db, dedupeKey); r.outcomeUnknown != 1 {
		t.Fatalf("after the cut send: row = %+v; want outcome_unknown 1", r)
	}
	c.advance(time.Minute)
	restarted := restart(worker, c)
	if err := restarted.SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	if err := restart(worker, c).SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}

	if strings.Join(fake.log, ",") != "create,read,create" {
		t.Fatalf("requests = %v; want one read and one resend", fake.log)
	}
	if n := len(fake.posted); n != 1 {
		t.Fatalf("comments on the Issue = %d; want 1", n)
	}
	r := outboxRow(t, db, dedupeKey)
	if r.status != "sent" || r.commentID.String != "comment-1" || r.attempts != 2 || r.outcomeUnknown != 0 {
		t.Fatalf("row = %+v; want sent on the 2nd attempt", r)
	}
}

// 受け入れ基準: 停止で送信を中断すると attempts は増えず、再起動の後に照合してから送る。
func TestShutdownDuringSendKeepsAttemptsAndChecksAfterRestart(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment, issueBody: issueJSON(false),
		holdCreate: true, createStarted: make(chan struct{}, 1)}
	db, worker, c, _ := setup(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.SendDue(ctx) }()
	select {
	case <-fake.createStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("commentCreate was not sent")
	}
	cancel() // Control Plane shutdown.
	if err := <-done; err == nil {
		t.Fatal("SendDue returned nil after shutdown interrupted the send")
	}
	r := outboxRow(t, db, dedupeKey)
	if r.status != "pending" || r.attempts != 0 || r.outcomeUnknown != 1 || r.nextAttemptAt.Valid {
		t.Fatalf("after shutdown: row = %+v; want pending, 0 attempts, outcome_unknown 1", r)
	}

	// The comment reached Linear although no response was read.
	fake.mu.Lock()
	fake.holdCreate = false
	fake.posted = append(fake.posted, linear.Comment{ID: "comment-held", Body: fake.comments[0]["body"].(string)})
	fake.mu.Unlock()
	if err := restart(worker, c).SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.log, ",") != "create,read" {
		t.Fatalf("requests = %v; want the comments read and no resend", fake.log)
	}
	r = outboxRow(t, db, dedupeKey)
	if r.status != "sent" || r.commentID.String != "comment-held" || r.attempts != 0 || r.outcomeUnknown != 0 {
		t.Fatalf("row = %+v; want sent with the found comment ID", r)
	}
}

// 受け入れ基準: コメントを読めなかったときは送らず attempts が1増える。
func TestUnreadableCommentsAddOneAttemptAndKeepOutcomeUnknown(t *testing.T) {
	fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment, issueBody: issueJSON(false),
		cutCreates: 1, commentsStatus: http.StatusInternalServerError}
	db, worker, c, _ := setup(t, fake)

	sendDue(t, worker)
	before := outboxRow(t, db, dedupeKey)
	c.advance(time.Minute)
	if err := restart(worker, c).SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := outboxRow(t, db, dedupeKey)
	if fake.commentCount() != 1 || strings.Join(fake.log, ",") != "create,read" {
		t.Fatalf("requests = %v; want a read and no resend", fake.log)
	}
	if r.status != "pending" || r.attempts != before.attempts+1 || r.outcomeUnknown != 1 {
		t.Fatalf("row = %+v (before %+v); want 1 more attempt and outcome_unknown 1", r, before)
	}
}

// 受け入れ基準: 照合の要否をプロセスの中の記憶で決めていない。Only the column
// decides: a fresh Worker reads first for a never-tried row whose column is 1,
// and does not read for a tried row whose column is 0.
func TestCheckDecidedByColumnOnly(t *testing.T) {
	t.Run("column 1, no attempts", func(t *testing.T) {
		fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment, issueBody: issueJSON(false)}
		db, worker, _, _ := setup(t, fake)
		mustExec(t, db, `UPDATE linear_outbox SET outcome_unknown = 1 WHERE action = 'comment'`)
		sendDue(t, worker)
		if strings.Join(fake.log, ",") != "read,create" {
			t.Fatalf("requests = %v; want read before create", fake.log)
		}
	})
	t.Run("column 0, tried before", func(t *testing.T) {
		fake := &fakeLinear{commentStatus: http.StatusOK, commentBody: okComment, issueBody: issueJSON(false)}
		db, worker, _, _ := setup(t, fake)
		mustExec(t, db, `UPDATE linear_outbox SET attempts = 2, outcome_unknown = 0 WHERE action = 'comment'`)
		sendDue(t, worker)
		if strings.Join(fake.log, ",") != "create" {
			t.Fatalf("requests = %v; want create without a read", fake.log)
		}
		if r := outboxRow(t, db, dedupeKey); r.status != "sent" || r.attempts != 3 {
			t.Fatalf("row = %+v", r)
		}
	})
}
