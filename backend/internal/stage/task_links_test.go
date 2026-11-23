package stage

import (
	"reflect"
	"strings"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
)

var testModelAPIs = []string{"api.openai.com:443", "chatgpt.com:443", "api.anthropic.com:443"}

// spec 09 3.3 "タスクのリンク" steps 1-2.
func TestTaskLinksExtractionRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		texts []string
		want  []string
	}{
		{"title, body, criteria order without duplicates",
			[]string{"件名 https://a.example.com/1", "本文 https://b.example.com/2 https://a.example.com/1", "条件 https://c.example.com/3 https://b.example.com/2"},
			[]string{"https://a.example.com/1", "https://b.example.com/2", "https://c.example.com/3"}},
		{"terminators", []string{"x https://s.example.com/a b\nhttps://n.example.com/b\n<https://lt.example.com/c> \"https://dq.example.com/d\" 'https://sq.example.com/e' (https://p.example.com/f) [https://br.example.com/g] https://ws.example.com/h　後"},
			[]string{"https://s.example.com/a", "https://n.example.com/b", "https://lt.example.com/c", "https://dq.example.com/d", "https://sq.example.com/e", "https://p.example.com/f", "https://br.example.com/g", "https://ws.example.com/h"}},
		{"trailing punctuation", []string{"https://a.example.com/x. https://b.example.com/y, https://c.example.com/z。https://d.example.com/w、 https://e.example.com/v.,。、"},
			// 。 is not a terminator: the third link runs to the next space.
			[]string{"https://a.example.com/x", "https://b.example.com/y", "https://c.example.com/z。https://d.example.com/w", "https://e.example.com/v"}},
		{"query, fragment and explicit 443 are kept as written", []string{"https://q.example.com/p?a=1&b=2#frag https://port.example.com:443/ok"},
			[]string{"https://q.example.com/p?a=1&b=2#frag", "https://port.example.com:443/ok"}},
		{"excluded links", []string{strings.Join([]string{
			"http://plain.example.com/", "https://192.0.2.1/", "https://[2001:db8::1]/", "https://localhost/", "https://intranet/",
			"https://svc.local/", "https://build.internal/", "https://router.home.arpa/", "https://port.example.com:8443/", "https://port.example.com:80/",
			"https://user@cred.example.com/", "https://u:p@cred.example.com/", "https://api.openai.com/v1", "https://chatgpt.com/c/1",
			"https://api.anthropic.com/v1/messages", "https://", "HTTPS://upper.example.com/", "https://*.example.com/",
		}, " ")}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := TaskLinks(testModelAPIs, tc.texts...)
			if got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("TaskLinks=%#v; want %#v", got, tc.want)
			}
		})
	}
}

func TestTaskLinkDestinationsArePort443PerHost(t *testing.T) {
	got := TaskLinkDestinations([]string{"https://a.example.com/1", "https://b.example.com:443/2", "https://a.example.com/3"})
	if want := []string{"a.example.com:443", "b.example.com:443"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("destinations=%v; want %v", got, want)
	}
}

func TestDeclaresTaskLinks(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		prepare *contract.ProgramStage
		want    bool
	}{
		{nil, false},
		{&contract.ProgramStage{}, false},
		{&contract.ProgramStage{Network: &contract.Network{Allow: []string{"github.com:443"}}}, false},
		{&contract.ProgramStage{Network: &contract.Network{TaskLinks: &no}}, false},
		{&contract.ProgramStage{Network: &contract.Network{TaskLinks: &yes}}, true},
	} {
		profile := contract.ProfileDefinition{Stages: contract.ProfileStages{Prepare: tc.prepare}}
		if got := DeclaresTaskLinks(profile); got != tc.want {
			t.Fatalf("DeclaresTaskLinks(%+v)=%v; want %v", tc.prepare, got, tc.want)
		}
	}
}
