package harnessenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sys/unix"
)

const AuthenticationLimit = 64 * 1024

// Authentication is a per-execution lease on one harness's Runner-owned store.
// The stable sidecar lock is never removed or renamed with the credential.
// All diagnostics are fixed text: neither paths nor credential bytes escape.
type Authentication struct {
	root       *os.Root
	lock       *os.File
	name       string
	initial    []byte
	logMu      sync.Mutex
	logSecrets []string
	logBlocked bool
}

func authError(reason string) error { return fmt.Errorf("authentication: %s", reason) }

// AcquireAuthentication locks before opening the credential, including across
// Runner processes. The caller supplies the same absolute store path for every
// execution of the same harness. Close only after stopped writers and writeback.
func AcquireAuthentication(ctx context.Context, store string) (_ *Authentication, err error) {
	if !filepath.IsAbs(store) {
		return nil, configError(authError("explicit store path required"))
	}
	root, err := os.OpenRoot(filepath.Dir(store))
	if err != nil {
		return nil, configError(authError("store unavailable"))
	}
	a := &Authentication{root: root, name: filepath.Base(store)}
	defer func() {
		if err != nil {
			a.Close()
		}
	}()
	a.lock, err = root.OpenFile(a.name+".lock", os.O_CREATE|os.O_EXCL|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if errors.Is(err, os.ErrExist) {
		a.lock, err = root.OpenFile(a.name+".lock", os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, configError(authError("lock unavailable"))
	}
	info, statErr := a.lock.Stat()
	named, nameErr := root.Lstat(a.name + ".lock")
	if statErr != nil || nameErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !named.Mode().IsRegular() || !os.SameFile(info, named) {
		return nil, configError(authError("invalid lock file"))
	}
	for {
		if err = ctx.Err(); err != nil {
			return nil, configError(err)
		}
		err = unix.Flock(int(a.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return nil, configError(authError("lock failed"))
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, configError(ctx.Err())
		case <-timer.C:
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, configError(err)
	}
	a.initial, err = readAuthentication(root, a.name, true)
	if err != nil {
		return nil, configError(err)
	}
	a.logSecrets = authenticationSecrets(a.initial)
	return a, nil
}

// ValidateAuthentication never includes a JSON parser's untrusted error text.
func ValidateAuthentication(data []byte) error {
	if len(data) > AuthenticationLimit {
		return authError("file exceeds 64 KiB")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return authError("file is not a JSON object")
	}
	return nil
}

func readAuthentication(root *os.Root, name string, requirePrivate bool) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, authError("file unavailable")
	}
	if !info.Mode().IsRegular() {
		return nil, authError("file is not regular")
	}
	if requirePrivate && info.Mode().Perm() != 0600 {
		return nil, authError("file permissions must be 0600")
	}
	if info.Size() > AuthenticationLimit {
		return nil, authError("file exceeds 64 KiB")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, authError("file unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, authError("file replaced")
	}
	data, err := io.ReadAll(io.LimitReader(f, AuthenticationLimit+1))
	if err != nil {
		return nil, authError("file read failed")
	}
	if err = ValidateAuthentication(data); err != nil {
		return nil, err
	}
	return data, nil
}

func authenticationName(variable string) string {
	if variable == "CODEX_HOME" {
		return "auth.json"
	}
	return ".credentials.json"
}

// Install adds credentials only to a disposable copy, outside Files/context
// revisions. Install before the launch-boundary Files check. The lease outlives
// both the Sandbox volume and this Environment.
func (a *Authentication) Install(e *Environment) error {
	if a.root == nil || e.root == nil || !e.working || e.authentication != nil {
		return configError(authError("working copy and active lease required"))
	}
	name := authenticationName(e.variable)
	f, err := e.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return configError(authError("copy placement failed"))
	}
	_, err = f.Write(a.initial)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return configError(authError("copy placement failed"))
	}
	e.authentication = a
	return nil
}

// AuthenticationFile is deliberately separate from the pinned Files interface.
// Its contents may only be transferred to the private Sandbox configuration.
func (e *Environment) AuthenticationFile() (string, []byte, error) {
	if e.authentication == nil {
		return "", nil, nil
	}
	if err := e.checkIdentity(); err != nil {
		return "", nil, authError("copy directory replaced")
	}
	name := authenticationName(e.variable)
	data, err := readAuthentication(e.root, name, false)
	return name, data, err
}

// RefreshAuthentication records values obtained after a CLI command has stopped,
// without persisting them. A failed read permanently suppresses later CLI logs.
// A nil input reads the local copy, for launchers that use that copy directly.
func (e *Environment) RefreshAuthentication(data []byte) error {
	a := e.authentication
	if a == nil {
		return nil
	}
	if data == nil {
		_, local, err := e.AuthenticationFile()
		if err != nil {
			e.SuppressAuthenticationLogs()
			return err
		}
		data = local
	}
	if err := ValidateAuthentication(data); err != nil {
		e.SuppressAuthenticationLogs()
		return err
	}
	secrets := authenticationSecrets(data)
	a.logMu.Lock()
	a.logSecrets = append(a.logSecrets, secrets...)
	a.logMu.Unlock()
	return nil
}

// SuppressAuthenticationLogs fails closed when Sandbox recovery cannot provide
// valid credentials. Later successful reads must not re-enable CLI output.
func (e *Environment) SuppressAuthenticationLogs() {
	if a := e.authentication; a != nil {
		a.logMu.Lock()
		a.logBlocked = true
		a.logMu.Unlock()
	}
}

// WriteBackAuthentication validates bytes obtained from the stopped Sandbox.
// A nil input reads the local copy, for launchers that use that copy directly.
// Call even after cancellation, before Close; errors must only produce warnings.
func (e *Environment) WriteBackAuthentication(data []byte) error {
	a := e.authentication
	if a == nil {
		return nil
	}
	if data == nil {
		_, local, err := e.AuthenticationFile()
		if err != nil {
			e.SuppressAuthenticationLogs()
			return err
		}
		data = local
	}
	if err := e.RefreshAuthentication(data); err != nil {
		return err
	}
	if bytes.Equal(data, a.initial) {
		return nil
	}
	if a.root == nil {
		return authError("lease closed")
	}
	return replaceAuthentication(a.root, a.name, data, func(f *os.File, data []byte) error {
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		return f.Sync()
	})
}

// The persist callback is a private fault boundary for partial-write/fsync tests.
func replaceAuthentication(root *os.Root, name string, data []byte, persist func(*os.File, []byte) error) error {
	temp := ".evaloom-auth-" + ulid.Make().String()
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return authError("temporary file creation failed")
	}
	defer root.Remove(temp)
	err = persist(f, data)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return authError("temporary file write or sync failed")
	}
	if err := root.Rename(temp, name); err != nil {
		return authError("atomic replacement failed")
	}
	return nil
}

func (a *Authentication) Close() error {
	if a.root == nil {
		return nil
	}
	var err error
	if a.lock != nil {
		err = a.lock.Close() // closing releases flock, including on an early return
		a.lock = nil
	}
	err = errors.Join(err, a.root.Close())
	a.root, a.initial = nil, nil
	a.logMu.Lock()
	a.logSecrets = nil
	a.logBlocked = true
	a.logMu.Unlock()
	if err != nil {
		return authError("lease close failed")
	}
	return nil
}
