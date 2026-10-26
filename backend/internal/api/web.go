package api

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
)

const webIndex = "index.html"

// webHandler serves the built Management UI (architecture 3.1). Static assets
// carry no credentials, so they are reachable before login; the owner APIs stay
// behind Protect. Paths without a file extension fall back to index.html so the
// SPA can handle client-side routes. API and auth namespaces never fall back.
func webHandler(web fs.FS, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	notFound := func(w http.ResponseWriter) {
		WriteError(w, logger, &Error{Code: NotFound, Message: "not found"})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			notFound(w)
			return
		}
		if p == "/api" || strings.HasPrefix(p, "/api/") || p == "/auth" || strings.HasPrefix(p, "/auth/") {
			notFound(w)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+p), "/")
		if name != "" && name != webIndex {
			if hiddenPath(name) || !fs.ValidPath(name) {
				notFound(w)
				return
			}
			served, err := serveWebFile(w, r, web, name)
			if err != nil {
				// Includes paths that os.Root refuses because they leave the UI directory.
				logger.Warn("Management UI file not served", "path", name, "error", err)
				notFound(w)
				return
			}
			if served {
				return
			}
			if path.Ext(name) != "" {
				// A missing asset must not be answered with HTML.
				notFound(w)
				return
			}
		}
		served, err := serveWebFile(w, r, web, webIndex)
		if err != nil {
			WriteError(w, logger, err)
		} else if !served {
			logger.Error("Management UI index.html is missing")
			notFound(w)
		}
	})
}

func hiddenPath(name string) bool {
	for _, segment := range strings.Split(name, "/") {
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}
	return false
}

// serveWebFile reports false without writing when name is not a regular file.
func serveWebFile(w http.ResponseWriter, r *http.Request, web fs.FS, name string) (bool, error) {
	file, err := web.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	content, ok := file.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(file)
		if err != nil {
			return false, err
		}
		content = strings.NewReader(string(data))
	}
	http.ServeContent(w, r, name, time.Time{}, content)
	return true, nil
}
