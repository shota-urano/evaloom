package runner

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/adapter"
	"github.com/shota-urano/evaloom/backend/internal/adapter/dummy"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

// Exercise production assembly and container mount enforcement from the stage
// frame, with a fake runtime and deterministic stage output. Real isolation
// tests belong to evaloom-7j6.2.
func TestAssembledStagesReachRuntimeWithRestrictedCapabilities(t *testing.T) {
	r, claim, _, services := stageFixture(t, true)
	config := testConfig(t, "https://control.example.test")
	config.DataDir = r.dataDir
	state := t.TempDir()
	config.Container.RuntimePath = filepath.Join(state, "runtime")
	config.Container.RuntimeEnv = map[string]string{"ASSEMBLY_STATE": state}
	if err := os.WriteFile(config.Container.RuntimePath, []byte(`#!/bin/sh
case "$1 $2" in
 'image inspect') printf '%s' '{"Os":"linux","Config":{}}' ;;
 'container create') printf '%s\n' "$@" > "$ASSEMBLY_STATE/args" ;;
 'container inspect') printf '%s' '{"Status":"exited","Running":false,"ExitCode":0}' ;;
 'container start'|'container rm') ;;
 *) exit 99 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	e := &executionExecutor{runner: r, config: config, claim: claim, paths: services.Paths, adapters: services.Adapters}
	services.Executor = stageExecutorFunc(func(ctx context.Context, in StageInvocation) StageOutcome {
		spec, config, err := e.stageSandbox(in)
		if err != nil {
			t.Fatal(err)
		}
		spec, err = in.SandboxSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		if in.Stage == model.StageWork || in.Stage == model.StageCheck {
			if len(spec.Secrets) != 0 {
				t.Fatal("work/check received keys")
			}
		}
		if in.Stage == model.StagePublish && spec.Secrets["delivery"] != "delivery-private-value" {
			t.Fatal("publish did not receive its declared key")
		}
		box, err := sandbox.NewContainer(config)
		if err != nil {
			t.Fatal(err)
		}
		process, err := box.RunControlled(ctx, spec, []string{"fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Wait(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(state, "args"))
		if err != nil {
			t.Fatal(err)
		}
		args := string(raw)
		workspacePath, err := filepath.EvalSymlinks(in.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		profilePath, err := filepath.EvalSymlinks(in.Profile.Path)
		if err != nil {
			t.Fatal(err)
		}
		workspace := "type=bind,source=" + workspacePath + ",target=" + e.paths.Workspace + ",bind-recursive=disabled"
		if in.Stage == model.StageCheck || in.Stage == model.StagePublish {
			workspace += ",readonly"
		}
		if !strings.Contains(args, workspace+"\n") {
			t.Fatalf("wrong workspace permissions for %s: %s", in.Stage, args)
		}
		if in.Stage == model.StageWork && strings.Contains(args, "source="+profilePath+",") {
			t.Fatal("work can see full Profile")
		}
		if in.Stage == model.StageWork || in.Stage == model.StageCheck {
			if strings.Contains(args, "EVALOOM_SECRET_") {
				t.Fatal("runtime imported keys into work/check")
			}
		}
		writeStageOutput(t, in)
		return successfulStage(in.Stage)
	})
	if err := r.ExecuteStages(t.Context(), claim, services); err != nil {
		t.Fatal(err)
	}
}

func TestWorkStageConfigurationExcludesUndeclaredProfileFiles(t *testing.T) {
	// The existing directory/pinning test verifies exact file contents too.
	// Here inspect the configuration passed by production runWork to its Adapter.
	f := newLogControlPlane(t)
	p := newProfileFixture(t)
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	claim := p.claim
	claim.Execution.ID = logExecution
	if _, err := f.db.Exec(`UPDATE harness_profiles SET definition=? WHERE id='fixture-profile'`, string(claim.Profile.Definition)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE executions SET profile_id='fixture-profile',profile_commit_sha=?,harness_id='fixture-harness' WHERE id=?`, claim.Profile.CommitSHA, logExecution); err != nil {
		t.Fatal(err)
	}
	e := &executionExecutor{runner: r, config: f.config, claim: claim, paths: DefaultProposalPaths}
	e.config.MicroHarnessAuthFiles = map[string]string{"codex": "unused-by-dummy"}
	in := StageInvocation{Stage: model.StageWork, Profile: LoadedProfile{Definition: p.def}, Directory: t.TempDir(), timeout: time.Minute}
	started := false
	in.Adapter = dummy.NewWithStart(func(_ adapter.HarnessRequest, env adapter.SandboxEnv) error {
		started = true
		want := map[string]bool{"AGENTS.md": true, "work/instructions.md": true, "work/skills/test/SKILL.md": true, "work/settings.json": true}
		if err := filepath.WalkDir(env.ConfigDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			name, err := filepath.Rel(env.ConfigDir, path)
			if err != nil {
				return err
			}
			if !want[name] {
				t.Errorf("undeclared Profile file exposed to work: %s", name)
			}
			delete(want, name)
			return nil
		}); err != nil {
			return err
		}
		if len(want) != 0 {
			t.Errorf("declared work files missing: %v", want)
		}
		return nil
	})
	if err := os.WriteFile(filepath.Join(in.Directory, "request.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := r.NewLog(logExecution, "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Finish(false)
	outcome := e.runWork(t.Context(), in, sandbox.SandboxSpec{}, sandbox.ContainerConfig{}, log)
	if outcome.RunnerError != nil || !started {
		t.Fatalf("work did not succeed: %+v", outcome)
	}
}
