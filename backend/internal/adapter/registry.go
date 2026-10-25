package adapter

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// ErrNotRegistered identifies a lookup for an unknown adapter name.
var ErrNotRegistered = errors.New("adapter not registered")

// Registry maps harness names to implementations without depending on concrete
// adapter packages. Its zero value is ready for use. A Registry must not be copied
// after first use. Registry operations are safe for concurrent callers; adapters
// are responsible for synchronizing their own session state.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// Register binds an exact, case-sensitive name to an implementation. Names are
// not restricted to the initial harnesses, so new adapter packages need only a
// registration call. Duplicate registration never replaces an existing adapter.
func (r *Registry) Register(name string, implementation Adapter) error {
	if name == "" || strings.TrimSpace(name) != name {
		return fmt.Errorf("invalid adapter name %q", name)
	}
	if isNilAdapter(implementation) {
		return fmt.Errorf("adapter %q: nil implementation", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[name]; exists {
		return fmt.Errorf("adapter %q already registered", name)
	}
	if r.adapters == nil {
		r.adapters = make(map[string]Adapter)
	}
	r.adapters[name] = implementation
	return nil
}

// Lookup returns the registered implementation. Missing names wrap
// ErrNotRegistered so callers can distinguish configuration errors with errors.Is.
func (r *Registry) Lookup(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	implementation, exists := r.adapters[name]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrNotRegistered, name)
	}
	return implementation, nil
}

// Names returns a sorted snapshot for Harness registration validation.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func isNilAdapter(implementation Adapter) bool {
	if implementation == nil {
		return true
	}
	switch value := reflect.ValueOf(implementation); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
