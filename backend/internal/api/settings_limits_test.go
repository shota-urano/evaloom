package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/registry"
)

const defaultSettingsJSON = `{"linear":{"triggerLabel":"ai-ready","routingLabelPrefix":"route:"},` +
	`"routing":{"proposer":{"microHarness":"claude_code","model":null},"defaultRunnerId":null},` +
	`"limits":{"stageTimeoutMaxSec":{"prepare":1800,"work":14400,"check":3600,"publish":1800}},` +
	`"approval":{"ttlSec":86400}}` + "\n"

// Without rows, GET /api/settings serves the defaults of spec 02 3.8 for
// all six keys, limits and approval included.
func TestSettingsLimitsAndApprovalDefaults(t *testing.T) {
	f := newAuthFixture(t)
	status, body := getJSON(t, f, "/api/settings")
	if status != 200 || string(body) != defaultSettingsJSON {
		t.Fatalf("GET status=%d body=%s", status, body)
	}
}

// PATCH replaces limits.stageTimeoutMaxSec and approval.ttlSec, stores them
// in the shape their readers (Profile registration, the approval decision)
// parse, and writes one settings.update audit row per changed key. Other
// keys keep their values.
func TestSettingsLimitsAndApprovalPatchStoresAndAudits(t *testing.T) {
	f := newAuthFixture(t)
	status, body := projectRequest(t, f, "PATCH", "/api/settings", map[string]any{
		"limits":   map[string]any{"stageTimeoutMaxSec": map[string]any{"prepare": 60, "work": 7200, "check": 600, "publish": 300}},
		"approval": map[string]any{"ttlSec": 600},
	})
	want := strings.Replace(strings.Replace(defaultSettingsJSON,
		`{"prepare":1800,"work":14400,"check":3600,"publish":1800}`, `{"prepare":60,"work":7200,"check":600,"publish":300}`, 1),
		`"ttlSec":86400`, `"ttlSec":600`, 1)
	if status != 200 || string(body) != want {
		t.Fatalf("PATCH status=%d body=%s", status, body)
	}
	if _, body = getJSON(t, f, "/api/settings"); string(body) != want {
		t.Fatalf("GET body=%s", body)
	}
	state := settingsState{
		rows: map[string]string{
			"limits.stageTimeoutMaxSec": `{"prepare":60,"work":7200,"check":600,"publish":300}`,
			"approval.ttlSec":           `600`,
		},
		audits: []string{
			`owner settings limits.stageTimeoutMaxSec {"after":{"prepare":60,"work":7200,"check":600,"publish":300},"before":{"prepare":1800,"work":14400,"check":3600,"publish":1800}}`,
			`owner settings approval.ttlSec {"after":600,"before":86400}`,
		},
	}
	if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, state) {
		t.Fatalf("state = %+v; want %+v", got, state)
	}
	// Profile registration reads the stored row with the same parser.
	limits := registry.ParseStageTimeoutMaxSec(state.rows["limits.stageTimeoutMaxSec"], true)
	if want := (map[model.Stage]int{model.StagePrepare: 60, model.StageWork: 7200, model.StageCheck: 600, model.StagePublish: 300}); !reflect.DeepEqual(limits, want) {
		t.Fatalf("registry limits = %v", limits)
	}

	// The same values: nothing is written.
	status, _ = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{
		"limits":   map[string]any{"stageTimeoutMaxSec": map[string]any{"prepare": 60, "work": 7200, "check": 600, "publish": 300}},
		"approval": map[string]any{"ttlSec": 600},
	})
	if got := readSettingsState(t, f.db); status != 200 || !reflect.DeepEqual(got, state) {
		t.Fatalf("same-value PATCH status=%d state=%+v", status, got)
	}

	// Changing only the TTL leaves limits and the linear and routing keys alone.
	status, body = projectRequest(t, f, "PATCH", "/api/settings", map[string]any{"approval": map[string]any{"ttlSec": 3600}})
	if status != 200 || !strings.Contains(string(body), `"stageTimeoutMaxSec":{"prepare":60,"work":7200,"check":600,"publish":300}`) ||
		!strings.Contains(string(body), `"linear":{"triggerLabel":"ai-ready","routingLabelPrefix":"route:"}`) {
		t.Fatalf("TTL PATCH status=%d body=%s", status, body)
	}
	state.rows["approval.ttlSec"] = `3600`
	state.audits = append(state.audits, `owner settings approval.ttlSec {"after":3600,"before":600}`)
	if got := readSettingsState(t, f.db); !reflect.DeepEqual(got, state) {
		t.Fatalf("state = %+v; want %+v", got, state)
	}
}

// A value of the wrong type, a non-positive number, a missing or unknown
// stage, or null is 400, and nothing of the request (other keys included)
// is written.
func TestSettingsLimitsAndApprovalRejectInvalidValues(t *testing.T) {
	stages := func(override map[string]any) map[string]any {
		value := map[string]any{"prepare": 60, "work": 60, "check": 60, "publish": 60}
		for k, v := range override {
			if v == "<delete>" {
				delete(value, k)
			} else {
				value[k] = v
			}
		}
		return value
	}
	for _, test := range []struct {
		name          string
		body          map[string]any
		field, reason string
	}{
		{"ttl string", map[string]any{"approval": map[string]any{"ttlSec": "600"}}, "approval.ttlSec", "invalid"},
		{"ttl fraction", map[string]any{"approval": map[string]any{"ttlSec": 1.5}}, "approval.ttlSec", "invalid"},
		{"ttl bool", map[string]any{"approval": map[string]any{"ttlSec": true}}, "approval.ttlSec", "invalid"},
		{"ttl zero", map[string]any{"approval": map[string]any{"ttlSec": 0}}, "approval.ttlSec", "out_of_range"},
		{"ttl negative", map[string]any{"approval": map[string]any{"ttlSec": -1}}, "approval.ttlSec", "out_of_range"},
		{"ttl null", map[string]any{"approval": map[string]any{"ttlSec": nil}}, "approval.ttlSec", "required"},
		{"approval unknown key", map[string]any{"approval": map[string]any{"ttl": 600}}, "body", "invalid_json"},
		{"approval not object", map[string]any{"approval": 600}, "approval", "invalid"},
		{"limits null", map[string]any{"limits": map[string]any{"stageTimeoutMaxSec": nil}}, "limits.stageTimeoutMaxSec", "required"},
		{"limits not object", map[string]any{"limits": map[string]any{"stageTimeoutMaxSec": 60}}, "limits.stageTimeoutMaxSec", "invalid"},
		{"limits unknown key", map[string]any{"limits": map[string]any{"maxSec": 60}}, "body", "invalid_json"},
		{"stage missing", map[string]any{"limits": map[string]any{"stageTimeoutMaxSec": stages(map[string]any{"check": "<delete>"})}}, "limits.stageTimeoutMaxSec.check", "required"},
		{"stage unknown", map[string]any{"limits": map[string]any{"stageTimeoutMaxSec": stages(map[string]any{"review": 60})}}, "body", "invalid_json"},
		{"stage string", map[string]any{"limits": map[string]any{"stageTimeoutMaxSec": stages(map[string]any{"work": "60"})}}, "limits.stageTimeoutMaxSec.work", "invalid"},
		{"stage fraction", map[string]any{"limits": map[string]any{"stageTimeoutMaxSec": stages(map[string]any{"work": 0.5})}}, "limits.stageTimeoutMaxSec.work", "invalid"},
		{"stage zero", map[string]any{"limits": map[string]any{"stageTimeoutMaxSec": stages(map[string]any{"publish": 0})}}, "limits.stageTimeoutMaxSec.publish", "out_of_range"},
		{"stage null", map[string]any{"limits": map[string]any{"stageTimeoutMaxSec": stages(map[string]any{"prepare": nil})}}, "limits.stageTimeoutMaxSec.prepare", "required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAuthFixture(t)
			body := map[string]any{"linear": map[string]any{"triggerLabel": "go"}}
			for k, v := range test.body {
				body[k] = v
			}
			status, response := projectRequest(t, f, "PATCH", "/api/settings", body)
			requireProjectError(t, status, response, 400, ValidationFailed, test.field, test.reason)
			if got := readSettingsState(t, f.db); len(got.rows) != 0 || len(got.audits) != 0 {
				t.Fatalf("rejected PATCH wrote %+v", got)
			}
		})
	}
}

// A settings row and its audit row commit together: a failure of either
// write rolls back every key of the PATCH.
func TestSettingsLimitsAndApprovalRollBackOnWriteFailure(t *testing.T) {
	for _, test := range []struct{ name, trigger string }{
		{"settings", `CREATE TRIGGER fail_write BEFORE INSERT ON settings
			WHEN NEW.key='approval.ttlSec' BEGIN SELECT RAISE(ABORT, 'write failed'); END`},
		{"audit", `CREATE TRIGGER fail_write BEFORE INSERT ON audit_logs
			WHEN NEW.target_id='limits.stageTimeoutMaxSec' BEGIN SELECT RAISE(ABORT, 'audit failed'); END`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAuthFixture(t)
			execSQL(t, f, test.trigger)
			status, body := projectRequest(t, f, "PATCH", "/api/settings", map[string]any{
				"linear":   map[string]any{"triggerLabel": "go"},
				"limits":   map[string]any{"stageTimeoutMaxSec": map[string]any{"prepare": 60, "work": 60, "check": 60, "publish": 60}},
				"approval": map[string]any{"ttlSec": 600},
			})
			if status != 500 {
				t.Fatalf("failed write status=%d body=%s", status, body)
			}
			if got := readSettingsState(t, f.db); len(got.rows) != 0 || len(got.audits) != 0 {
				t.Fatalf("failed write left %+v", got)
			}
		})
	}
}

// A stored row the readers cannot fully use is shown as they read it: a
// stage without a positive integer allows no timeout (0), an unusable TTL
// falls back to the default.
func TestSettingsUnusableLimitsAndApprovalRows(t *testing.T) {
	f := newAuthFixture(t)
	execSQL(t, f, `INSERT INTO settings VALUES ('limits.stageTimeoutMaxSec', '{"prepare":10,"work":"x","check":-1}', 't')`)
	execSQL(t, f, `INSERT INTO settings VALUES ('approval.ttlSec', '"soon"', 't')`)
	_, body := getJSON(t, f, "/api/settings")
	var view struct {
		Limits   struct{ StageTimeoutMaxSec map[string]int } `json:"limits"`
		Approval struct{ TTLSec int64 }                      `json:"approval"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if want := (map[string]int{"prepare": 10, "work": 0, "check": 0, "publish": 0}); !reflect.DeepEqual(view.Limits.StageTimeoutMaxSec, want) || view.Approval.TTLSec != 86400 {
		t.Fatalf("GET body=%s", body)
	}
}

// approval.ttlSec set through PATCH is what an approval without ttlSec uses
// for its expiry (spec 17 Q55, 02 Q200).
func TestSettingsApprovalTTLIsUsedByApprovalDecision(t *testing.T) {
	f, _, _, _, approval := waitingApprovalFixture(t)
	status, body := approvalRequest(t, f, "PATCH", "/api/settings", `{"approval":{"ttlSec":600}}`)
	if status != 200 {
		t.Fatalf("PATCH status=%d body=%s", status, body)
	}
	var id, contentID string
	if err := f.db.QueryRow(`SELECT decision_id, content_id FROM approvals WHERE id=?`, approval.ID).Scan(&id, &contentID); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	status, body = approvalRequest(t, f, "POST", "/api/decisions/"+id+"/answer", `{"action":"approve","contentId":"`+contentID+`"}`)
	if status != 200 {
		t.Fatalf("answer=%d %s", status, body)
	}
	status, body = approvalRequest(t, f, "GET", "/api/approvals/"+approval.ID, "")
	view := decodeApproval(t, status, body, 200)
	if view.ExpiresAt == nil || view.ExpiresAt.Before(before.Add(600*time.Second)) || view.ExpiresAt.After(time.Now().UTC().Add(600*time.Second)) {
		t.Fatalf("approval=%s", body)
	}
}
