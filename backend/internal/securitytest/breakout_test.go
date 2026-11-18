package securitytest

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// breakoutChannel is a run channel stand-in: reply=false refuses authorize
// by closing the connection (as Runner does outside publish).
func breakoutChannel(t *testing.T, reply bool) string {
	t.Helper()
	dir, err := os.MkdirTemp("/private/tmp", "e7b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			if reply && strings.Contains(line, `"op": "authorize"`) {
				_, _ = conn.Write([]byte(`{"allowed":false,"reason":"approval_missing"}` + "\n"))
			}
			conn.Close()
		}
	}()
	return path
}

func runBreakout(t *testing.T, b Breakout, stage, channel string) []byte {
	t.Helper()
	dir := t.TempDir()
	script, result := filepath.Join(dir, BreakoutScriptName), filepath.Join(dir, BreakoutReport(stage))
	if err := os.WriteFile(script, b.Script, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", script, stage, result)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
	if channel != "" {
		cmd.Env = append(cmd.Env, "EVALOOM_CHANNEL="+channel)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("breakout script: %v %s", err, out)
	}
	report, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func ranReport(t *testing.T, ok bool) []byte {
	return encodeReport(t, []map[string]any{observation("breakout/run", "command", ok, map[string]any{})})
}

// The script attempts what its plan says, names the running stage in its
// requests, and carries no key value; the verdict catches each access that
// gets through, by the report and by the trusted records.
func TestBreakoutScriptAndVerdict(t *testing.T) {
	var mu sync.Mutex
	var log []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		log = append(log, r.RequestURI)
		mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer api.Close()
	targets := isolationTargets()
	targets.ControlAPI = []string{api.URL}
	b, err := NewBreakout(targets)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{targets.Secrets.Read, targets.Secrets.Write, targets.UndeclaredSecret} {
		if strings.Contains(string(b.Script), value) || strings.Contains(string(b.WorkPlan()), value) {
			t.Fatal("the breakout script carries a key value")
		}
	}
	d := declaredEverything()
	// Prepare accepts probe_read and the declared destination through the
	// proxy; check accepts nothing.
	for stage, either := range map[string][]string{IsolationCheck: nil, IsolationPrepare: {"probe_read/value", "declared_destination/declared-proxy"}} {
		for _, r := range b.Rules[stage] {
			if (r.Expect == Either) != slices.Contains(either, r.ID) || r.Expect == Require {
				t.Errorf("%s rule %+v", stage, r)
			}
		}
		if !slices.ContainsFunc(b.Rules[stage], func(r IsolationRule) bool { return r.Row == RowAuthorize }) {
			t.Errorf("%s has no authorize rule", stage)
		}
	}

	// On the host nothing is isolated: the Control API is reached, and the
	// authorize the channel answers counts as relayed.
	report := runBreakout(t, b, IsolationCheck, breakoutChannel(t, true))
	mu.Lock()
	records := TrustedRecords{ControlAPI: slices.Clone(log)}
	mu.Unlock()
	if len(records.ControlAPI) == 0 || !strings.Contains(records.ControlAPI[0], "stage=check&") {
		t.Fatalf("Control API log %v", records.ControlAPI)
	}
	v := BreakoutVerdict(IsolationCheck, ranReport(t, true), report, b.Rules[IsolationCheck], d, records)
	for _, row := range []string{RowControlAPI, RowAuthorize} {
		if !slices.Contains(Rows(v), row) {
			t.Errorf("%s not detected: %v", row, v)
		}
	}

	// Runner's refusal (closing without a reply) is a denied authorize.
	only, err := NewBreakout(targets, RowAuthorize, RowProbeWrite)
	if err != nil {
		t.Fatal(err)
	}
	report = runBreakout(t, only, IsolationPrepare, breakoutChannel(t, false))
	if v := BreakoutVerdict(IsolationPrepare, ranReport(t, true), report, only.Rules[IsolationPrepare], d, TrustedRecords{}); len(v) != 0 {
		t.Fatalf("refused authorize rejected: %v", v)
	}
	// A channel that was never reached is unavailable, not a refusal.
	report = runBreakout(t, only, IsolationPrepare, "")
	if v := BreakoutVerdict(IsolationPrepare, ranReport(t, true), report, only.Rules[IsolationPrepare], d, TrustedRecords{}); len(v) == 0 || !strings.Contains(v[0].Detail, "実行不能") {
		t.Fatalf("unreached channel accepted: %v", v)
	}
}

func TestBreakoutVerdictUsesTrustedRecords(t *testing.T) {
	d := declaredEverything()
	b, err := NewBreakout(isolationTargets())
	if err != nil {
		t.Fatal(err)
	}
	conforming := func(stage string) []map[string]any {
		obs := conformingReport(stage, b.Rules[stage], d)
		for _, o := range obs {
			if o["kind"] == "authorize" {
				o["detail"] = map[string]any{"connected": true, "replied": false}
			}
		}
		return obs
	}
	for _, stage := range BreakoutStages {
		records := TrustedRecords{ControlAPI: []string{"/api/runner/claim"}, Destinations: map[string][]string{RowDeclaredDest: {"/" + BreakoutScriptName + "?"}}}
		if v := BreakoutVerdict(stage, ranReport(t, true), encodeReport(t, conforming(stage)), b.Rules[stage], d, records); len(v) != 0 {
			t.Fatalf("%s conforming run rejected: %v", stage, v)
		}
		// The report claims isolation; the trusted records disagree.
		records = TrustedRecords{ControlAPI: []string{"/api/executions/e1/authorize", "/api/tasks?stage=" + stage + "&probe=owner"},
			Destinations: map[string][]string{RowModelAPI: {IsolationProbePath + "?stage=" + stage + "&probe=" + RowModelAPI}}}
		v := BreakoutVerdict(stage, ranReport(t, true), encodeReport(t, conforming(stage)), b.Rules[stage], d, records)
		for _, row := range []string{RowAuthorize, RowControlAPI, RowModelAPI} {
			if !slices.Contains(Rows(v), row) {
				t.Errorf("%s: record of %s not detected: %v", stage, row, v)
			}
		}
		// The stage must have run the script, and the script must report.
		if v := BreakoutVerdict(stage, ranReport(t, false), encodeReport(t, conforming(stage)), b.Rules[stage], d, TrustedRecords{}); len(v) == 0 {
			t.Errorf("%s: script not run accepted", stage)
		}
		if v := BreakoutVerdict(stage, nil, encodeReport(t, conforming(stage)), b.Rules[stage], d, TrustedRecords{}); len(v) == 0 {
			t.Errorf("%s: missing stage report accepted", stage)
		}
		if v := BreakoutVerdict(stage, ranReport(t, true), nil, b.Rules[stage], d, TrustedRecords{}); len(v) == 0 {
			t.Errorf("%s: missing script report accepted", stage)
		}
	}
	// Check reaches nowhere: even the declared destination is a violation.
	records := TrustedRecords{Destinations: map[string][]string{RowDeclaredDest: {IsolationProbePath + "?stage=check&probe=" + RowDeclaredDest}}}
	if v := BreakoutVerdict(IsolationCheck, ranReport(t, true), encodeReport(t, conforming(IsolationCheck)), b.Rules[IsolationCheck], d, records); !slices.Contains(Rows(v), RowDeclaredDest) {
		t.Fatalf("check reaching the declared destination accepted: %v", v)
	}
	// probe_write found in prepare is a violation.
	obs := conforming(IsolationPrepare)
	for _, o := range obs {
		if o["id"] == "probe_write/value" {
			o["succeeded"] = true
		}
	}
	if v := BreakoutVerdict(IsolationPrepare, ranReport(t, true), encodeReport(t, obs), b.Rules[IsolationPrepare], d, TrustedRecords{}); !slices.Contains(Rows(v), RowProbeWrite) {
		t.Fatalf("probe_write in prepare accepted: %v", v)
	}
}
