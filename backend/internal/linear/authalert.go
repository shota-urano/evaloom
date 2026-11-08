package linear

import "sync"

// AuthAlertGate is the one suppression of the authentication error
// notification shared by the Linear Poller and the Linear comment Worker
// (spec 16 3.4.3, spec 12 3.5, spec 15 6章, Q84, Q86). Both authenticate
// with the same API key, so an invalid key alerts the owner once, from
// whichever side detected it first.
//
// The state lives in the process only: after a restart the next
// authentication error alerts again (spec 16 3.4.3). The zero value is an
// unsuppressed gate and is safe for concurrent use.
type AuthAlertGate struct {
	mu   sync.Mutex
	held bool
}

// Acquire reports whether the caller should send the authentication error
// notification. It checks and sets the suppression in one step, so of any
// number of concurrent callers exactly one gets true until Release.
func (g *AuthAlertGate) Acquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held {
		return false
	}
	g.held = true
	return true
}

// Release lifts the suppression. Call it after a query (any Team) or a
// comment send succeeded: the key is valid again. A failure Linear returned
// after authenticating (a removed Issue, for example) is not a success and
// must not call it.
func (g *AuthAlertGate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = false
}
