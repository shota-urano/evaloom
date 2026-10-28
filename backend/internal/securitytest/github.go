package securitytest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// Access is recorded before authorization/routing, including rejected requests.
// It is independent of files writable by the harness. Token values are omitted.
type Access struct {
	Method, Path, Query string
	Authorized          bool
	Body                []byte
}

type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// GitHub implements only the local fake's PR create/list/get surface. Unknown
// operations fail, so accidentally invoking merge or a real API cannot pass.
type GitHub struct {
	Server            *httptest.Server
	mu                sync.Mutex
	repository, token string
	accesses          []Access
	pulls             []PullRequest
}

func NewGitHub(repository, token string) *GitHub {
	g := &GitHub{repository: repository, token: token}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serveHTTP))
	return g
}

func (g *GitHub) Close() { g.Server.Close() }

func (g *GitHub) Accesses() []Access {
	g.mu.Lock()
	defer g.mu.Unlock()
	accesses := append([]Access{}, g.accesses...)
	for i := range accesses {
		accesses[i].Body = append([]byte{}, accesses[i].Body...)
	}
	return accesses
}

func (g *GitHub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	g.mu.Lock()
	defer g.mu.Unlock()
	authorized := g.token != "" && r.Header.Get("Authorization") == "Bearer "+g.token
	g.accesses = append(g.accesses, Access{r.Method, r.URL.Path, r.URL.RawQuery, authorized, body})
	w.Header().Set("Content-Type", "application/json")
	if !authorized {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, `{"message":"Invalid body"}`, http.StatusBadRequest)
		return
	}
	path := "/repos/" + g.repository + "/pulls"
	write := func(status int, value any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(value) }
	if r.URL.Path == path {
		switch r.Method {
		case http.MethodGet:
			pulls := []PullRequest{}
			for _, pull := range g.pulls {
				head := r.URL.Query().Get("head")
				state := r.URL.Query().Get("state")
				if (head == "" || head == pull.Head.Ref || head == strings.Split(g.repository, "/")[0]+":"+pull.Head.Ref) &&
					(state == "" || state == "all" || state == pull.State) {
					pulls = append(pulls, pull)
				}
			}
			write(http.StatusOK, pulls)
			return
		case http.MethodPost:
			var input struct{ Title, Body, Head, Base string }
			if json.Unmarshal(body, &input) != nil || input.Head == "" || input.Base == "" || input.Title == "" {
				http.Error(w, `{"message":"Invalid pull request"}`, http.StatusUnprocessableEntity)
				return
			}
			for _, pull := range g.pulls {
				if pull.Head.Ref == input.Head && pull.State == "open" {
					http.Error(w, `{"message":"Pull request already exists"}`, http.StatusUnprocessableEntity)
					return
				}
			}
			number := len(g.pulls) + 1
			pull := PullRequest{Number: number, URL: g.Server.URL + "/" + g.repository + "/pull/" + strconv.Itoa(number),
				State: "open", Title: input.Title, Body: input.Body}
			pull.Head.Ref, pull.Base.Ref = input.Head, input.Base
			g.pulls = append(g.pulls, pull)
			write(http.StatusCreated, pull)
			return
		}
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, path+"/") {
		number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, path+"/"))
		if err == nil && number > 0 && number <= len(g.pulls) {
			write(http.StatusOK, g.pulls[number-1])
			return
		}
	}
	http.Error(w, `{"message":"Unsupported fake GitHub operation"}`, http.StatusNotFound)
}
