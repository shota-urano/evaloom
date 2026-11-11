package secretstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"golang.org/x/sys/unix"
)

func TestSetRotationAndStageSelection(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	read := []byte(" read-key\nsecond line\r\n")
	write := []byte("publish-key")
	for _, key := range []struct {
		slot, access string
		value        []byte
	}{
		{"source", "read", read}, {"destination", "write", write},
		{"undeclared", "read", []byte("never selected")},
	} {
		if err := s.Set("software-engineer", key.slot, key.access, key.value); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "secrets", "software-engineer", key.slot)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe key permissions: %v", err)
		}
		content, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(content, append([]byte(key.access+"\n"), key.value...)) {
			t.Fatal("stored value/access changed")
		}
	}
	prepare, err := s.ForStage("software-engineer", model.StagePrepare, []contract.Secret{{Slot: "source", Access: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepare.Environment(), map[string]string{"EVALOOM_SECRET_SOURCE": string(read)}) {
		t.Fatal("prepare selected the wrong keys")
	}
	prepare.Environment()["EVALOOM_SECRET_SOURCE"] = "caller mutation"
	if prepare.Environment()["EVALOOM_SECRET_SOURCE"] != string(read) {
		t.Fatal("caller changed stored snapshot")
	}
	publish, err := s.ForStage("software-engineer", model.StagePublish, []contract.Secret{{Slot: "source", Access: "read"}, {Slot: "destination", Access: "write"}})
	if err != nil || len(publish.Environment()) != 2 || publish.Environment()["EVALOOM_SECRET_DESTINATION"] != string(write) {
		t.Fatal("publish selected wrong values")
	}
	if len(publish.RedactionValues()) != 2 {
		t.Fatal("missing redaction values")
	}
	encoded, err := json.Marshal(publish)
	if err != nil || string(encoded) != "{}" || strings.Contains(fmt.Sprintf("%v %+v %#v", publish, publish, publish), string(write)) {
		t.Fatal("accidental serialization exposed keys")
	}
	if err := s.Set("software-engineer", "source", "write", []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	assertConfigError(t, s, "software-engineer", model.StagePrepare, []contract.Secret{{Slot: "source", Access: "read"}})
	rotated, err := s.ForStage("software-engineer", model.StagePublish, []contract.Secret{{Slot: "source", Access: "write"}})
	if err != nil || rotated.Environment()["EVALOOM_SECRET_SOURCE"] != "rotated" {
		t.Fatal("rotation was not visible")
	}
	if prepare.Environment()["EVALOOM_SECRET_SOURCE"] != string(read) {
		t.Fatal("rotation changed an active stage snapshot")
	}
	files, err := os.ReadDir(filepath.Join(dir, "secrets", "software-engineer"))
	if err != nil || len(files) != 3 {
		t.Fatal("rotation left extra files")
	}
	if err := s.Set("another-harness", "source", "read", []byte("separate")); err != nil {
		t.Fatal(err)
	}
	other, err := s.ForStage("another-harness", model.StagePrepare, []contract.Secret{{Slot: "source", Access: "read"}})
	if err != nil || other.Environment()["EVALOOM_SECRET_SOURCE"] != "separate" {
		t.Fatal("harness namespaces are not isolated")
	}
}

func assertConfigError(t *testing.T, s *Store, harness string, stage model.Stage, declarations []contract.Secret) {
	t.Helper()
	resolved, err := s.ForStage(harness, stage, declarations)
	var config *ConfigError
	if !errors.As(err, &config) || config.Status() != model.ExecutionFailed || config.ErrorClass() != model.ErrorConfig || len(resolved.Environment()) != 0 {
		t.Fatalf("want config_error without partial keys, got %v", err)
	}
}

func TestStageKeysFailClosed(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Set("test", "key", "write", []byte("private-value")); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, harness string
		stage         model.Stage
		keys          []contract.Secret
	}{
		{"registered write", "test", model.StagePrepare, []contract.Secret{{Slot: "key", Access: "read"}}},
		{"declared write", "test", model.StagePrepare, []contract.Secret{{Slot: "key", Access: "write"}}},
		{"missing after valid", "test", model.StagePublish, []contract.Secret{{Slot: "key", Access: "write"}, {Slot: "missing", Access: "read"}}},
		{"duplicate", "test", model.StagePublish, []contract.Secret{{Slot: "key", Access: "read"}, {Slot: "key", Access: "write"}}},
		{"another harness", "other", model.StagePublish, []contract.Secret{{Slot: "key", Access: "read"}}},
		{"bad access", "test", model.StagePublish, []contract.Secret{{Slot: "key", Access: "invalid"}}},
		{"slot traversal", "test", model.StagePublish, []contract.Secret{{Slot: "../key", Access: "read"}}},
		{"harness traversal", "../test", model.StagePublish, nil},
		{"unknown stage", "test", "unknown", nil},
	} {
		t.Run(tt.name, func(t *testing.T) { assertConfigError(t, s, tt.harness, tt.stage, tt.keys) })
	}
	for _, stage := range []model.Stage{model.StageWork, model.StageCheck, "proposal"} {
		assertConfigError(t, s, "test", stage, []contract.Secret{{Slot: "key", Access: "read"}})
		resolved, err := New(filepath.Join(t.TempDir(), "missing")).ForStage("test", stage, nil)
		if err != nil || len(resolved.Environment()) != 0 {
			t.Fatal("keyless stage read the store")
		}
	}
	assertConfigError(t, New(filepath.Join(t.TempDir(), "missing")), "test", model.StagePublish, []contract.Secret{{Slot: "key", Access: "read"}})
}

func TestStoreRejectsUnsafeFilesWithoutFollowingOrOverwritingThem(t *testing.T) {
	for _, kind := range []string{"symlink-secrets", "symlink-harness", "symlink-key", "data-symlink", "data-symlink-trailing-slash", "hardlink", "public-file", "public-directory", "fifo", "directory", "invalid-access", "NUL", "empty"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(root, "data")
			s := New(data)
			if err := s.Set("test", "key", "read", []byte("original")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(data, "secrets", "test", "key")
			outside := filepath.Join(root, "outside")
			if err := os.WriteFile(outside, []byte("read\nprivate-outside"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink-secrets", "symlink-harness", "data-symlink", "data-symlink-trailing-slash":
				link := filepath.Dir(filepath.Dir(path))
				if kind == "symlink-harness" {
					link = filepath.Dir(path)
				}
				if kind == "data-symlink" || kind == "data-symlink-trailing-slash" {
					link = data
				}
				if err := os.Rename(link, link+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(link+"-real", link); err != nil {
					t.Fatal(err)
				}
				if kind == "data-symlink-trailing-slash" {
					s = New(data + "/")
				}
			case "symlink-key", "hardlink", "fifo", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "symlink-key":
					err = os.Symlink(outside, path)
				case "hardlink":
					err = os.Link(outside, path)
				case "fifo":
					err = unix.Mkfifo(path, 0600)
				case "directory":
					err = os.Mkdir(path, 0700)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "public-file":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "public-directory":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			default:
				content := map[string]string{"invalid-access": "admin\nprivate-outside", "NUL": "read\nprivate\x00outside", "empty": "read\n"}[kind]
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			assertConfigError(t, s, "test", model.StagePublish, []contract.Secret{{Slot: "key", Access: "read"}})
			// Corrupt-but-private regular files can be repaired by CLI rotation.
			if kind != "invalid-access" && kind != "NUL" && kind != "empty" {
				if err := s.Set("test", "key", "write", []byte("replacement")); err == nil {
					t.Fatal("unsafe destination replaced")
				}
			}
			content, err := os.ReadFile(outside)
			if err != nil || string(content) != "read\nprivate-outside" {
				t.Fatal("outside file changed")
			}
		})
	}
}

func TestInvalidSetPreservesPreviousKey(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Set("test", "key", "read", []byte("original")); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		harness, slot, access string
		value                 []byte
	}{
		{"../outside", "key", "read", []byte("private")},
		{"test", "../outside", "read", []byte("private")},
		{"test", "Uppercase", "read", []byte("private")},
		{"test", strings.Repeat("k", 33), "read", []byte("private")},
		{"test", "key", "private-invalid-access", []byte("private")},
		{"test", "key", "write", nil},
		{"test", "key", "write", []byte("private\x00key")},
	} {
		if err := s.Set(tt.harness, tt.slot, tt.access, tt.value); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid input accepted or echoed")
		}
	}
	resolved, err := s.ForStage("test", model.StagePrepare, []contract.Secret{{Slot: "key", Access: "read"}})
	if err != nil || resolved.Environment()["EVALOOM_SECRET_KEY"] != "original" {
		t.Fatal("invalid rotation changed existing key")
	}
}

func TestConcurrentRotationNeverExposesPartialRecord(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Set("test", "key", "read", []byte("first")); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := range 4 {
		workers.Go(func() {
			for range 20 {
				if i%2 == 0 {
					if err := s.Set("test", "key", "read", []byte("next")); err != nil {
						t.Error(err)
					}
				} else {
					resolved, err := s.ForStage("test", model.StagePrepare, []contract.Secret{{Slot: "key", Access: "read"}})
					if err != nil {
						t.Error(err)
						continue
					}
					if value := resolved.Environment()["EVALOOM_SECRET_KEY"]; value != "first" && value != "next" {
						t.Error("partial key observed")
					}
				}
			}
		})
	}
	workers.Wait()
}
