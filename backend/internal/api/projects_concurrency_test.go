package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

// Hold the first real Git HTTP request until released. Later requests serve the
// same bare repository immediately, allowing a competing mutation to finish.
// Channels establish the ordering; timeouts are only deadlock watchdogs.
func gateProjectRemote(t *testing.T, input map[string]any) (entered, disconnected <-chan struct{}, release func()) {
	t.Helper()
	remote := input["gitRemoteUrl"].(string)
	projectGit(t, remote, "update-server-info")
	files := http.FileServer(http.Dir(remote))
	arrived, done, unblock := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var once sync.Once
	release = func() { once.Do(func() { close(unblock) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !first.Swap(true) {
			close(arrived)
			defer close(done)
			select {
			case <-unblock:
			case <-r.Context().Done():
				return
			}
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	projectGit(t, input["localRepoPath"].(string), "remote", "set-url", "origin", server.URL)
	input["gitRemoteUrl"] = server.URL
	return arrived, done, release
}

type projectResponse struct {
	status int
	body   []byte
	err    error
}

func startProjectRequest(t *testing.T, f *authFixture, method, endpoint string, value any) <-chan projectResponse {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	r, err := http.NewRequestWithContext(ctx, method, f.server.URL+endpoint, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r.AddCookie(f.cookie)
	r.Header.Set("Origin", f.server.URL)
	r.Header.Set("X-CSRF-Token", f.csrf)
	r.Header.Set("Content-Type", "application/json")
	result := make(chan projectResponse, 1)
	go func() {
		response, err := f.server.Client().Do(r)
		if err != nil {
			result <- projectResponse{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		result <- projectResponse{response.StatusCode, body, err}
	}()
	return result
}

func awaitProjectResponse(t *testing.T, result <-chan projectResponse, watchdog time.Duration) projectResponse {
	t.Helper()
	select {
	case response := <-result:
		if response.err != nil {
			t.Fatal(response.err)
		}
		return response
	case <-time.After(watchdog):
		t.Fatal("request did not finish while remote Git validation was held")
		return projectResponse{}
	}
}

func awaitProjectSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("Git HTTP request did not reach the expected synchronization point")
	}
}

func TestProjectsRemoteValidationDoesNotBlockList(t *testing.T) {
	for _, method := range []string{"POST", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			f := projectFixture(t)
			input := projectInput(t, "project-a")
			endpoint, wantStatus, wantListed := "/api/projects", 201, 0
			if method == "PATCH" {
				status, body := projectRequest(t, f, "POST", endpoint, input)
				requireProject(t, status, body, 201)
				endpoint, wantStatus, wantListed = endpoint+"/project-a", 200, 1
			}
			entered, _, release := gateProjectRemote(t, input)
			var request any = input
			if method == "PATCH" {
				request = map[string]any{"gitRemoteUrl": input["gitRemoteUrl"], "name": "Updated"}
			}
			pending := startProjectRequest(t, f, method, endpoint, request)
			awaitProjectSignal(t, entered)
			response := awaitProjectResponse(t, startProjectRequest(t, f, "GET", "/api/projects", nil), 5*time.Second)
			var page Page[model.Project]
			if err := json.Unmarshal(response.body, &page); err != nil || response.status != 200 || len(page.Items) != wantListed {
				t.Fatalf("list during Git validation: %d %s err=%v", response.status, response.body, err)
			}
			select {
			case response := <-pending:
				t.Fatalf("mutation finished before remote was released: %+v", response)
			default:
			}
			release()
			response = awaitProjectResponse(t, pending, 5*time.Second)
			requireProject(t, response.status, response.body, wantStatus)
		})
	}
}

func TestProjectsPatchRejectsChangesDuringValidation(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	status, body := projectRequest(t, f, "POST", "/api/projects", input)
	before := requireProject(t, status, body, 201)
	entered, _, release := gateProjectRemote(t, input)
	pending := startProjectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{
		"gitRemoteUrl": input["gitRemoteUrl"], "testCommand": "stale command",
	})
	awaitProjectSignal(t, entered)
	winner := awaitProjectResponse(t, startProjectRequest(t, f, "PATCH", "/api/projects/project-a", map[string]any{
		"gitRemoteUrl": input["gitRemoteUrl"], "name": "Concurrent winner",
	}), 5*time.Second)
	after := requireProject(t, winner.status, winner.body, 200)
	release()
	loser := awaitProjectResponse(t, pending, 5*time.Second)
	requireProjectError(t, loser.status, loser.body, 409, Conflict, "", "")
	status, body = projectRequest(t, f, "GET", "/api/projects/project-a", nil)
	if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, after) || got.Name != "Concurrent winner" || got.TestCommand != nil {
		t.Fatalf("stale PATCH overwrote winner: %+v", got)
	}
	var detail string
	if err := f.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = 'project.update'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var history struct{ Before, After model.ProjectSettings }
	if err := json.Unmarshal([]byte(detail), &history); err != nil ||
		!reflect.DeepEqual(history.Before, before.ProjectSettings) || !reflect.DeepEqual(history.After, after.ProjectSettings) ||
		projectCount(t, f, "audit_logs") != 2 {
		t.Fatalf("conflict changed audit history: %s err=%v", detail, err)
	}
}

func TestProjectsRecheckDuplicatePathAfterValidation(t *testing.T) {
	for _, method := range []string{"POST", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			f := projectFixture(t)
			endpoint, wantCount := "/api/projects", 1
			var before model.Project
			if method == "PATCH" {
				status, body := projectRequest(t, f, "POST", endpoint, projectInput(t, "project-a"))
				before = requireProject(t, status, body, 201)
				endpoint, wantCount = endpoint+"/project-a", 2
			}
			input := projectInput(t, "project-a")
			entered, _, release := gateProjectRemote(t, input)
			var request any = input
			if method == "PATCH" {
				request = map[string]any{"localRepoPath": input["localRepoPath"], "gitRemoteUrl": input["gitRemoteUrl"]}
			}
			pending := startProjectRequest(t, f, method, endpoint, request)
			awaitProjectSignal(t, entered)
			// Register a symlink alias while the first request is validating Git.
			link := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(input["localRepoPath"].(string), link); err != nil {
				t.Fatal(err)
			}
			input["id"], input["localRepoPath"] = "project-b", link
			winner := awaitProjectResponse(t, startProjectRequest(t, f, "POST", "/api/projects", input), 5*time.Second)
			requireProject(t, winner.status, winner.body, 201)
			release()
			loser := awaitProjectResponse(t, pending, 5*time.Second)
			requireProjectError(t, loser.status, loser.body, 409, Conflict, "", "")
			if method == "PATCH" {
				status, body := projectRequest(t, f, "GET", endpoint, nil)
				if got := requireProject(t, status, body, 200); !reflect.DeepEqual(got, before) {
					t.Fatalf("duplicate-path PATCH changed project: %+v", got)
				}
			}
			if projectCount(t, f, "projects") != wantCount || projectCount(t, f, "audit_logs") != wantCount {
				t.Fatal("rejected duplicate changed projects or audit history")
			}
		})
	}
}

func TestProjectsRecheckRunnerAfterValidation(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	entered, _, release := gateProjectRemote(t, input)
	pending := startProjectRequest(t, f, "POST", "/api/projects", input)
	awaitProjectSignal(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.db.ExecContext(ctx, `UPDATE runners SET worktree_root = ? WHERE id = 'mac-mini'`, input["localRepoPath"]); err != nil {
		t.Fatal(err)
	}
	release()
	response := awaitProjectResponse(t, pending, 5*time.Second)
	requireProjectError(t, response.status, response.body, 409, Conflict, "", "")
	if projectCount(t, f, "projects") != 0 || projectCount(t, f, "audit_logs") != 0 {
		t.Fatal("saved a path validated against a stale Runner Worktree Root")
	}
}

func TestProjectsRemoteValidationDeadlineFailsClosed(t *testing.T) {
	f := projectFixture(t)
	input := projectInput(t, "project-a")
	entered, disconnected, _ := gateProjectRemote(t, input)
	pending := startProjectRequest(t, f, "POST", "/api/projects", input)
	awaitProjectSignal(t, entered)
	// Never release the HTTP response: the production deadline must terminate
	// Git and its transport helper, returning the existing 422 reason.
	response := awaitProjectResponse(t, pending, 20*time.Second)
	requireProjectError(t, response.status, response.body, 422, PreconditionFailed, "baseBranch", "remote_unavailable")
	awaitProjectSignal(t, disconnected)
	if projectCount(t, f, "projects") != 0 || projectCount(t, f, "audit_logs") != 0 {
		t.Fatal("remote validation timeout changed projects or audit history")
	}
}
