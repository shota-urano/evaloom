package containers_test

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type instruction struct{ name, value string }

// These recipes deliberately use a small Dockerfile subset. Fail closed on
// additions (including ONBUILD, COPY, ADD, ENV and VOLUME), rather than trying
// to identify every possible credential filename or token format.
func parseRecipe(source string) ([]instruction, error) {
	var result []instruction
	var continued string
	scanner := bufio.NewScanner(strings.NewReader(source))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			continued += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		fields := strings.Fields(continued + line)
		continued = ""
		if len(fields) < 2 {
			return nil, fmt.Errorf("invalid instruction: %s", line)
		}
		result = append(result, instruction{strings.ToUpper(fields[0]), strings.Join(fields[1:], " ")})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if continued != "" {
		return nil, fmt.Errorf("unfinished continuation")
	}
	return result, nil
}

func recipePolicy(recipe []instruction, work bool) error {
	base := "alpine:3.20"
	if work {
		base = "node:22-bookworm-slim"
	}
	if len(recipe) == 0 || recipe[0] != (instruction{"FROM", base}) {
		return fmt.Errorf("expected approved Linux base %s", base)
	}
	// Only public package installation, required version guards and removal of
	// apt metadata are permitted. No file imports, arbitrary shell, environment
	// assignments, credential defaults, build secrets or extra stages can pass.
	allowedCommands := map[string]bool{
		`test -n "$CODEX_VERSION"`:  work,
		`test -n "$CLAUDE_VERSION"`: work,
		`apt-get update`:            work,
		`apt-get install -y --no-install-recommends python3 git ca-certificates`:                              work,
		`npm install --global "@openai/codex@${CODEX_VERSION}" "@anthropic-ai/claude-code@${CLAUDE_VERSION}"`: work,
		`rm -rf /var/lib/apt/lists/*`:                  work,
		`apk add --no-cache ca-certificates coreutils`: !work,
	}
	args := map[string]bool{}
	for _, step := range recipe[1:] {
		switch step.name {
		case "ARG":
			if !work || (step.value != "CODEX_VERSION" && step.value != "CLAUDE_VERSION") || args[step.value] {
				return fmt.Errorf("unexpected or defaulted ARG %q", step.value)
			}
			args[step.value] = true
		case "RUN":
			for _, command := range strings.Split(step.value, "&&") {
				if !allowedCommands[strings.TrimSpace(command)] {
					return fmt.Errorf("unapproved build command %q", command)
				}
			}
		default:
			return fmt.Errorf("unapproved instruction %s", step.name)
		}
	}
	if work && (!args["CODEX_VERSION"] || !args["CLAUDE_VERSION"]) {
		return fmt.Errorf("both CLI version arguments must be declared without defaults")
	}
	return nil
}

func readRecipe(t *testing.T, name string) []instruction {
	t.Helper()
	source, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := parseRecipe(string(source))
	if err != nil {
		t.Fatal(err)
	}
	return recipe
}

func TestProductionDockerfilesPolicy(t *testing.T) {
	for _, name := range []string{"Dockerfile.work", "Dockerfile.proxy"} {
		t.Run(name, func(t *testing.T) {
			recipe := readRecipe(t, name)
			if err := recipePolicy(recipe, name == "Dockerfile.work"); err != nil {
				t.Fatal(err)
			}
			if name == "Dockerfile.proxy" && buildCommands(recipe) != "apk add --no-cache ca-certificates coreutils" {
				t.Fatal("proxy must install CA certificates and /usr/bin/env (coreutils) on the BusyBox base")
			}
		})
	}
}

func buildCommands(recipe []instruction) string {
	var commands []string
	for _, step := range recipe {
		if step.name == "RUN" {
			commands = append(commands, step.value)
		}
	}
	return strings.Join(commands, " && ")
}

func TestWorkImageRequiresExplicitCLIVersions(t *testing.T) {
	recipe := readRecipe(t, "Dockerfile.work")
	if err := recipePolicy(recipe, true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  []string
		ok   bool
	}{
		{"both_unset", nil, false},
		{"codex_unset", []string{"CLAUDE_VERSION=2.1.316"}, false},
		{"claude_unset", []string{"CODEX_VERSION=0.163.0"}, false},
		{"both_empty", []string{"CODEX_VERSION=", "CLAUDE_VERSION="}, false},
		{"codex_empty", []string{"CODEX_VERSION=", "CLAUDE_VERSION=2.1.316"}, false},
		{"claude_empty", []string{"CODEX_VERSION=0.163.0", "CLAUDE_VERSION="}, false},
		{"both_explicit", []string{"CODEX_VERSION=0.163.0", "CLAUDE_VERSION=2.1.316"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			log := filepath.Join(root, "calls")
			// Execute the actual RUN shell expressions. All package operations
			// and cleanup are stubs; no Docker, network or host package changes.
			for _, name := range []string{"apt-get", "npm", "rm"} {
				stub := "#!/bin/sh\nprintf '%s' \"${0##*/}\" >> \"$CALLS\"\nprintf ' %s' \"$@\" >> \"$CALLS\"\nprintf '\\n' >> \"$CALLS\"\n"
				if err := os.WriteFile(filepath.Join(root, name), []byte(stub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("/bin/sh", "-c", buildCommands(recipe))
			cmd.Env = append([]string{"PATH=" + root, "CALLS=" + log}, tc.env...)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("build RUN result = %v; want success=%v; output=%s", err, tc.ok, output)
			}
			calls, readErr := os.ReadFile(log)
			if !tc.ok {
				if !os.IsNotExist(readErr) {
					t.Fatalf("missing version must fail before package operations: %s (%v)", calls, readErr)
				}
				return
			}
			want := "apt-get update\napt-get install -y --no-install-recommends python3 git ca-certificates\nnpm install --global @openai/codex@0.163.0 @anthropic-ai/claude-code@2.1.316\nrm -rf /var/lib/apt/lists/*\n"
			if readErr != nil || string(calls) != want {
				t.Fatalf("package operations = %q (%v); want %q", calls, readErr, want)
			}
		})
	}
}

func TestProductionDockerfilePolicyRejectsUnsafeInstructions(t *testing.T) {
	source, err := os.ReadFile("Dockerfile.work")
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{
		`VOLUME ["/data"]`, `volume /data`, `ONBUILD VOLUME /data`,
		`COPY auth.json /root/.codex/auth.json`, `COPY . /app`, `ADD key.pem /key.pem`,
		`ENV OPENAI_API_KEY=example`, `ENV PATH=/custom`,
		`ARG TOKEN`, `ARG TOKEN=example`, `ARG CODEX_VERSION=0.163.0`,
		`RUN echo example > /token`, `RUN --mount=type=secret,id=auth cat /run/secrets/auth`,
		`FROM private-image:local`,
	} {
		t.Run(extra, func(t *testing.T) {
			recipe, err := parseRecipe(string(source) + "\n" + extra + "\n")
			if err != nil {
				t.Fatal(err)
			}
			if err := recipePolicy(recipe, true); err == nil {
				t.Fatalf("unsafe addition accepted: %s", extra)
			}
		})
	}
}
