package stage

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"unicode"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/sandboxproxy"
)

// ErrTooManyTaskLinks is the config_error reason of spec 09 3.3 step 3.
var ErrTooManyTaskLinks = errors.New("タスクのリンクが 50 件を超えている")

// DeclaresTaskLinks reports stages.prepare.network.taskLinks: true (contract 5.2).
func DeclaresTaskLinks(profile contract.ProfileDefinition) bool {
	p := profile.Stages.Prepare
	return p != nil && p.Network != nil && p.Network.TaskLinks != nil && *p.Network.TaskLinks
}

// TaskLinks extracts the links of spec 09 3.3 steps 1-2 from the task title,
// body and acceptance criteria, in that order. modelAPIs is the Q146 list of
// host:port destinations; a link whose host appears there is dropped. The
// result is never nil. A list longer than contract.MaxTaskLinks is returned
// as is; the caller fails the stage (step 3).
func TaskLinks(modelAPIs []string, texts ...string) []string {
	modelHosts := map[string]bool{}
	for _, destination := range modelAPIs {
		if host, _, err := net.SplitHostPort(destination); err == nil {
			modelHosts[host] = true
		}
	}
	links := []string{}
	seen := map[string]bool{}
	for _, text := range texts {
		for {
			start := strings.Index(text, "https://")
			if start < 0 {
				break
			}
			text = text[start:]
			end := strings.IndexFunc(text, func(r rune) bool {
				return unicode.IsSpace(r) || strings.ContainsRune(`<>"')]`, r)
			})
			if end < 0 {
				end = len(text)
			}
			link := strings.TrimRight(text[:end], ".,。、")
			text = text[end:]
			if seen[link] {
				continue
			}
			if _, ok := TaskLinkDestination(link, modelHosts); ok {
				seen[link] = true
				links = append(links, link)
			}
		}
	}
	return links
}

// TaskLinkDestination returns the host:443 destination of an accepted link.
// Rejected (spec 09 3.3 step 2): IP literals, local names (the network.allow
// rule of 3.3), ports other than 443, user information, and model API hosts.
func TaskLinkDestination(link string, modelHosts map[string]bool) (string, bool) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return "", false
	}
	host := u.Hostname()
	if u.Host != host && u.Host != host+":443" {
		return "", false
	}
	destination := host + ":443"
	if contract.DestinationIssue(destination) != "" || modelHosts[host] {
		return "", false
	}
	// The proxy's own rule (e.g. home.arpa) must not fail the whole stage.
	if _, err := sandboxproxy.Destination(destination); err != nil {
		return "", false
	}
	return destination, true
}

// TaskLinkDestinations returns the distinct host:443 destinations of links
// already accepted by TaskLinks, in order of first appearance.
func TaskLinkDestinations(links []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, link := range links {
		destination, ok := TaskLinkDestination(link, nil)
		if ok && !seen[destination] {
			seen[destination] = true
			out = append(out, destination)
		}
	}
	return out
}
