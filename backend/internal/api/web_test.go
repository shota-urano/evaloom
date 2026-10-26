package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const testIndex = `<!doctype html><div id="root"></div>`

func newWebHandler(t *testing.T) http.Handler {
	t.Helper()
	web := fstest.MapFS{
		"index.html":        {Data: []byte(testIndex)},
		"assets/app-1a.js":  {Data: []byte("console.log('ui')")},
		"assets/app-1a.css": {Data: []byte("body{}")},
		".secret":           {Data: []byte("hidden")},
	}
	db := runnerDB(t, 0)
	auth, err := NewOwnerAuth(db, nil, "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithWeb(db, nil, auth, web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "127.0.0.1:8080"
		handler.ServeHTTP(w, r)
	})
}

func serve(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

// Architecture 3.1: evaloomd serves the built UI on the same origin, and the
// login screen must be reachable without a session.
func TestWebServesIndexAndAssetsWithoutSession(t *testing.T) {
	handler := newWebHandler(t)
	for _, target := range []string{"/", "/index.html"} {
		w := serve(handler, "GET", target)
		if w.Code != 200 || w.Body.String() != testIndex || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: status=%d type=%q body=%q", target, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	w := serve(handler, "GET", "/assets/app-1a.js")
	if w.Code != 200 || w.Body.String() != "console.log('ui')" || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("asset: status=%d type=%q body=%q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if w := serve(handler, "HEAD", "/assets/app-1a.css"); w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("HEAD asset: status=%d type=%q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestWebFallsBackToIndexForClientRoutes(t *testing.T) {
	handler := newWebHandler(t)
	for _, target := range []string{"/projects", "/tasks/task-001", "/assets", "/login?next=%2F"} {
		w := serve(handler, "GET", target)
		if w.Code != 200 || w.Body.String() != testIndex {
			t.Fatalf("%s: status=%d body=%q", target, w.Code, w.Body.String())
		}
	}
}

func TestWebDoesNotShadowAPIOrServeMissingAndHiddenFiles(t *testing.T) {
	handler := newWebHandler(t)
	for _, target := range []string{"/api/missing", "/api/runners", "/auth/other", "/assets/missing.js", "/.secret", "/assets/.hidden/x"} {
		w := serve(handler, "GET", target)
		if w.Code == 200 || strings.Contains(w.Body.String(), "<div") || strings.Contains(w.Body.String(), "hidden") {
			t.Fatalf("%s: status=%d body=%q", target, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"error":{"code":`) {
			t.Fatalf("%s: want common error envelope, got %q", target, w.Body.String())
		}
	}
	// Owner APIs remain protected: no session is 401, not the SPA.
	if w := serve(handler, "GET", "/api/runners"); w.Code != 401 {
		t.Fatalf("/api/runners without session: %d", w.Code)
	}
}

func TestWebRejectsStateChangingMethods(t *testing.T) {
	handler := newWebHandler(t)
	r := httptest.NewRequest("POST", "/projects", nil)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 404 || strings.Contains(w.Body.String(), "<div") {
		t.Fatalf("POST: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestWebKeepsHostBoundary(t *testing.T) {
	db := runnerDB(t, 0)
	auth, err := NewOwnerAuth(db, nil, "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	raw := NewHandlerWithWeb(db, nil, auth, fstest.MapFS{"index.html": {Data: []byte(testIndex)}})
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example:8080"
	w := httptest.NewRecorder()
	raw.ServeHTTP(w, r)
	if w.Code != 403 || strings.Contains(w.Body.String(), "<div") {
		t.Fatalf("foreign host: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestWebRootCannotEscapeThroughSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(testIndex), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	db := runnerDB(t, 0)
	auth, err := NewOwnerAuth(db, nil, "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithWeb(db, nil, auth, root.FS())
	r := httptest.NewRequest("GET", "/leak.txt", nil)
	r.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 404 || strings.Contains(w.Body.String(), "outside secret") {
		t.Fatalf("symlink escape: status=%d body=%q", w.Code, w.Body.String())
	}
}
