package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/api"
	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"github.com/shota-urano/evaloom/backend/internal/ownerauth"
	"github.com/shota-urano/evaloom/backend/internal/runner"
	"github.com/shota-urano/evaloom/backend/internal/runnerauth"
	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
	"github.com/shota-urano/evaloom/backend/internal/securitytest"
	"github.com/shota-urano/evaloom/backend/internal/store"
)

// The sole program-stage replacement is the runtime CLI. It executes the
// committed command in the translated mount view rather than manufacturing
// stage reports or result files. No Docker daemon is required.
func TestExecutionRuntimeHelper(t *testing.T) {
	root := os.Getenv("EVALOOM_FAKE_RUNTIME")
	if root == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(91)
		}
	}
	value := func(flag string) string {
		i := slices.Index(args, flag)
		if i < 0 {
			return ""
		}
		return args[i+1]
	}
	// evaloom-6pi: the runtime refuses one control command with exit code 1
	// and a daemon message, as Docker does (sandbox_failure_test.go).
	// work-wait fails only the inspection of a work container (CODEX_HOME).
	failing := os.Getenv("EVALOOM_FAKE_FAIL")
	workContainer := false
	if failing == "work-wait" && args[0]+" "+args[1] == "container inspect" {
		raw, _ := os.ReadFile(filepath.Join(root, args[len(args)-1]+".json"))
		workContainer = bytes.Contains(raw, []byte("CODEX_HOME="))
	}
	// relay-create / relay-volume fail only the stage channel's relay sidecar
	// or its volume (spec 09 3.6 item 6).
	relayFailure := failing == "relay-create" && args[0]+" "+args[1] == "container create" && strings.HasSuffix(value("--name"), "-relay") ||
		failing == "relay-volume" && args[0]+" "+args[1] == "volume create" && strings.HasSuffix(args[len(args)-1], "-channel")
	if workContainer || relayFailure || failing != "" &&
		map[string]string{"image": "image inspect", "create": "container create", "wait": "container inspect"}[failing] == args[0]+" "+args[1] {
		token, _ := os.ReadFile(os.Getenv("EVALOOM_FAKE_TOKEN_FILE"))
		fmt.Fprintf(os.Stderr, "%s\nrunner token %s\nAuthorization: Bearer fixture-bearer-value\n%s\n",
			fakeDaemonMessage, token, strings.Repeat("x", 3000))
		os.Exit(1)
	}
	switch args[0] + " " + args[1] {
	case "image inspect":
		fmt.Print(`{"Os":"linux","Config":{}}`)
	case "container create":
		// Spec 14 3.2 negative controls weaken one stage's command here, at
		// the runtime CLI boundary, as a broken Sandbox configuration would.
		if raw := os.Getenv("EVALOOM_FAKE_LOOSEN"); raw != "" {
			var l securitytest.Loosening
			must(json.Unmarshal([]byte(raw), &l))
			args = l.Loosen(args, runner.DefaultProposalPaths)
		}
		data, _ := json.Marshal(args)
		must(os.WriteFile(filepath.Join(root, value("--name")+".json"), data, 0600))
		// Name-only --env options import values from this create client's
		// environment (stage secrets), as the Docker CLI does. The fake daemon
		// keeps them in the test-owned runtime directory until start.
		imported := map[string]string{}
		for i, arg := range args {
			if arg == "--env" && i+1 < len(args) && !strings.Contains(args[i+1], "=") {
				if v, ok := os.LookupEnv(args[i+1]); ok {
					imported[args[i+1]] = v
				}
			}
		}
		if len(imported) != 0 {
			data, _ := json.Marshal(imported)
			must(os.WriteFile(filepath.Join(root, value("--name")+".env"), data, 0600))
		}
	case "container start":
		name := args[len(args)-1]
		raw, err := os.ReadFile(filepath.Join(root, name+".json"))
		must(err)
		var create []string
		must(json.Unmarshal(raw, &create))
		if strings.HasSuffix(name, "-relay") {
			// The stage channel relay sidecar (spec 09 3.6): the production
			// relay code listening in the stage's channel volume, over this
			// attach client's stdio, as the sidecar's own stdio would be.
			listener, err := net.Listen("unix", filepath.Join(fakeChannelVolume(root, create), sandboxproxy.ChannelName))
			must(err)
			_ = sandboxproxy.ServeChannel(listener, os.Stdin, os.Stdout)
			os.Exit(0)
		}
		mounts := map[string]string{}
		workdir := ""
		for i := 2; i < len(create); i++ {
			switch create[i] {
			case "--workdir":
				workdir = create[i+1]
			case "--mount":
				var source, target string
				volume := strings.HasPrefix(create[i+1], "type=volume,")
				for _, part := range strings.Split(create[i+1], ",") {
					if s, ok := strings.CutPrefix(part, "source="); ok {
						source = s
						if volume {
							source = filepath.Join(root, "volumes", s)
							// A channel volume is a short directory, so its
							// socket path fits sockaddr_un on the host.
							if real, err := filepath.EvalSymlinks(source); err == nil {
								source = real
							}
						}
					}
					if s, ok := strings.CutPrefix(part, "target="); ok {
						target = s
					}
				}
				mounts[target] = source
			}
		}
		var targets []string
		for target := range mounts {
			targets = append(targets, target)
		}
		sort.Slice(targets, func(i, j int) bool { return len(targets[i]) > len(targets[j]) })
		translate := func(path string) string {
			for _, target := range targets {
				if path == target || strings.HasPrefix(path, target+"/") {
					return mounts[target] + strings.TrimPrefix(path, target)
				}
			}
			return path
		}
		var env []string
		index := slices.Index(create, "-i") + 1
		if index == 0 {
			// Keyed stages replace env -i with "-u NAME" pairs and keep only
			// the imported --env variables.
			index = slices.Index(create, "-u")
			for index >= 0 && index+1 < len(create) && create[index] == "-u" {
				index += 2
			}
			if raw, err := os.ReadFile(filepath.Join(root, name+".env")); err == nil {
				var imported map[string]string
				must(json.Unmarshal(raw, &imported))
				for key, v := range imported {
					env = append(env, key+"="+v)
				}
			}
		}
		for index < len(create) && strings.Contains(create[index], "=") {
			key, v, _ := strings.Cut(create[index], "=")
			env = append(env, key+"="+translate(v))
			index++
		}
		// Without a mount namespace, paths inside request.json stay guest
		// paths. Programs that follow them (the testdata special harness) get
		// the mount table; a real container never sets this variable.
		table, _ := json.Marshal(mounts)
		env = append(env, "EVALOOM_FAKE_MOUNTS="+string(table))
		command := append([]string(nil), create[index:]...)
		if !filepath.IsAbs(command[0]) {
			command[0] = filepath.Join(translate(workdir), command[0])
		}
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Dir = translate(workdir)
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		exit := 0
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				exit = ee.ExitCode()
			} else {
				fmt.Fprintln(os.Stderr, err)
				exit = 127
			}
		}
		must(os.WriteFile(filepath.Join(root, name+".exit"), []byte(fmt.Sprint(exit)), 0600))
	case "volume create":
		// Work sessions seed a private configuration volume (spec 09). The
		// fake daemon keeps it as a directory plus the archive it received.
		name := args[len(args)-1]
		must(os.MkdirAll(filepath.Join(root, "volumes"), 0700))
		if strings.HasSuffix(name, "-channel") {
			// Stage channel volumes hold a Unix socket: keep the real
			// directory short and link it from the fake daemon's volumes.
			short, err := os.MkdirTemp("", "evc-")
			must(err)
			must(os.Symlink(short, filepath.Join(root, "volumes", name)))
		} else {
			must(os.MkdirAll(filepath.Join(root, "volumes", name), 0700))
		}
	case "container exec":
		// Only the channel relay's readiness check: wait until it listens.
		create, err := os.ReadFile(filepath.Join(root, args[2]+".json"))
		must(err)
		var createArgs []string
		must(json.Unmarshal(create, &createArgs))
		socket := filepath.Join(fakeChannelVolume(root, createArgs), sandboxproxy.ChannelName)
		deadline := time.Now().Add(5 * time.Second)
		for {
			if conn, err := net.Dial("unix", socket); err == nil {
				conn.Close()
				break
			}
			if time.Now().After(deadline) {
				must(fmt.Errorf("channel relay not listening"))
			}
			time.Sleep(5 * time.Millisecond)
		}
	case "volume rm":
		if real, err := filepath.EvalSymlinks(filepath.Join(root, "volumes", args[len(args)-1])); err == nil {
			must(os.RemoveAll(real))
		}
		must(os.RemoveAll(filepath.Join(root, "volumes", args[len(args)-1])))
		_ = os.Remove(filepath.Join(root, "volumes", args[len(args)-1]+".tar"))
	case "volume ls":
		filter := strings.TrimSuffix(strings.TrimPrefix(value("--filter"), "name=^"), "$")
		if _, err := os.Stat(filepath.Join(root, "volumes", filter)); err == nil {
			fmt.Println(filter)
		}
	case "container cp":
		// cp - name:dir/. stores the archive in the container's volume;
		// cp name:dir/. - returns it, as the daemon reads the mounted volume.
		source, target := args[2], args[3]
		name, _, _ := strings.Cut(target, ":")
		if source != "-" {
			name, _, _ = strings.Cut(source, ":")
		}
		raw, err := os.ReadFile(filepath.Join(root, name+".json"))
		must(err)
		var create []string
		must(json.Unmarshal(raw, &create))
		volume := ""
		for i, arg := range create {
			if i > 0 && create[i-1] == "--mount" && strings.HasPrefix(arg, "type=volume,") {
				for _, part := range strings.Split(arg, ",") {
					if v, ok := strings.CutPrefix(part, "source="); ok {
						volume = v
					}
				}
			}
		}
		if volume == "" {
			must(fmt.Errorf("cp without configuration volume"))
		}
		archive := filepath.Join(root, "volumes", volume+".tar")
		if source == "-" {
			data, err := io.ReadAll(os.Stdin)
			must(err)
			must(os.WriteFile(archive, data, 0600))
		} else {
			data, err := os.ReadFile(archive)
			must(err)
			_, _ = os.Stdout.Write(data)
		}
	case "container inspect":
		raw, err := os.ReadFile(filepath.Join(root, args[len(args)-1]+".exit"))
		must(err)
		fmt.Printf(`{"Status":"exited","Running":false,"ExitCode":%s}`, raw)
	case "container rm":
	default:
		fmt.Fprintln(os.Stderr, "unsupported fake runtime call", args)
		os.Exit(92)
	}
	os.Exit(0)
}

// fakeChannelVolume is the host directory of the relay sidecar's channel volume.
func fakeChannelVolume(root string, create []string) string {
	for i, arg := range create {
		if i > 0 && create[i-1] == "--mount" && strings.HasPrefix(arg, "type=volume,") && strings.Contains(arg, ",target="+sandboxproxy.ChannelDirectory+",") {
			for _, part := range strings.Split(arg, ",") {
				if v, ok := strings.CutPrefix(part, "source="); ok {
					if real, err := filepath.EvalSymlinks(filepath.Join(root, "volumes", v)); err == nil {
						return real
					}
				}
			}
		}
	}
	return filepath.Join(root, "missing-channel-volume")
}

// This binary is the test special harness's prepare/check/publish program.
// Its wrapper is committed and tagged with the Profile. Results are written
// by the program itself and collected through the production Runner path.
func TestExecutionProfileProgram(t *testing.T) {
	if os.Getenv("EVALOOM_RESULT") == "" {
		return
	}
	fail := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(93)
		}
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	stage, scenario := args[0], args[1]
	request, err := os.ReadFile(os.Getenv("EVALOOM_REQUEST"))
	fail(err)
	var req contract.Request
	fail(json.Unmarshal(request, &req))
	if string(req.Stage) != stage {
		fail(fmt.Errorf("wrong request stage %s", req.Stage))
	}
	directory := filepath.Dir(os.Getenv("EVALOOM_REQUEST"))
	result := map[string]any{"contractVersion": "1", "stage": stage, "status": "succeeded", "message": "fixture " + stage}
	if stage == "check" {
		result["contentId"] = "fixture-content"
		fail(os.WriteFile(filepath.Join(directory, "out", "checked.txt"), []byte("checked pinned content"), 0600))
		if scenario == "approval" {
			result["status"] = "needs_approval"
			result["approvalRequests"] = []any{map[string]any{"operation": "deploy", "contentId": "fixture-content", "target": map[string]any{"label": "fixture"}, "summary": "deploy fixture"}}
		}
	}
	if stage == "publish" {
		content, err := os.ReadFile(filepath.Join(directory, "inputs", "check", "checked.txt"))
		fail(err)
		if string(content) != "checked pinned content" {
			fail(fmt.Errorf("lost check output"))
		}
		if req.CheckedContentID == nil || *req.CheckedContentID != "fixture-content" {
			fail(fmt.Errorf("lost checkedContentId"))
		}
		if scenario == "approval" {
			if len(req.Approvals) != 1 || req.Approvals[0].ID == nil {
				fail(fmt.Errorf("missing approval"))
			}
			conn, err := net.Dial("unix", os.Getenv("EVALOOM_CHANNEL"))
			fail(err)
			defer conn.Close()
			reader := bufio.NewReader(conn)
			var replies []map[string]any
			for range 3 {
				fail(json.NewEncoder(conn).Encode(map[string]any{"op": "authorize", "operation": "deploy", "approvalId": req.Approvals[0].ID, "contentId": "fixture-content"}))
				line, err := reader.ReadBytes('\n')
				fail(err)
				var reply map[string]any
				fail(json.Unmarshal(line, &reply))
				replies = append(replies, reply)
			}
			data, _ := json.Marshal(replies)
			fail(os.WriteFile(filepath.Join(directory, "out", "authorizations.json"), data, 0600))
		}
	}
	data, _ := json.Marshal(result)
	fail(os.WriteFile(os.Getenv("EVALOOM_RESULT"), data, 0600))
	fmt.Println("fixture executed " + stage)
	os.Exit(0)
}

type safeOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (o *safeOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.Write(data)
}
func (o *safeOutput) String() string { o.mu.Lock(); defer o.mu.Unlock(); return o.buffer.String() }

type executionFixture struct {
	t            *testing.T
	db           *sql.DB
	server       *httptest.Server
	cookie       *http.Cookie
	csrf         string
	config       runner.Config
	configPath   string
	adapters     *adapter.Registry
	id           string
	output       *safeOutput
	done         chan error
	cancel       context.CancelFunc
	mu           sync.Mutex
	workRequests []adapter.HarnessRequest
	accesses     []string
	wait         time.Duration // waitStatus deadline; 0 means 25s
	claims       []runner.Claim
	// workAdapter, when set, replaces the deterministic dummy work Adapter
	// with one built from Runner's real work wiring (the probe Adapter).
	workAdapter func(runner.WorkAdapterConfig) adapter.Adapter
}

// fixtureHarness is the special harness repository a fixture registers.
// populate writes its files before the commit tagged profile/v1.0.0;
// instructions is the pinned work instruction text the work Adapter must see.
type fixtureHarness struct {
	populate func(t *testing.T, repo, binary string)
	// populateFixture, when set, replaces populate for content that depends
	// on the running fixture (Control API address, data paths).
	populateFixture func(t *testing.T, f *executionFixture, repo string)
	instructions    string
	taskBody        string
	// workOut, when set, holds extra files the deterministic work Adapter
	// writes into its out directory next to summary.md (spec 14 3.5).
	workOut map[string]string
}

// inlineHarness wraps this test binary as the prepare/check/publish program.
func inlineHarness(scenario string) fixtureHarness {
	return fixtureHarness{instructions: "fixture work instructions", taskBody: "fixture input", populate: func(t *testing.T, repo, binary string) {
		if err := os.MkdirAll(filepath.Join(repo, "bin"), 0700); err != nil {
			t.Fatal(err)
		}
		for _, stage := range []string{"prepare", "check", "publish"} {
			script := "#!/bin/sh\nexec " + shellQuote(binary) + " -test.run=^TestExecutionProfileProgram$ -- " + stage + " " + scenario + "\n"
			if err := os.WriteFile(filepath.Join(repo, "bin", stage), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(repo, "evaloom"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(repo, "prompts"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "prompts", "work.md"), []byte("fixture work instructions"), 0600); err != nil {
			t.Fatal(err)
		}
		profile := `{"contractVersion":"1","profileVersion":"1.0.0","name":"fixture","description":"integration fixture","acceptanceCriteria":"required","stages":{"prepare":{"command":["bin/prepare"],"timeoutSec":20},"work":{"microHarness":"codex","model":"fixture","instructions":["prompts/work.md"],"resume":"optional","timeoutSec":20},"check":{"command":["bin/check"],"timeoutSec":20},"publish":{"command":["bin/publish"],"timeoutSec":20}},"approvals":[{"operation":"deploy","description":"deploy fixture","waivable":false}]}`
		if err := os.WriteFile(filepath.Join(repo, "evaloom", "profile.yaml"), []byte(profile), 0600); err != nil {
			t.Fatal(err)
		}
	}}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func newExecutionFixture(t *testing.T, scenario string) *executionFixture {
	t.Helper()
	return newHarnessFixture(t, scenario, inlineHarness(scenario))
}

func newHarnessFixture(t *testing.T, scenario string, harness fixtureHarness) *executionFixture {
	t.Helper()
	f := &executionFixture{t: t, output: &safeOutput{}, adapters: &adapter.Registry{}}
	// Unix socket paths must fit sockaddr_un even after resolving /var to
	// /private/var on macOS. Keep this fixture's Runner data path short.
	root, err := os.MkdirTemp("/private/tmp", "e13-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	f.db, err = store.Open(t.Context(), filepath.Join(root, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	files, err := store.NewArtifactStore(f.db, filepath.Join(root, "control-data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	f.server = httptest.NewUnstartedServer(nil)
	auth, err := api.NewOwnerAuth(f.db, log, f.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Capture actual claim replies without changing any request, status or body.
	handler := api.NewHandlerWithArtifacts(f.db, log, auth, nil, api.ArtifactConfig{Store: files, MaxBytes: 1 << 20}, f.adapters)
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Control Plane access log in front of the real handler (spec 14 3.2
		// judges "unreachable" rows by it too, not only by probe reports).
		f.mu.Lock()
		f.accesses = append(f.accesses, r.RequestURI)
		f.mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/claim") {
			handler.ServeHTTP(w, r)
			return
		}
		capture := httptest.NewRecorder()
		handler.ServeHTTP(capture, r)
		if capture.Code == 200 {
			var response struct {
				Job runner.Claim `json:"job"`
			}
			if json.Unmarshal(capture.Body.Bytes(), &response) == nil && response.Job.Kind == "execution" {
				f.mu.Lock()
				f.claims = append(f.claims, response.Job)
				f.mu.Unlock()
			}
		}
		for key, values := range capture.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(capture.Code)
		_, _ = w.Write(capture.Body.Bytes())
	})
	f.server.Start()
	t.Cleanup(f.server.Close)
	configPath := writeConfig(t, f.server.URL)
	f.config, err = runner.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	f.config.DataDir = filepath.Join(root, "data")
	f.config.WorkspaceRoot = filepath.Join(root, "workspaces")
	f.config.SecretsDir = filepath.Join(root, "data", "secrets")
	if err := os.MkdirAll(f.config.WorkspaceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	token, err := runnerauth.Register(t.Context(), f.db, f.config.RunnerID, "Runner", f.config.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config.TokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.config.Container.RuntimePath = filepath.Join(root, "runtime-cli")
	if err := os.WriteFile(f.config.Container.RuntimePath, []byte("#!/bin/sh\nexec "+shellQuote(binary)+" -test.run=^TestExecutionRuntimeHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.config.Container.RuntimeEnv = map[string]string{"EVALOOM_FAKE_RUNTIME": runtimeDir}
	// The stage channel relay sidecar's trusted binary (spec 09 3.6). The fake
	// runtime runs the production relay code in its place.
	f.config.Container.ProxyBinary = filepath.Join(root, "proxy")
	if err := os.WriteFile(f.config.Container.ProxyBinary, []byte("fixture proxy"), 0700); err != nil {
		t.Fatal(err)
	}
	f.config.MicroHarnessAuthFiles = map[string]string{"codex": filepath.Join(f.config.DataDir, "auth.json")}
	if err := os.WriteFile(f.config.MicroHarnessAuthFiles["codex"], []byte(`{"token":"fixture-auth"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The production archive/input trees are intentionally read-only. Restore
		// permissions only on this test's own data before TempDir removes it.
		_ = filepath.WalkDir(f.config.DataDir, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
	})
	f.configPath = configPath
	data, _ := json.Marshal(f.config)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.adapters.Register("codex", dummy.NewWithStart(func(req adapter.HarnessRequest, env adapter.SandboxEnv) error {
		f.mu.Lock()
		f.workRequests = append(f.workRequests, req)
		attempt := len(f.workRequests)
		f.mu.Unlock()
		pinned, err := os.ReadFile(filepath.Join(env.ConfigDir, "prompts", "work.md"))
		if err != nil || string(pinned) != harness.instructions {
			return fmt.Errorf("wrong pinned work instructions: %s %v", pinned, err)
		}
		if _, err := os.Stat(filepath.Join(env.ConfigDir, "bin", "check")); !os.IsNotExist(err) {
			return fmt.Errorf("work received check program")
		}
		out := filepath.Join(f.config.DataDir, "executions", req.ExecutionID, fmt.Sprintf("work-%d", attempt), "out")
		if scenario == "question" && attempt == 1 {
			return os.WriteFile(filepath.Join(out, "question.json"), []byte(`{"question":"Keep or remove?","options":["keep","remove"]}`), 0600)
		}
		for name, content := range harness.workOut {
			if err := os.WriteFile(filepath.Join(out, name), []byte(content), 0600); err != nil {
				return err
			}
		}
		return os.WriteFile(filepath.Join(out, "summary.md"), []byte("dummy work completed fixture-auth"), 0600)
	})); err != nil {
		t.Fatal(err)
	}
	hash, err := ownerauth.HashPassword([]byte("fixture owner password"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOwnerCredentials(t.Context(), f.db, "owner", hash); err != nil {
		t.Fatal(err)
	}
	response := f.request("POST", "/auth/login", `{"id":"owner","password":"fixture owner password"}`, 200)
	if len(response.Cookies()) != 1 {
		t.Fatal("no owner session")
	}
	f.cookie = response.Cookies()[0]
	var login struct {
		CSRF string `json:"csrfToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	f.csrf = login.CSRF
	repo := filepath.Join(root, "harness")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if harness.populateFixture != nil {
		harness.populateFixture(t, f, repo)
	} else {
		harness.populate(t, repo, binary)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, output)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-qm", "fixture")
	git("tag", "profile/v1.0.0")
	harnessInput, _ := json.Marshal(map[string]any{"id": "fixture", "name": "Fixture", "repoPath": repo, "repoUrl": "https://example.invalid/fixture.git"})
	f.owner("POST", "/api/harnesses", string(harnessInput), 201)
	profileData := f.owner("POST", "/api/harness-profiles", `{"harnessId":"fixture","version":"1.0.0"}`, 201)
	var registered struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(profileData, &registered); err != nil {
		t.Fatal(err)
	}
	projectData, _ := json.Marshal(map[string]any{"id": "fixture", "name": "Fixture", "runnerId": f.config.RunnerID, "harnessId": "fixture", "profileId": registered.ID})
	f.owner("POST", "/api/projects", string(projectData), 201)
	taskInput, _ := json.Marshal(map[string]any{"projectId": "fixture", "title": "Fixture task", "body": harness.taskBody, "acceptanceCriteria": "fixture passes"})
	f.owner("POST", "/api/tasks", string(taskInput), 201)
	if err := f.db.QueryRow(`SELECT id FROM executions`).Scan(&f.id); err != nil {
		t.Fatal(err)
	}
	// Only test-repository files are committed. The product worktree is untouched.
	// Mutating an uncommitted program must not affect the archived tagged command.
	if err := os.WriteFile(filepath.Join(repo, "bin", "check"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if scenario == "retag" {
		git("add", ".")
		git("commit", "-qm", "replacement")
		git("tag", "-f", "profile/v1.0.0")
	}
	return f
}

func (f *executionFixture) request(method, path, body string, want int) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Origin", f.server.URL)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "fixture-task")
	if f.cookie != nil {
		req.AddCookie(f.cookie)
		req.Header.Set("X-CSRF-Token", f.csrf)
	}
	response, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	if response.StatusCode != want {
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		f.t.Fatalf("%s %s: %d %s; want %d", method, path, response.StatusCode, data, want)
	}
	return response
}
func (f *executionFixture) owner(method, path, body string, want int) []byte {
	f.t.Helper()
	r := f.request(method, path, body, want)
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}
func (f *executionFixture) start() {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.done = make(chan error, 1)
	go func() {
		f.done <- runWithAdapters(ctx, []string{"-config", f.configPath}, strings.NewReader(""), f.output, func(w runner.WorkAdapterConfig) *adapter.Registry {
			adapters := &adapter.Registry{}
			if f.workAdapter != nil {
				// The probe Adapter manages its own working configuration.
				_ = adapters.Register("codex", f.workAdapter(w))
				return adapters
			}
			base, _ := f.adapters.Lookup("codex")
			_ = adapters.Register("codex", &authenticatedFixtureAdapter{Adapter: base, wiring: w})
			return adapters
		})
	}()
	f.t.Cleanup(func() {
		cancel()
		select {
		case err := <-f.done:
			if err != nil {
				f.t.Error(err)
			}
		case <-time.After(25 * time.Second):
			f.t.Error("runner did not stop")
		}
	})
}

// The command test replaces only the micro-harness. Its deterministic Adapter
// still installs real credentials under a real lease, as production Adapters
// do; Runner no longer reads a credential store outside that lease.
type authenticatedFixtureAdapter struct {
	adapter.Adapter
	wiring runner.WorkAdapterConfig
}

func (a *authenticatedFixtureAdapter) Start(ctx context.Context, req adapter.HarnessRequest, env adapter.SandboxEnv) (adapter.SessionID, error) {
	original, err := a.wiring.ResolveConfig(env)
	if err != nil {
		return "", err
	}
	working, err := original.WorkingCopy(ctx)
	if err != nil {
		return "", err
	}
	defer working.Close()
	store, err := a.wiring.ResolveAuthentication(req)
	if err != nil {
		return "", err
	}
	lease, err := harnessenv.AcquireAuthentication(ctx, store)
	if err != nil {
		return "", err
	}
	defer lease.Close()
	if err := lease.Install(working); err != nil {
		return "", err
	}
	env.ConfigDir = working.ConfigDir()
	env.Env = working.Env()
	return a.Adapter.Start(ctx, req, env)
}
func (f *executionFixture) waitStatus(want string) {
	f.t.Helper()
	wait := 25 * time.Second
	if f.wait != 0 {
		wait = f.wait
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		var status string
		var errorClass sql.NullString
		if err := f.db.QueryRow(`SELECT status,error_class FROM executions WHERE id=?`, f.id).Scan(&status, &errorClass); err != nil {
			f.t.Fatal(err)
		}
		if status == want {
			return
		}
		if status == "failed" || status == "timed_out" || status == "cancelled" {
			rows, _ := f.db.Query(`SELECT stage,status,coalesce(error_class,'') FROM stage_runs WHERE execution_id=? ORDER BY started_at`, f.id)
			if rows != nil {
				for rows.Next() {
					var a, b, c string
					_ = rows.Scan(&a, &b, &c)
					f.t.Logf("stage %s %s %s", a, b, c)
				}
				rows.Close()
			}
			_ = filepath.WalkDir(f.config.DataDir, func(path string, entry os.DirEntry, err error) error {
				if err == nil && !entry.IsDir() && (strings.HasSuffix(path, ".log") || strings.HasSuffix(path, "result.json")) {
					data, _ := os.ReadFile(path)
					f.t.Logf("%s: %s", path, data)
				}
				return nil
			})
			f.t.Fatalf("Execution %s (%s), wanted %s; output %s", status, errorClass.String, want, f.output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("Execution did not reach %s; output %s", want, f.output.String())
}
func (f *executionFixture) stages(want []string) {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT stage||'-'||attempt FROM stage_runs WHERE execution_id=? ORDER BY started_at,id`, f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var stage string
		if err := rows.Scan(&stage); err != nil {
			f.t.Fatal(err)
		}
		got = append(got, stage)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		f.t.Fatalf("stages %v; want %v", got, want)
	}
}
func (f *executionFixture) decision(kind string) string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(`SELECT id FROM owner_decisions WHERE execution_id=? AND kind=? AND status='open'`, f.id, kind).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f *executionFixture) claimCount(want int) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.claims) != want {
		f.t.Fatalf("claims=%d; want %d", len(f.claims), want)
	}
}

func TestCommandExecutionIntegration(t *testing.T) {
	for _, scenario := range []string{"success", "approval", "question", "retag"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionFixture(t, scenario)
			f.start()
			switch scenario {
			case "retag":
				f.waitStatus("failed")
				f.stages([]string{"prepare-1"})
				f.claimCount(1)
				var code string
				if err := f.db.QueryRow(`SELECT error_class FROM executions WHERE id=?`, f.id).Scan(&code); err != nil {
					t.Fatal(err)
				}
				if code != "config_error" {
					t.Fatalf("retag error class %s", code)
				}
				calls, err := filepath.Glob(filepath.Join(f.config.Container.RuntimeEnv["EVALOOM_FAKE_RUNTIME"], "*.json"))
				if err != nil {
					t.Fatal(err)
				}
				if len(calls) != 0 {
					t.Fatalf("retagged Profile started runtime: %v", calls)
				}
				return
			case "success":
				f.waitStatus("succeeded")
				f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})
				f.claimCount(1)
			case "approval":
				f.waitStatus("waiting_approval")
				f.stages([]string{"prepare-1", "work-1", "check-1"})
				f.claimCount(1)
				decision := f.decision("approval")
				// An empty queue remains empty before the owner answers.
				req, _ := http.NewRequest("POST", f.server.URL+"/api/runners/"+f.config.RunnerID+"/claim", strings.NewReader(fmt.Sprintf(`{"workspaceRoot":%q}`, f.config.WorkspaceRoot)))
				token, _ := os.ReadFile(f.config.TokenFile)
				req.Header.Set("Authorization", "Bearer "+string(token))
				req.Header.Set("Content-Type", "application/json")
				response, err := f.server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 204 && (response.StatusCode != 200 || !bytes.Contains(body, []byte(`"job":null`))) {
					t.Fatalf("waiting claim: %d", response.StatusCode)
				}
				f.owner("POST", "/api/decisions/"+decision+"/answer", `{"action":"approve","contentId":"fixture-content"}`, 200)
				f.waitStatus("succeeded")
				f.stages([]string{"prepare-1", "work-1", "check-1", "publish-1"})
				f.claimCount(2)
				f.mu.Lock()
				resumed := f.claims[1]
				f.mu.Unlock()
				var resume struct {
					Stage     string   `json:"stage"`
					Completed []string `json:"completedStages"`
				}
				if err := json.Unmarshal(resumed.Resume, &resume); err != nil {
					t.Fatal(err)
				}
				if resumed.Execution.ID != f.id || resumed.Execution.ResumeStage == nil || *resumed.Execution.ResumeStage != "publish" || resumed.Workspace.Created || resume.Stage != "publish" || !reflect.DeepEqual(resume.Completed, []string{"prepare", "work", "check"}) {
					t.Fatalf("unexpected publish resume claim: %+v resume=%+v", resumed, resume)
				}
				var used int
				var status string
				if err := f.db.QueryRow(`SELECT use_count,status FROM approvals WHERE execution_id=?`, f.id).Scan(&used, &status); err != nil {
					t.Fatal(err)
				}
				if used != 1 || status != "consumed" {
					t.Fatalf("approval %s use_count=%d", status, used)
				}
				path := filepath.Join(f.config.DataDir, "executions", f.id, "publish-1", "out", "authorizations.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var replies []struct {
					Allowed bool   `json:"allowed"`
					Reason  string `json:"reason"`
				}
				if err := json.Unmarshal(data, &replies); err != nil {
					t.Fatal(err)
				}
				if len(replies) != 3 || !replies[0].Allowed || replies[1].Allowed || replies[1].Reason != "already_used" || replies[2].Allowed || replies[2].Reason != "already_used" {
					t.Fatalf("authorizations %s", data)
				}
			case "question":
				f.waitStatus("blocked")
				f.stages([]string{"prepare-1", "work-1"})
				f.claimCount(1)
				decision := f.decision("question")
				f.owner("POST", "/api/decisions/"+decision+"/answer", `{"action":"answer","text":"keep it","option":"keep"}`, 200)
				f.waitStatus("succeeded")
				f.stages([]string{"prepare-1", "work-1", "work-2", "check-1", "publish-1"})
				f.claimCount(2)
				data, err := os.ReadFile(filepath.Join(f.config.DataDir, "executions", f.id, "work-2", "request.json"))
				if err != nil {
					t.Fatal(err)
				}
				var request contract.Request
				if err := json.Unmarshal(data, &request); err != nil {
					t.Fatal(err)
				}
				if request.Answer == nil || request.Answer.DecisionID != decision || request.Answer.Text != "keep it" || request.Answer.Option == nil || *request.Answer.Option != "keep" {
					t.Fatalf("resume answer %s", data)
				}
				f.mu.Lock()
				workRequests := append([]adapter.HarnessRequest(nil), f.workRequests...)
				claims := append([]runner.Claim(nil), f.claims...)
				f.mu.Unlock()
				if len(workRequests) != 2 || !strings.Contains(workRequests[1].Context.Instructions, `"text":"keep it"`) {
					t.Fatalf("answer not delivered to Adapter: %+v", workRequests)
				}
				if claims[1].Execution.ID != claims[0].Execution.ID || claims[1].Workspace.ID != claims[0].Workspace.ID || claims[1].Execution.ResumeStage == nil || *claims[1].Execution.ResumeStage != "work" {
					t.Fatalf("resume claim %+v", claims[1])
				}
			}
			// Artifacts and stage outcomes were persisted by the real API, and all
			// programs returned their own results rather than the test reporting them.
			var contextFiles, contextTasks, contextEvents int
			for query, target := range map[string]*int{`SELECT count(*) FROM context_revisions WHERE execution_id=? AND layer='harness' AND kind='instructions'`: &contextFiles, `SELECT count(*) FROM context_revisions WHERE execution_id=? AND layer='task'`: &contextTasks, `SELECT count(*) FROM execution_events WHERE execution_id=? AND type='context_pinned'`: &contextEvents} {
				if err := f.db.QueryRow(query, f.id).Scan(target); err != nil {
					t.Fatal(err)
				}
			}
			if contextFiles != 1 || contextTasks != 1 || contextEvents != 1 {
				t.Fatalf("context pins duplicated or missing: %d/%d/%d", contextFiles, contextTasks, contextEvents)
			}
			var stages, results, requests, usage int
			for query, target := range map[string]*int{`SELECT count(*) FROM stage_runs WHERE execution_id=?`: &stages, `SELECT count(*) FROM artifacts WHERE execution_id=? AND kind='result_json'`: &results, `SELECT count(*) FROM artifacts WHERE execution_id=? AND kind='request_json'`: &requests, `SELECT count(*) FROM usage_records WHERE execution_id=?`: &usage} {
				if err := f.db.QueryRow(query, f.id).Scan(target); err != nil {
					t.Fatal(err)
				}
			}
			var taskStatus string
			if err := f.db.QueryRow(`SELECT t.status FROM tasks t JOIN executions e ON e.task_id=t.id WHERE e.id=?`, f.id).Scan(&taskStatus); err != nil {
				t.Fatal(err)
			}
			if taskStatus != "completed" {
				t.Fatalf("task status %s", taskStatus)
			}
			f.mu.Lock()
			workCalls := len(f.workRequests)
			f.mu.Unlock()
			wantWork := 1
			if scenario == "question" {
				wantWork = 2
			}
			if workCalls != wantWork {
				t.Fatalf("work calls %d; want %d", workCalls, wantWork)
			}
			all, err := filepath.Glob(filepath.Join(f.config.Container.RuntimeEnv["EVALOOM_FAKE_RUNTIME"], "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			// Each program stage also has its channel relay sidecar (spec 09 3.6).
			var calls []string
			relays := 0
			for _, call := range all {
				if strings.HasSuffix(call, "-relay.json") {
					relays++
				} else {
					calls = append(calls, call)
				}
			}
			if len(calls) != 3 || relays != 3 {
				t.Fatalf("program launches %d; want exactly prepare/check/publish", len(calls))
			}
			for _, call := range calls {
				data, err := os.ReadFile(call)
				if err != nil {
					t.Fatal(err)
				}
				var args []string
				if err := json.Unmarshal(data, &args); err != nil {
					t.Fatal(err)
				}
				mounted := false
				for _, arg := range args {
					if strings.HasSuffix(arg, ",target=/evaloom/request.json,readonly") {
						mounted = true
					}
				}
				if !mounted {
					t.Fatalf("request was not mounted read-only: %v", args)
				}
			}
			summary, err := os.ReadFile(filepath.Join(f.config.DataDir, "executions", f.id, fmt.Sprintf("work-%d", wantWork), "out", "summary.md"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(summary, []byte("fixture-auth")) || !bytes.Contains(summary, []byte("[REDACTED]")) {
				t.Fatalf("work authentication was not excluded: %s", summary)
			}
			var storedSummary string
			if err := f.db.QueryRow(`SELECT storage_path FROM artifacts WHERE execution_id=? AND kind='work_summary' ORDER BY id DESC LIMIT 1`, f.id).Scan(&storedSummary); err != nil {
				t.Fatal(err)
			}
			artifactSummary, err := os.ReadFile(filepath.Join(filepath.Dir(f.config.DataDir), "control-data", storedSummary))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(artifactSummary, []byte("fixture-auth")) || !bytes.Contains(artifactSummary, []byte("[REDACTED]")) {
				t.Fatalf("work artifact leaked authentication: %s", artifactSummary)
			}
			if results != 3 || requests != stages || usage != stages {
				t.Fatalf("stage/artifact/usage counts %d/%d/%d/%d", stages, results, requests, usage)
			}
		})
	}
}
