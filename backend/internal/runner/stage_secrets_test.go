package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

func TestExecutionKeyPreflightStopsBeforeExecutor(t *testing.T) {
	for _, access := range []string{"missing", "write", "read"} {
		t.Run(access, func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, true)
			var def contract.ProfileDefinition
			if err := json.Unmarshal(claim.Profile.Definition, &def); err != nil {
				t.Fatal(err)
			}
			def.Stages.Prepare.Secrets = []contract.Secret{{Slot: "source", Access: "read"}}
			claim.Profile.Definition, _ = json.Marshal(def)
			if access != "missing" {
				if err := secretstore.New(r.dataDir).Set(claim.Profile.HarnessID, "source", access, []byte("source-value")); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
				calls++
				if access != "read" {
					t.Fatal("invalid key reached the executor")
				}
				spec, err := in.SandboxSpec(sandbox.SandboxSpec{Kind: sandbox.Prepare})
				if err != nil || spec.Secrets["source"] != "source-value" {
					t.Fatalf("selected read key missing: %v", err)
				}
				return successfulStage(in.Stage)
			})
			reporter.next = func(model.Stage, StageOutcome) StageNext { return StageNext{Next: "stop"} }
			if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
				t.Fatal(err)
			}
			outcome := reporter.outcomes[0]
			if access == "read" {
				if calls != 1 || outcome.RunnerError != nil {
					t.Fatalf("read key did not start: %+v", outcome)
				}
			} else if calls != 0 || outcome.RunnerError == nil || *outcome.RunnerError != "config_error" {
				t.Fatalf("key error did not fail closed: calls=%d outcome=%+v", calls, outcome)
			}
		})
	}
}

type secretRuntimeCall struct{ Args, Env []string }

// A fake Docker CLI imports name-only --env on create, then the workload
// deliberately prints the imported values on both output streams. Its daemon
// state is test-only, outside every mount, just like sandbox's runtime fixture.
func TestSecretRuntimeHelper(t *testing.T) {
	root := os.Getenv("SECRET_RUNTIME_ROOT")
	if root == "" {
		return
	}
	index := slices.Index(os.Args, "--")
	if index < 0 {
		os.Exit(90)
	}
	args := os.Args[index+1:]
	data, _ := json.Marshal(secretRuntimeCall{args, os.Environ()})
	file, err := os.OpenFile(filepath.Join(root, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(91)
	}
	_, _ = file.Write(append(data, '\n'))
	_ = file.Close()
	if args[0] == "image" {
		fmt.Print(`{"Os":"linux","Config":{"Env":["EVALOOM_SECRET_UNDECLARED=image-key","PATH=/image/bin"]}}`)
		os.Exit(0)
	}
	switch args[1] {
	case "create":
		var values []string
		for i, arg := range args {
			if arg == "--env" {
				values = append(values, os.Getenv(args[i+1]))
			}
		}
		data, _ := json.Marshal(values)
		if os.WriteFile(filepath.Join(root, "values"), data, 0600) != nil {
			os.Exit(92)
		}
	case "start":
		data, err := os.ReadFile(filepath.Join(root, "values"))
		if err != nil {
			os.Exit(93)
		}
		var values []string
		if json.Unmarshal(data, &values) != nil {
			os.Exit(94)
		}
		for _, value := range values {
			fmt.Fprintln(os.Stdout, "stdout="+value)
			fmt.Fprintln(os.Stderr, "stderr="+value)
			encoded, _ := json.Marshal(value)
			fmt.Fprintln(os.Stdout, "escaped="+string(encoded))
			fmt.Fprintln(os.Stderr, "escaped="+string(encoded))
		}
	case "inspect":
		fmt.Print(`{"Status":"exited","Running":false,"ExitCode":0}`)
	case "rm":
	default:
		os.Exit(95)
	}
	os.Exit(0)
}

func secretRuntime(t *testing.T, r *Runner, in StageInvocation, paths contract.RequestPaths, stdout, stderr io.Writer) (*sandbox.Container, string) {
	t.Helper()
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'" }
	runtime := filepath.Join(root, "runtime")
	if err := os.WriteFile(runtime, []byte("#!/bin/sh\nexec "+quote(executable)+" -test.run='^TestSecretRuntimeHelper$' -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	roots := map[string]string{
		paths.Profile: in.Profile.Path, paths.Workspace: in.Workspace,
		paths.Out: filepath.Join(in.Directory, "out"), paths.Inputs: filepath.Join(in.Directory, "inputs"),
		paths.Scratch: filepath.Join(in.Directory, "scratch"), paths.Records: filepath.Join(root, "records"),
	}
	if err := os.Mkdir(roots[paths.Records], 0700); err != nil {
		t.Fatal(err)
	}
	box, err := sandbox.NewContainer(sandbox.ContainerConfig{
		RuntimePath: runtime, Image: "test-image", RuntimeEnv: map[string]string{"SECRET_RUNTIME_ROOT": root, "EVALOOM_SECRET_UNDECLARED": "runtime-key"},
		HostHome: filepath.Join(root, "home"), DataDir: r.dataDir, RunnerTokenFile: filepath.Join(root, "runner-token"),
		StagePaths: paths, StageMountRoots: roots, ControlTimeout: 5 * time.Second, Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	return box, root
}

func TestExecutionKeysRedactedAcrossRuntimeLogsEventsAndArtifacts(t *testing.T) {
	for _, test := range []struct{ name, key string }{
		{"text", "read-value-with-$(literal)=日本語"},
		{"JSON object", `{"client_id":"client","client_secret":"private-value"}`},
		{"escaped JSON string", "quoted\"key\n秘密"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testExecutionKeysRedactedAcrossRuntimeLogsEventsAndArtifacts(t, test.key)
		})
	}
}

func testExecutionKeysRedactedAcrossRuntimeLogsEventsAndArtifacts(t *testing.T, readKey string) {
	t.Helper()
	f := newLogControlPlane(t)
	live, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	r, claim, reporter, services := stageFixture(t, true)
	r.token, r.controlPlaneURL, r.client = live.token, f.server.URL, f.server.Client()
	claim.Execution.ID = logExecution
	var def contract.ProfileDefinition
	if err := json.Unmarshal(claim.Profile.Definition, &def); err != nil {
		t.Fatal(err)
	}
	def.Stages.Prepare.Secrets = []contract.Secret{{Slot: "source", Access: "read"}}
	claim.Profile.Definition, _ = json.Marshal(def)
	const writeKey = "delivery-private-value"
	if err := secretstore.New(r.dataDir).Set(claim.Profile.HarnessID, "source", "read", []byte(readKey)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EVALOOM_SECRET_HOST", "host-key")
	var saved []StageInvocation
	var runtimeRoots, localLogs []string
	services.Executor = stageExecutorFunc(func(ctx context.Context, in StageInvocation) StageOutcome {
		saved = append(saved, in)
		log, err := in.NewLog(nil)
		if err != nil {
			t.Fatal(err)
		}
		localLogs = append(localLogs, log.Path())
		// The real Container streams stdout/stderr concurrently to these pipes.
		outReader, outWriter := io.Pipe()
		errReader, errWriter := io.Pipe()
		done := make(chan error, 2)
		go func() { done <- log.Read(outReader) }()
		go func() { done <- log.Read(errReader) }()
		box, runtimeRoot := secretRuntime(t, r, in, services.Paths, outWriter, errWriter)
		runtimeRoots = append(runtimeRoots, runtimeRoot)
		p := services.Paths
		spec := sandbox.SandboxSpec{Kind: sandbox.SandboxKind(in.Stage), Limits: sandbox.ResourceLimits{CPU: 1, MemoryBytes: 1024 * 1024, Time: 10 * time.Second},
			Env: map[string]string{"PATH": "/usr/bin:/bin", "EVALOOM_SECRET_UNDECLARED": "spec-key"}}
		roles := []string{p.Out, p.Scratch, p.Workspace}
		if in.Stage == model.StageWork {
			roles = append(roles, p.Inputs, p.Records)
		} else {
			roles = append(roles, p.Profile)
			if in.Stage != model.StagePrepare {
				roles = append(roles, p.Inputs)
			}
		}
		for _, target := range roles {
			host := map[string]string{p.Out: filepath.Join(in.Directory, "out"), p.Scratch: filepath.Join(in.Directory, "scratch"), p.Workspace: in.Workspace, p.Inputs: filepath.Join(in.Directory, "inputs"), p.Records: filepath.Join(runtimeRoot, "records"), p.Profile: in.Profile.Path}[target]
			spec.Mounts = append(spec.Mounts, sandbox.Mount{HostPath: host, GuestPath: target, Writable: target == p.Out || target == p.Scratch || (target == p.Workspace && (in.Stage == model.StagePrepare || in.Stage == model.StageWork))})
		}
		spec, err = in.SandboxSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		process, err := box.Run(ctx, spec, []string{"/bin/probe"})
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Wait(); err != nil {
			t.Fatal(err)
		}
		_ = outWriter.Close()
		_ = errWriter.Close()
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		if err := log.Finish(false); err != nil {
			t.Fatal(err)
		}
		if len(log.secrets) != 0 {
			t.Fatal("closed log retains keys")
		}
		if err := log.Deliver(ctx); err != nil {
			t.Fatal(err)
		}
		outcome := successfulStage(in.Stage)
		key := ""
		if in.Stage == model.StagePrepare {
			key = readKey
		} else if in.Stage == model.StagePublish {
			key = writeKey
		}
		if key != "" {
			if err := os.WriteFile(filepath.Join(in.Directory, "out", "leak"), []byte("out="+key), 0600); err != nil {
				t.Fatal(err)
			}
			outcome.Result, _ = json.Marshal(map[string]string{"stage": string(in.Stage), "message": key})
			if err := os.WriteFile(filepath.Join(in.Directory, "result.json"), outcome.Result, 0600); err != nil {
				t.Fatal(err)
			}
			for _, output := range []struct {
				kind, name string
				data       []byte
			}{{"out_file", "leak", []byte("out=" + key)}, {"result_json", "result.json", outcome.Result}} {
				artifact, err := in.CollectArtifact(output.kind, output.name, bytes.NewReader(output.data))
				if err != nil {
					t.Fatal(err)
				}
				if !artifact.Redacted {
					t.Fatal("artifact lacks redacted metadata")
				}
				if err := r.registerArtifact(ctx, artifact); err != nil {
					t.Fatal(err)
				}
			}
		}
		return outcome
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := r.ExecuteStages(ctx, claim, services); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reporter.finished, executionStageOrder) {
		t.Fatalf("stages=%v", reporter.finished)
	}
	for _, outcome := range reporter.outcomes {
		if outcome.RunnerError != nil {
			t.Fatalf("stage failed: %+v", outcome)
		}
	}
	for i, in := range saved {
		if _, err := in.SandboxSpec(sandbox.SandboxSpec{Kind: sandbox.SandboxKind(in.Stage)}); err == nil {
			t.Fatal("finished invocation retains key capability")
		}
		data, err := os.ReadFile(filepath.Join(runtimeRoots[i], "calls"))
		if err != nil {
			t.Fatal(err)
		}
		creates := 0
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			var call secretRuntimeCall
			if err := json.Unmarshal(line, &call); err != nil {
				t.Fatal(err)
			}
			args := strings.Join(call.Args, "\x00")
			if strings.Contains(args, readKey) || strings.Contains(args, writeKey) {
				t.Fatal("key exposed in runtime arguments")
			}
			if len(call.Args) > 1 && call.Args[1] == "create" {
				creates++
				want := map[model.Stage]string{model.StagePrepare: "EVALOOM_SECRET_SOURCE=" + readKey, model.StagePublish: "EVALOOM_SECRET_DELIVERY=" + writeKey}[in.Stage]
				count := 0
				for _, entry := range call.Env {
					if strings.HasPrefix(entry, "EVALOOM_SECRET_") {
						count++
						if entry != want {
							t.Fatal("unexpected key in create environment")
						}
					}
				}
				if (want == "" && count != 0) || (want != "" && count != 1) {
					t.Fatalf("wrong keys for %s", in.Stage)
				}
			}
		}
		if creates != 1 {
			t.Fatalf("runtime create count=%d", creates)
		}
		for _, path := range []string{localLogs[i], filepath.Join(in.Directory, "out", "leak"), filepath.Join(in.Directory, "result.json")} {
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			assertNoStageKeys(t, data, readKey, writeKey)
			if (in.Stage == model.StagePrepare || in.Stage == model.StagePublish) && !bytes.Contains(data, []byte("[REDACTED]")) {
				t.Fatalf("missing replacement in %s", path)
			}
		}
	}
	rows, err := f.db.Query(`SELECT payload FROM execution_events WHERE type='log'`)
	if err != nil {
		t.Fatal(err)
	}
	events := 0
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		assertNoStageKeys(t, payload, readKey, writeKey)
		if !bytes.Contains(payload, []byte("[REDACTED]")) {
			t.Fatal("event missing replacement")
		}
		events++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if events != 8 {
		t.Fatalf("stdout/stderr event count=%d", events)
	}
	rows, err = f.db.Query(`SELECT storage_path, redacted, stage FROM artifacts`)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := 0
	for rows.Next() {
		var path, stage string
		var redacted int
		if err := rows.Scan(&path, &redacted, &stage); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(f.dataDir, path))
		if err != nil {
			t.Fatal(err)
		}
		assertNoStageKeys(t, data, readKey, writeKey)
		if (stage == "prepare" || stage == "publish") && (redacted != 1 || !bytes.Contains(data, []byte("[REDACTED]"))) {
			t.Fatal("stored artifact not redacted")
		}
		artifacts++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if artifacts != 8 {
		t.Fatalf("artifact count=%d", artifacts)
	}
	for _, outcome := range reporter.outcomes {
		assertNoStageKeys(t, outcome.Result, readKey, writeKey)
	}
}

func assertNoStageKeys(t *testing.T, data []byte, values ...string) {
	t.Helper()
	for _, value := range values {
		for _, literal := range secretLiterals([]string{value}) {
			if bytes.Contains(data, []byte(literal)) {
				t.Fatal("stage key persisted")
			}
		}
	}
}

func TestArtifactJSONRedactionPreservesStructure(t *testing.T) {
	key := "quoted\"key\n秘密"
	raw := []byte(`{"n":9007199254740993,"nested":[{"value":"quoted\"key\n\u79d8\u5bc6"}],"plain":"keep"}`)
	clean := redactStageBytes(raw, []string{key})
	want := `{"n":9007199254740993,"nested":[{"value":"[REDACTED]"}],"plain":"keep"}`
	if string(clean) != want {
		t.Fatalf("JSON changed incorrectly: %s", clean)
	}
	for _, test := range []struct{ raw, want, key string }{
		{`{"message":"secret","message":"keep"}`, `{"message":"[REDACTED]","message":"keep"}`, "secret"},
		{" {\r\n\"message\" : \"secret\" } ", " {\r\n\"message\" : \"[REDACTED]\" } ", "secret"},
		// Q203 replaces matching objects, arrays and numbers as whole values.
		{`{"value":123456}`, `{"value":"[REDACTED]"}`, "123456"},
		{`{"value":true}`, `{"value":true}`, "true"},
		{`123456`, `"[REDACTED]"`, "123456"},
		{`{"key":{}}`, `{"key":"[REDACTED]"}`, `{}`},
		{`{"key":["private",123]}`, `{"key":"[REDACTED]"}`, `["private",123]`},
		{" \n{\"private\":123}\r\n", " \n\"[REDACTED]\"\r\n", " \n{\"private\":123}\r\n"},
	} {
		if got := string(redactStageBytes([]byte(test.raw), []string{test.key})); got != test.want {
			t.Fatalf("JSON exclusion got=%s want=%s", got, test.want)
		}
	}
}

func TestArtifactSecretRepresentations(t *testing.T) {
	const key = "quoted\"key\n秘密"
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, representation := range []string{key, string(encoded), string(encoded[1 : len(encoded)-1])} {
		raw := "credential=" + representation + "\r\nkeep"
		want := "credential=[REDACTED]\r\nkeep"
		if representation == string(encoded) {
			want = "credential=\"[REDACTED]\"\r\nkeep"
		}
		if got := string(redactStageBytes([]byte(raw), []string{key})); got != want {
			t.Errorf("representation %q: got %q, want %q", representation, got, want)
		}
	}
}

func TestExecutionJSONKeysRedactedFromArtifactsAndResults(t *testing.T) {
	const key = `{"client_id":"client","client_secret":"private-value"}`
	objectString, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	const quotedKey = "quoted\"key\n秘密"
	quoted, err := json.Marshal(quotedKey)
	if err != nil {
		t.Fatal(err)
	}
	escaped, err := json.Marshal(string(quoted[1 : len(quoted)-1]))
	if err != nil {
		t.Fatal(err)
	}
	completeString, err := json.Marshal(string(quoted))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, key, raw, want string }{
		{"object credential", key, key, `"[REDACTED]"`},
		{"nested object credential", key, `{"credentials":` + key + `,"keep":9007199254740993}`, `{"credentials":"[REDACTED]","keep":9007199254740993}`},
		{"object credential in array", key, `{"credentials":[` + key + `,` + key + `],"keep":true}`, `{"credentials":["[REDACTED]","[REDACTED]"],"keep":true}`},
		{"reordered and escaped object credential", key, `{"credentials":{"client_secret":"private\u002dvalue", "client_id":"client"},"keep":true}`, `{"credentials":"[REDACTED]","keep":true}`},
		{"array credential", `["private",123]`, `["private",123]`, `"[REDACTED]"`},
		{"nested array credential", `["private",123]`, `{"credentials":[["private",123],["private",123]],"keep":true}`, `{"credentials":["[REDACTED]","[REDACTED]"],"keep":true}`},
		{"number credential", "123456", `{"value":123456,"keep":123457}`, `{"value":"[REDACTED]","keep":123457}`},
		{"root number credential", "123456", `123456`, `"[REDACTED]"`},
		{"number credential in array", "9007199254740993", `[9007199254740993,9007199254740992,1e400]`, `["[REDACTED]",9007199254740992,1e400]`},
		{"object credential in root string", key, string(objectString), `"[REDACTED]"`},
		{"object credential in object string", key, `{"credentials":` + string(objectString) + `,"keep":9007199254740993}`, `{"credentials":"[REDACTED]","keep":9007199254740993}`},
		{"object credential in array strings", key, `{"credentials":[` + string(objectString) + `,` + string(objectString) + `],"keep":true}`, `{"credentials":["[REDACTED]","[REDACTED]"],"keep":true}`},
		{"credential inside object", "private-value", key, `{"client_id":"client","client_secret":"[REDACTED]"}`},
		{"root string", quotedKey, string(quoted), `"[REDACTED]"`},
		{"object string", quotedKey, `{"credential":` + string(quoted) + `,"keep":9007199254740993}`, `{"credential":"[REDACTED]","keep":9007199254740993}`},
		{"array string", quotedKey, `[` + string(quoted) + `,"keep"]`, `["[REDACTED]","keep"]`},
		{"nested string", quotedKey, `{"nested":[{"credential":` + string(quoted) + `}],"keep":true}`, `{"nested":[{"credential":"[REDACTED]"}],"keep":true}`},
		{"object name", quotedKey, `{` + string(quoted) + `:"keep",` + string(quoted) + `:"duplicate"}`, `{"[REDACTED]":"keep","[REDACTED]":"duplicate"}`},
		{"unicode root string", quotedKey, `"quoted\u0022key\u000a\u79d8\u5bc6"`, `"[REDACTED]"`},
		{"unicode nested string", quotedKey, `{"nested":["before quoted\"key\n\u79d8\u5bc6 after"]}`, `{"nested":["before [REDACTED] after"]}`},
		{"escaped value in root string", quotedKey, string(escaped), `"[REDACTED]"`},
		{"escaped value in nested string", quotedKey, `{"nested":[` + string(escaped) + `]}`, `{"nested":["[REDACTED]"]}`},
		{"complete string credential", string(quoted), string(completeString), `"[REDACTED]"`},
		{"authorization result", "private-token", `{"contractVersion":"1","stage":"prepare","status":"succeeded","message":"Authorization: Bearer private-token"}`, `{"contractVersion":"1","stage":"prepare","status":"succeeded","message":"Authorization: [REDACTED]"}`},
		{"authorization pattern without literal match", "unrelated-secret", `{"contractVersion":"1","stage":"prepare","status":"succeeded","message":"Authorization: Bearer unknown-token"}`, `{"contractVersion":"1","stage":"prepare","status":"succeeded","message":"Authorization: [REDACTED]"}`},
		{"authorization before required fields", "private-token", `{"message":"Authorization: Bearer private-token","contractVersion":"1","stage":"prepare","status":"succeeded"}`, `{"message":"Authorization: [REDACTED]","contractVersion":"1","stage":"prepare","status":"succeeded"}`},
		{"authorization object key", "private-token", `{"Authorization: Bearer private-token":{"keep":[1e400,true,null,{}]},"after":"keep"}`, `{"Authorization: [REDACTED]":{"keep":[1e400,true,null,{}]},"after":"keep"}`},
		{"authorization root string", "private-token", `"Authorization: Bearer private-token"`, `"Authorization: [REDACTED]"`},
		{"authorization escaped newline and quotes", "private-token", `{"message":"Authorization: Bearer \"private-token\"\nkeep","after":[false,42]}`, `{"message":"Authorization: [REDACTED]\nkeep","after":[false,42]}`},
		{"private key result", "private-token", `{"contractVersion":"1","stage":"prepare","status":"succeeded","message":"-----BEGIN PRIVATE KEY-----\nprivate-token\n-----END PRIVATE KEY-----","usage":null}`, `{"contractVersion":"1","stage":"prepare","status":"succeeded","message":"[REDACTED]\n\n","usage":null}`},
		{"unterminated private key", "private-token", `{"nested":["-----BEGIN PRIVATE KEY-----\nprivate-token","keep"],"after":true}`, `{"nested":["[REDACTED]\n","keep"],"after":true}`},
		{"duplicate object keys", "private-token", `{"message":"private-token","message":"Authorization: Bearer unknown-token","after":123}`, `{"message":"[REDACTED]","message":"Authorization: [REDACTED]","after":123}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLogControlPlane(t)
			live, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			r, claim, reporter, services := stageFixture(t, true)
			r.token, r.controlPlaneURL, r.client = live.token, f.server.URL, f.server.Client()
			claim.Execution.ID = logExecution
			var def contract.ProfileDefinition
			if err := json.Unmarshal(claim.Profile.Definition, &def); err != nil {
				t.Fatal(err)
			}
			def.Stages.Prepare.Secrets = []contract.Secret{{Slot: "source", Access: "read"}}
			claim.Profile.Definition, _ = json.Marshal(def)
			if err := secretstore.New(r.dataDir).Set(claim.Profile.HarnessID, "source", "read", []byte(test.key)); err != nil {
				t.Fatal(err)
			}
			reporter.next = func(model.Stage, StageOutcome) StageNext { return StageNext{Next: "stop"} }
			var directory string
			var artifacts []LogArtifact
			services.Executor = stageExecutorFunc(func(ctx context.Context, in StageInvocation) StageOutcome {
				directory = in.Directory
				for _, output := range []struct{ kind, name, path string }{
					{"out_file", "credentials.json", filepath.Join(in.Directory, "out", "credentials.json")},
					{"result_json", "result.json", filepath.Join(in.Directory, "result.json")},
				} {
					if err := os.WriteFile(output.path, []byte(test.raw), 0600); err != nil {
						t.Fatal(err)
					}
					artifact, err := in.CollectArtifact(output.kind, output.name, strings.NewReader(test.raw))
					if err != nil {
						t.Fatal(err)
					}
					artifacts = append(artifacts, artifact)
					if err := r.registerArtifact(ctx, artifact); err != nil {
						t.Fatal(err)
					}
				}
				outcome := successfulStage(in.Stage)
				outcome.Result = json.RawMessage(test.raw)
				return outcome
			})
			if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
				t.Fatal(err)
			}
			if len(reporter.outcomes) != 1 || reporter.outcomes[0].RunnerError != nil {
				t.Fatalf("unexpected stage outcomes: %+v", reporter.outcomes)
			}
			check := func(name string, data []byte) {
				t.Helper()
				assertNoStageKeys(t, data, test.key)
				if !json.Valid(data) || string(data) != test.want {
					t.Errorf("%s: got %s, want %s", name, data, test.want)
				}
				// For valid stage results, exercise the same contract decoder as
				// the Control Plane and explicitly retain the required fields.
				if strings.Contains(test.raw, `"contractVersion"`) {
					var original contract.Result
					if err := json.Unmarshal([]byte(test.raw), &original); err != nil {
						t.Fatalf("invalid original result fixture: %v", err)
					}
					var result contract.Result
					if err := json.Unmarshal(data, &result); err != nil {
						t.Errorf("%s: redacted result violates contract: %v", name, err)
					} else if result.ContractVersion != original.ContractVersion || result.Stage != original.Stage || result.Status != original.Status {
						t.Errorf("%s: result required fields changed: %+v", name, result)
					}
				}
				// Check decoded strings as well as persisted bytes: escaping must
				// never allow a consumer to recover the registered credential.
				decoder := json.NewDecoder(bytes.NewReader(data))
				decoder.UseNumber()
				for {
					token, err := decoder.Token()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if value, ok := token.(string); ok && strings.Contains(value, test.key) {
						t.Errorf("%s: decoded string retains credential", name)
					}
				}
			}
			if len(artifacts) != 2 {
				t.Fatalf("artifact count=%d", len(artifacts))
			}
			for _, artifact := range artifacts {
				data, err := os.ReadFile(artifact.Path)
				if err != nil {
					t.Fatal(err)
				}
				check(artifact.Name, data)
				if !artifact.Redacted {
					t.Errorf("%s: missing redacted metadata", artifact.Name)
				}
				var path string
				var redacted int
				if err := f.db.QueryRow(`SELECT storage_path, redacted FROM artifacts WHERE id=?`, artifact.ID).Scan(&path, &redacted); err != nil {
					t.Fatal(err)
				}
				stored, err := os.ReadFile(filepath.Join(f.dataDir, path))
				if err != nil {
					t.Fatal(err)
				}
				check("stored "+artifact.Name, stored)
				if redacted != 1 {
					t.Errorf("%s: persisted redacted metadata=%d", artifact.Name, redacted)
				}
			}
			for _, name := range []string{filepath.Join("out", "credentials.json"), "result.json"} {
				data, err := os.ReadFile(filepath.Join(directory, name))
				if err != nil {
					t.Fatal(err)
				}
				check(name, data)
			}
			check("reported result", reporter.outcomes[0].Result)
		})
	}
}

func TestStageKeyScopeAndUnsafeOutputs(t *testing.T) {
	for _, kind := range []string{"readonly", "symlink", "hardlink", "directory symlink"} {
		t.Run(kind, func(t *testing.T) {
			r, claim, reporter, services := stageFixture(t, true)
			setStageResume(t, &claim, stageResume{Stage: model.StagePublish, CheckedContentID: stagePtr("checked")})
			var environment map[string]string
			var retained StageInvocation
			outside := filepath.Join(claim.Workspace.Path, "private")
			if err := os.WriteFile(outside, []byte("delivery-private-value"), 0600); err != nil {
				t.Fatal(err)
			}
			services.Executor = stageExecutorFunc(func(_ context.Context, in StageInvocation) StageOutcome {
				retained = in
				spec, err := in.SandboxSpec(sandbox.SandboxSpec{Kind: sandbox.Publish})
				if err != nil {
					t.Fatal(err)
				}
				environment = spec.Secrets
				if _, err := in.SandboxSpec(sandbox.SandboxSpec{Kind: sandbox.Work}); err == nil {
					t.Fatal("publish keys granted to work")
				}
				path := filepath.Join(in.Directory, "out", "value")
				switch kind {
				case "readonly":
					if err := os.WriteFile(path, []byte("delivery-private-value"), 0400); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
				case "hardlink":
					if err := os.Link(outside, path); err != nil {
						t.Fatal(err)
					}
				case "directory symlink":
					if err := os.Remove(filepath.Join(in.Directory, "out")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(in.Workspace, filepath.Join(in.Directory, "out")); err != nil {
						t.Fatal(err)
					}
				}
				return successfulStage(in.Stage)
			})
			if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
				t.Fatal(err)
			}
			if len(environment) != 0 {
				t.Fatal("finished invocation retains sandbox keys")
			}
			if _, err := retained.NewLog(nil); err == nil {
				t.Fatal("finished invocation creates a keyed log")
			}
			if _, err := retained.CollectArtifact("out_file", "late", strings.NewReader("late")); err == nil {
				t.Fatal("finished invocation collects artifacts")
			}
			outcome := reporter.outcomes[0]
			if kind == "readonly" {
				if outcome.RunnerError != nil {
					t.Fatalf("read-only output failed: %+v", outcome)
				}
				data, err := os.ReadFile(filepath.Join(retained.Directory, "out", "value"))
				if err != nil || string(data) != "[REDACTED]" {
					t.Fatalf("read-only output not redacted: %q %v", data, err)
				}
			} else if outcome.RunnerError == nil || *outcome.RunnerError != "config_error" {
				t.Fatal("unsafe output was accepted")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "delivery-private-value" {
				t.Fatal("output redaction changed the workspace through a link")
			}
		})
	}
}
