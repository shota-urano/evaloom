package runner

import (
	"archive/tar"
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

	"github.com/oklog/ulid/v2"
	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/claudecode"
	"github.com/shota-urano/evaloom/backend/internal/adapter/codex"
	"github.com/shota-urano/evaloom/backend/internal/harnessenv"
	"golang.org/x/sys/unix"
)

// This external runtime fixture supplies CLI output and mounted files. The
// production Runner, Adapter, Container supervisor, private-volume verification,
// auth recovery, artifact transport and Control Plane all run unchanged.
func TestProposalAdapterRuntime(t *testing.T) {
	root := os.Getenv("PROPOSAL_RUNTIME_ROOT")
	if root == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	write := func(name string, data []byte) {
		if os.WriteFile(filepath.Join(root, name), data, 0600) != nil {
			os.Exit(91)
		}
	}
	if args[0] == "image" {
		fmt.Print(`{"Os":"linux","Config":{}}`)
		os.Exit(0)
	}
	if args[0] == "volume" {
		os.Exit(0)
	}
	if args[1] == "exec" {
		fmt.Print("ready")
		os.Exit(0)
	}
	if args[1] == "cp" {
		if args[2] == "-" {
			data, _ := io.ReadAll(os.Stdin)
			write("configuration.tar", data)
		} else {
			data, _ := os.ReadFile(filepath.Join(root, "configuration.tar"))
			if strings.Contains(args[2], ":/evaloom-auth/") {
				tr, tw := tar.NewReader(bytes.NewReader(data)), tar.NewWriter(os.Stdout)
				for {
					h, err := tr.Next()
					if err != nil {
						break
					}
					if h.Name == ".credentials.json" || h.Name == "auth.json" {
						_ = tw.WriteHeader(h)
						_, _ = io.Copy(tw, tr)
						break
					}
				}
				_ = tw.Close()
			} else {
				_, _ = os.Stdout.Write(data)
			}
		}
		os.Exit(0)
	}
	name := args[len(args)-1]
	if i := slices.Index(args, "--name"); i >= 0 {
		name = args[i+1]
	}
	if strings.HasSuffix(name, "-ingress") || strings.HasSuffix(name, "-egress") {
		if args[1] == "inspect" {
			network := "bridge"
			if strings.HasSuffix(name, "-ingress") {
				network = "none"
			}
			fmt.Printf(`{"State":{"Running":true},"HostConfig":{"NetworkMode":%q},"NetworkSettings":{"Networks":{%q:{}}}}`, network, network)
		}
		os.Exit(0)
	}
	switch args[1] {
	case "create":
		data, _ := json.Marshal(args)
		write(name+".json", data)
		if slices.Contains(args, "claude") || slices.Contains(args, "codex") {
			write("workload-created", data)
		}
	case "start":
		data, _ := os.ReadFile(filepath.Join(root, name+".json"))
		var create []string
		_ = json.Unmarshal(data, &create)
		if slices.Contains(create, "claude") || slices.Contains(create, "codex") {
			preflight := slices.Contains(create, "auth") || slices.Contains(create, "login")
			archive, _ := os.ReadFile(filepath.Join(root, "configuration.tar"))
			tr := tar.NewReader(bytes.NewReader(archive))
			var updated bytes.Buffer
			tw := tar.NewWriter(&updated)
			for {
				h, err := tr.Next()
				if err != nil {
					break
				}
				value, _ := io.ReadAll(tr)
				if h.Name == ".credentials.json" || h.Name == "auth.json" {
					fmt.Fprintln(os.Stderr, string(value))
					value = []byte(`{"token":"proposal-refreshed"}`)
					h.Size = int64(len(value))
				}
				_ = tw.WriteHeader(h)
				_, _ = tw.Write(value)
			}
			_ = tw.Close()
			write("configuration.tar", updated.Bytes())
			if !preflight {
				write("model-args", data)
				for i, arg := range create {
					if arg != "--mount" {
						continue
					}
					mount := create[i+1]
					if !strings.Contains(mount, "target=/evaloom/out,") {
						continue
					}
					source := strings.Split(strings.TrimPrefix(mount, "type=bind,source="), ",")[0]
					body := []byte(`{"label":"route:project","reason":"fits proposal-refreshed"}`)
					switch os.Getenv("PROPOSAL_RUNTIME_MODE") {
					case "extra":
						body = []byte(`{"label":"route:project","reason":"fits","extra":true}`)
					case "missing":
						continue
					case "symlink":
						_ = os.Symlink(filepath.Join(source, "other"), filepath.Join(source, "proposal.json"))
						_ = os.WriteFile(filepath.Join(source, "other"), body, 0600)
						continue
					}
					_ = os.WriteFile(filepath.Join(source, "proposal.json"), body, 0600)
				}
				if slices.Contains(create, "codex") {
					fmt.Println(`{"type":"thread.started","thread_id":"proposal-native"}`)
					fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"stdout is not the proposal"}}`)
					fmt.Println(`{"type":"turn.completed"}`)
				} else {
					fmt.Println(`{"type":"system","subtype":"init","session_id":"proposal-native"}`)
					fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"stdout is not the proposal"}`)
				}
			}
		}
	case "inspect":
		data, _ := os.ReadFile(filepath.Join(root, name+".json"))
		var create []string
		_ = json.Unmarshal(data, &create)
		preflight := slices.Contains(create, "auth") || slices.Contains(create, "login")
		code := 0
		if os.Getenv("PROPOSAL_RUNTIME_MODE") == "cli_failure" && !preflight && (slices.Contains(create, "claude") || slices.Contains(create, "codex")) {
			code = 1
		}
		if os.Getenv("PROPOSAL_RUNTIME_MODE") == "missing_cli" && preflight {
			code = 127
		}
		fmt.Printf(`{"Status":"exited","Running":false,"ExitCode":%d}`, code)
	case "rm", "kill", "ls":
	default:
		os.Exit(92)
	}
	os.Exit(0)
}

func proposalAdapters(w WorkAdapterConfig) *adapter.Registry {
	r := &adapter.Registry{}
	_ = r.Register("claude_code", claudecode.New(claudecode.Config{Launch: w.Launch, ResolveConfig: w.ResolveConfig, ResolveAuthentication: w.ResolveAuthentication, CancelGrace: w.CancelGrace}))
	_ = r.Register("codex", codex.New(codex.Config{Launch: w.Launch, ResolveConfig: w.ResolveConfig, ResolveAuthentication: w.ResolveAuthentication, CancelGrace: w.CancelGrace}))
	return r
}

func proposalRuntimeConfig(t *testing.T, config Config, mode string) (Config, string) {
	t.Helper()
	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config.Container.RuntimePath = filepath.Join(root, "runtime")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run='^TestProposalAdapterRuntime$' -- \"$@\"\n"
	if err := os.WriteFile(config.Container.RuntimePath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config.Container.ProxyBinary = filepath.Join(root, "proxy")
	if err := os.WriteFile(config.Container.ProxyBinary, []byte("fixture proxy"), 0700); err != nil {
		t.Fatal(err)
	}
	config.Container.RuntimeEnv = map[string]string{"PROPOSAL_RUNTIME_ROOT": root, "PROPOSAL_RUNTIME_MODE": mode, "GORACE": "atexit_sleep_ms=0"}
	store := filepath.Join(config.DataDir, "claude-auth.json")
	if err := os.MkdirAll(config.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, []byte(`{"token":"proposal-initial"}`), 0600); err != nil {
		t.Fatal(err)
	}
	config.MicroHarnessAuthFiles = map[string]string{"claude_code": store, "codex": filepath.Join(config.DataDir, "codex-auth.json")}
	if err := os.WriteFile(config.MicroHarnessAuthFiles["codex"], []byte(`{"token":"proposal-initial"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return config, root
}

func TestProposalUsesRealAdapterAndOutFile(t *testing.T) {
	for _, name := range []string{"claude_code", "codex"} {
		t.Run(name, func(t *testing.T) {
			for _, mode := range []string{"success", "extra", "missing", "symlink"} {
				t.Run(mode, func(t *testing.T) {
					db, config, _ := controlPlaneFixture(t, nil)
					config, state := proposalRuntimeConfig(t, config, mode)
					r, err := New(config)
					if err != nil {
						t.Fatal(err)
					}
					claim := proposalClaim(ulid.Make().String(), 30)
					claim.Proposer = json.RawMessage(fmt.Sprintf(`{"microHarness":%q,"model":"proposal-model"}`, name))
					insertRoutingProposal(t, db, claim.ID, ulid.Make().String(), config.RunnerID)
					services := ProposalServicesFromConfig(config)
					services.Adapters = proposalAdapters
					if err := r.ExecuteProposal(t.Context(), claim, services); err != nil {
						t.Fatal(err)
					}
					var status, label, reason, class string
					if err := db.QueryRow(`SELECT status,coalesce(proposed_label,''),coalesce(reason,''),coalesce(error_class,'') FROM route_proposals WHERE id=?`, claim.ID).Scan(&status, &label, &reason, &class); err != nil {
						t.Fatal(err)
					}
					if mode == "success" {
						if status != "proposed" || label != "route:project" || !strings.HasPrefix(reason, "fits ") || strings.Contains(reason, "proposal-refreshed") {
							t.Fatalf("result=%s %s %s %s", status, label, reason, class)
						}
					} else if status != "failed" || class != "contract_error" {
						t.Fatalf("result=%s class=%s", status, class)
					}
					auth, _ := os.ReadFile(config.MicroHarnessAuthFiles[name])
					if string(auth) != `{"token":"proposal-refreshed"}` {
						t.Fatal("updated proposal credential was lost")
					}
					args, err := os.ReadFile(filepath.Join(state, "model-args"))
					if err != nil {
						t.Fatal(err)
					}
					for _, hidden := range []string{"target=/evaloom/workspace", "target=/evaloom/profile", "target=/evaloom/records", config.TokenFile} {
						if strings.Contains(string(args), hidden) {
							t.Fatalf("hidden path exposed: %s", hidden)
						}
					}
					var command []string
					_ = json.Unmarshal(args, &command)
					required := []string{"Read,Write", "dontAsk", "proposal-model"}
					if name == "codex" {
						required = []string{"exec", "--ephemeral", "danger-full-access", "proposal-model"}
						if slices.Contains(command, "--cd") {
							t.Fatal("proposal received a work directory")
						}
					}
					for _, arg := range required {
						if !slices.Contains(command, arg) {
							t.Fatalf("missing fixed command setting %s", arg)
						}
					}
					if !strings.Contains(command[len(command)-1], "untrusted") {
						t.Fatal("input trust boundary missing")
					}
					for _, directory := range []string{"logs", "artifacts"} {
						files, err := os.ReadDir(filepath.Join(config.DataDir, directory))
						if directory == "artifacts" && (mode == "missing" || mode == "symlink") && os.IsNotExist(err) {
							continue
						}
						if err != nil {
							t.Fatal(err)
						}
						for _, file := range files {
							content, err := os.ReadFile(filepath.Join(config.DataDir, directory, file.Name()))
							if err != nil {
								t.Fatal(err)
							}
							for _, secret := range []string{"proposal-initial", "proposal-refreshed", r.token} {
								if strings.Contains(string(content), secret) {
									t.Fatalf("credential persisted in %s", directory)
								}
							}
						}
					}
					rows, err := db.Query(`SELECT kind,storage_path FROM artifacts WHERE route_proposal_id=?`, claim.ID)
					if err != nil {
						t.Fatal(err)
					}
					defer rows.Close()
					kinds := map[string]bool{}
					for rows.Next() {
						var kind, path string
						_ = rows.Scan(&kind, &path)
						kinds[kind] = true
					}
					if !kinds["log"] || kinds["proposal_result"] != (mode == "success" || mode == "extra") {
						t.Fatalf("artifacts=%v", kinds)
					}
					entries, _ := os.ReadDir(config.WorkspaceRoot)
					if len(entries) != 0 {
						t.Fatal("proposal allocated a workspace")
					}
				})
			}
		})
	}
}

func TestProposalWaitsForWorkAuthenticationLease(t *testing.T) {
	for _, name := range []string{"claude_code", "codex"} {
		t.Run(name, func(t *testing.T) {
			db, config, _ := controlPlaneFixture(t, nil)
			config, state := proposalRuntimeConfig(t, config, "success")
			lease, err := harnessenv.AcquireAuthentication(t.Context(), config.MicroHarnessAuthFiles[name])
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			r, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			claim := proposalClaim(ulid.Make().String(), 30)
			claim.Proposer = json.RawMessage(fmt.Sprintf(`{"microHarness":%q,"model":"m"}`, name))
			insertRoutingProposal(t, db, claim.ID, ulid.Make().String(), config.RunnerID)
			services := ProposalServicesFromConfig(config)
			services.Adapters = proposalAdapters
			done := make(chan error, 1)
			go func() { done <- r.ExecuteProposal(t.Context(), claim, services) }()
			select {
			case err := <-done:
				t.Fatalf("proposal did not wait for work lock: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			if _, err := os.Stat(filepath.Join(state, "workload-created")); !os.IsNotExist(err) {
				t.Fatal("CLI launched while work owns authentication")
			}
			_ = lease.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("proposal did not acquire released work lock")
			}
		})
	}
}

func TestProposalOutputValidation(t *testing.T) {
	for _, body := range []string{`{"label":"r","reason":"x","extra":1}`, `{"label":"r","reason":null}`, `{"label":"r","reason":"` + strings.Repeat("あ", 2001) + `"}`, strings.Repeat(" ", 64<<10) + `{}`} {
		if _, _, err := readProposalOutcomeBytes([]byte(body)); err == nil {
			t.Fatal("invalid proposal accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h := codex.New(codex.Config{}).(adapter.ProposalAdapter)
	if _, err := h.StartProposal(ctx, adapter.ProposalRequest{}, adapter.SandboxEnv{}); err != context.Canceled {
		t.Fatalf("cancelled proposal=%v", err)
	}
}

func TestProposalAuthenticationWaitCanStop(t *testing.T) {
	for _, name := range []string{"claude_code", "codex"} {
		t.Run(name, func(t *testing.T) {
			for _, mode := range []string{"cancel", "timeout"} {
				t.Run(mode, func(t *testing.T) {
					db, config, _ := controlPlaneFixture(t, nil)
					config, state := proposalRuntimeConfig(t, config, "success")
					lease, err := harnessenv.AcquireAuthentication(t.Context(), config.MicroHarnessAuthFiles[name])
					if err != nil {
						t.Fatal(err)
					}
					defer lease.Close()
					r, err := New(config)
					if err != nil {
						t.Fatal(err)
					}
					claim := proposalClaim(ulid.Make().String(), 1)
					claim.Proposer = json.RawMessage(fmt.Sprintf(`{"microHarness":%q,"model":"m"}`, name))
					insertRoutingProposal(t, db, claim.ID, ulid.Make().String(), config.RunnerID)
					services := ProposalServicesFromConfig(config)
					services.Adapters = proposalAdapters
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- r.ExecuteProposal(ctx, claim, services) }()
					if mode == "cancel" {
						time.Sleep(100 * time.Millisecond)
						cancel()
					}
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("authentication lock wait did not stop")
					}
					if _, err := os.Stat(filepath.Join(state, "workload-created")); !os.IsNotExist(err) {
						t.Fatal("CLI started while authentication was locked")
					}
					if mode == "timeout" {
						var class string
						if err := db.QueryRow(`SELECT error_class FROM route_proposals WHERE id=?`, claim.ID).Scan(&class); err != nil || class != "timeout" {
							t.Fatalf("timeout class=%s err=%v", class, err)
						}
					}
				})
			}
		})
	}
}

func TestProposalFileRejectsNonRegularAndOversized(t *testing.T) {
	for _, mode := range []string{"symlink", "directory", "fifo", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proposal.json")
			switch mode {
			case "symlink":
				_ = os.WriteFile(path+".target", []byte(`{"label":"r","reason":"x"}`), 0600)
				_ = os.Symlink(path+".target", path)
			case "directory":
				_ = os.Mkdir(path, 0700)
			case "fifo":
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				_ = os.WriteFile(path, bytes.Repeat([]byte(" "), (64<<10)+1), 0600)
			}
			if _, err := readProposalFile(path); err == nil {
				t.Fatalf("%s output accepted", mode)
			}
		})
	}
}

func TestProposalAdapterFailuresReportAndReleaseCapacity(t *testing.T) {
	for _, name := range []string{"claude_code", "codex"} {
		for _, mode := range []string{"missing_auth_config", "missing_auth_file", "missing_cli", "cli_failure", "unknown_adapter"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				db, config, _ := controlPlaneFixture(t, nil)
				config, state := proposalRuntimeConfig(t, config, mode)
				if mode == "missing_auth_config" {
					delete(config.MicroHarnessAuthFiles, name)
				} else if mode == "missing_auth_file" {
					config.MicroHarnessAuthFiles[name] = filepath.Join(config.DataDir, "absent.json")
				}
				r, err := New(config)
				if err != nil {
					t.Fatal(err)
				}
				claim := proposalClaim(ulid.Make().String(), 30)
				claim.Proposer = json.RawMessage(fmt.Sprintf(`{"microHarness":%q,"model":"m"}`, name))
				if mode == "unknown_adapter" {
					claim.Proposer = json.RawMessage(`{"microHarness":"unknown","model":"m"}`)
				}
				insertRoutingProposal(t, db, claim.ID, ulid.Make().String(), config.RunnerID)
				// Fill the other slot and queue a replacement. A successful claim
				// after reporting proves that Control Plane capacity was freed.
				insertRoutingProposal(t, db, ulid.Make().String(), ulid.Make().String(), config.RunnerID)
				nextID := ulid.Make().String()
				insertRoutingProposal(t, db, nextID, ulid.Make().String(), config.RunnerID)
				runnerJSON, _ := json.Marshal(config.RunnerID)
				for key, value := range map[string]string{
					"routing.defaultRunnerId": string(runnerJSON), "routing.proposer": string(claim.Proposer),
				} {
					if _, err := db.Exec(`INSERT INTO settings(key,value,updated_at) VALUES(?,?,'2026-11-17T00:00:00Z')
						ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := db.Exec(`UPDATE route_proposals SET status='queued',runner_id=NULL WHERE id=?`, nextID); err != nil {
					t.Fatal(err)
				}
				if next, err := r.claim(t.Context()); err != nil || next != nil {
					t.Fatalf("full capacity claim=%+v err=%v", next, err)
				}
				services := ProposalServicesFromConfig(config)
				services.Adapters = proposalAdapters
				if err := r.ExecuteProposal(t.Context(), claim, services); err != nil {
					t.Fatal(err)
				}
				want := "config_error"
				if mode == "cli_failure" {
					want = "work_failed"
				}
				var status, class string
				if err := db.QueryRow(`SELECT status,error_class FROM route_proposals WHERE id=?`, claim.ID).Scan(&status, &class); err != nil || status != "failed" || class != want {
					t.Fatalf("status=%s class=%s err=%v; want failed/%s", status, class, err, want)
				}
				var decisions int
				if err := db.QueryRow(`SELECT count(*) FROM owner_decisions WHERE subject_id=? AND kind='route' AND status='open'`, claim.ID).Scan(&decisions); err != nil || decisions != 1 {
					t.Fatalf("decisions=%d err=%v", decisions, err)
				}
				if mode == "missing_auth_config" || mode == "missing_auth_file" || mode == "unknown_adapter" {
					if _, err := os.Stat(filepath.Join(state, "workload-created")); !os.IsNotExist(err) {
						t.Fatal("CLI launched without configuration/authentication")
					}
				} else {
					auth, err := os.ReadFile(config.MicroHarnessAuthFiles[name])
					if err != nil || string(auth) != `{"token":"proposal-refreshed"}` {
						t.Fatal("failed CLI lost refreshed authentication:", err)
					}
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					lease, err := harnessenv.AcquireAuthentication(ctx, config.MicroHarnessAuthFiles[name])
					if err != nil {
						t.Fatal("failed CLI kept authentication lock:", err)
					}
					_ = lease.Close()
				}
				next, err := r.claim(t.Context())
				if err != nil || next == nil || next.Kind != "route_proposal" || next.Proposal.ID != nextID {
					t.Fatalf("released capacity claim=%+v err=%v", next, err)
				}
			})
		}
	}
}

func TestProposalArtifactOwnershipAndRetry(t *testing.T) {
	db, config, _ := controlPlaneFixture(t, nil)
	r, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	proposalID := ulid.Make().String()
	insertRoutingProposal(t, db, proposalID, ulid.Make().String(), config.RunnerID)
	path := filepath.Join(t.TempDir(), "sanitized")
	_ = os.WriteFile(path, []byte(`{"label":"route:project","reason":"fits"}`), 0600)
	a := LogArtifact{ID: ulid.Make().String(), RouteProposalID: proposalID, Stage: "proposal", Kind: "proposal_result", Name: "proposal.json", Path: path}
	for range 2 {
		if err := r.registerArtifact(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	_ = db.QueryRow(`SELECT count(*) FROM artifacts WHERE route_proposal_id=?`, proposalID).Scan(&count)
	if count != 1 {
		t.Fatal("duplicate proposal artifact after retry")
	}
	_, err = db.Exec(`UPDATE route_proposals SET runner_id=NULL WHERE id=?`, proposalID)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = ulid.Make().String()
	if err := r.registerArtifact(t.Context(), a); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("foreign proposal artifact=%v", err)
	}
	_ = db.QueryRow(`SELECT count(*) FROM artifacts WHERE route_proposal_id=?`, proposalID).Scan(&count)
	if count != 1 {
		t.Fatal("foreign Runner wrote proposal artifact")
	}
}
