// Package secretstore owns Runner-local special-harness credentials. It has
// no Control Plane, database or logging dependencies.
package secretstore

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/sys/unix"
)

var harnessPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
var slotPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

type Store struct{ dataDir string }

// New does not create directories or read credentials.
func New(dataDir string) *Store { return &Store{dataDir: dataDir} }

// Validate checks identifiers without reflecting untrusted input in errors.
func Validate(harnessID, slot, access string) error {
	if !harnessPattern.MatchString(harnessID) || !slotPattern.MatchString(slot) {
		return fmt.Errorf("secretstore: invalid harness ID or slot")
	}
	if access != "read" && access != "write" {
		return fmt.Errorf("secretstore: access must be read or write")
	}
	return nil
}

// Set atomically replaces one 0600 file containing an access line followed by
// the exact input bytes. A failed write leaves the previous key intact. Neither
// input values nor filesystem errors (which can echo paths) are returned.
func (s *Store) Set(harnessID, slot, access string, value []byte) error {
	if err := Validate(harnessID, slot, access); err != nil {
		return err
	}
	if len(value) == 0 || bytes.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("secretstore: value must be nonempty and contain no NUL")
	}
	dir, err := s.directory(harnessID, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Inspect an existing destination before replacement. Never follow links or
	// replace a directory, shared inode or a file with unsafe permissions.
	old, err := openKey(dir, slot)
	if err == nil {
		old.Close()
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("secretstore: unsafe existing key file")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("secretstore: cannot create key file")
	}
	tmp := ".set-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(dir.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("secretstore: cannot create key file")
	}
	defer unix.Unlinkat(int(dir.Fd()), tmp, 0)
	file := os.NewFile(uintptr(fd), "key")
	defer file.Close()
	if file.Chmod(0600) != nil {
		return fmt.Errorf("secretstore: cannot secure key file")
	}
	if _, err := io.WriteString(file, access+"\n"); err != nil {
		return fmt.Errorf("secretstore: cannot write key file")
	}
	if n, err := file.Write(value); err != nil || n != len(value) {
		return fmt.Errorf("secretstore: cannot write key file")
	}
	if file.Sync() != nil || file.Close() != nil {
		return fmt.Errorf("secretstore: cannot save key file")
	}
	if unix.Renameat(int(dir.Fd()), tmp, int(dir.Fd()), slot) != nil {
		return fmt.Errorf("secretstore: cannot replace key file")
	}
	return nil
}

func (s *Store) load(harnessID, slot string) (string, []byte, error) {
	dir, err := s.directory(harnessID, false)
	if err != nil {
		return "", nil, err
	}
	defer dir.Close()
	file, err := openKey(dir, slot)
	if err != nil {
		return "", nil, fmt.Errorf("secretstore: key is missing or unsafe")
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	defer clear(content)
	if err != nil {
		return "", nil, fmt.Errorf("secretstore: cannot read key file")
	}
	access, value, ok := bytes.Cut(content, []byte("\n"))
	if !ok || (string(access) != "read" && string(access) != "write") || len(value) == 0 || bytes.IndexByte(value, 0) >= 0 {
		return "", nil, fmt.Errorf("secretstore: invalid key file")
	}
	return string(access), bytes.Clone(value), nil
}

// Directory descriptors and openat with NOFOLLOW keep every store component
// anchored to the opened data directory, including during concurrent rotation.
// The Runner targets macOS and Linux, like the existing sandbox implementation.
func (s *Store) directory(harnessID string, create bool) (*os.File, error) {
	if !filepath.IsAbs(s.dataDir) || filepath.Clean(s.dataDir) != s.dataDir {
		return nil, fmt.Errorf("secretstore: data directory must be an absolute clean path")
	}
	if create {
		if os.MkdirAll(s.dataDir, 0700) != nil {
			return nil, fmt.Errorf("secretstore: cannot create data directory")
		}
	}
	fd, err := unix.Open(s.dataDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("secretstore: cannot open data directory")
	}
	dir := os.NewFile(uintptr(fd), "data")
	for _, name := range []string{"secrets", harnessID} {
		if create {
			if err := unix.Mkdirat(int(dir.Fd()), name, 0700); err != nil && err != unix.EEXIST {
				dir.Close()
				return nil, fmt.Errorf("secretstore: cannot create key directory")
			}
		}
		next, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		dir.Close()
		if err != nil {
			return nil, fmt.Errorf("secretstore: key directory is missing or unsafe")
		}
		dir = os.NewFile(uintptr(next), "keys")
		var stat unix.Stat_t
		if unix.Fstat(next, &stat) != nil || stat.Mode&07777 != 0700 || stat.Uid != uint32(os.Geteuid()) {
			dir.Close()
			return nil, fmt.Errorf("secretstore: key directory must be owned by Runner with mode 0700")
		}
	}
	return dir, nil
}

func openKey(dir *os.File, slot string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), slot, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "key")
	var stat unix.Stat_t
	// A concurrently rotated file may already be unlinked (Nlink == 0), but
	// this descriptor still owns the complete previous record. Multiple links
	// could expose the value outside this store and are rejected.
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&07777 != 0600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink > 1 {
		file.Close()
		return nil, fmt.Errorf("secretstore: key file must be a private 0600 regular file")
	}
	return file, nil
}
