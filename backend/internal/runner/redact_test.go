package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Only this test adapter connects the registrar boundary to the existing store.
// It does not define a Runner artifact HTTP endpoint or give Runner a DB handle.
type storedLogRegistrar struct {
	recordingArtifactRegistrar
	files *store.ArtifactStore
}

func (r *storedLogRegistrar) RegisterLogArtifact(ctx context.Context, artifact LogArtifact) error {
	if err := r.recordingArtifactRegistrar.RegisterLogArtifact(ctx, artifact); err != nil {
		return err
	}
	file, err := os.Open(artifact.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = r.files.Save(ctx, artifact.ID, artifact.ExecutionID, artifact.Kind, artifact.Redacted, file)
	return err
}

func TestLogRedactionBeforePersistenceEventsAndArtifactRegistration(t *testing.T) {
	for _, stream := range []string{"harness", "setup", "test"} {
		t.Run(stream, func(t *testing.T) {
			f := newLogControlPlane(t)
			process, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			files, err := store.NewArtifactStore(f.db, f.dataDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { files.Close() })
			registrar := &storedLogRegistrar{files: files}
			secrets := []string{"github-credential", "Model-API-secret", "first-line\nsecond-line", "shared", "shared-long", "", "github-credential"}
			log, err := process.NewLog(logExecution, stream, registrar, secrets...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { log.Finish(false) })
			// Copying the credential list prevents later caller mutations from
			// changing an in-flight log's redaction policy.
			secrets[0] = "changed-after-NewLog"
			input := "token=" + process.token + "suffix\r\n" +
				"github-credential/github-credential Model-API-secret\n" +
				"before first-line\nsecond-line after\n" +
				"shared-long shared\n" +
				"github-cred model-api-secret\n" + // Partial/case-changed values are not exact matches.
				"trace aUtHoRiZaTiOn: Bearer unknown-header-value\r\n" +
				"Proxy-Authorization:\tBasic unknown-proxy-value\n" +
				"key=-----BEGIN RSA PRIVATE KEY-----\nprivate-body-one\nprivate-body-two\n-----END RSA PRIVATE KEY----- done\n" +
				"-----BEGIN OPENSSH PRIVATE KEY-----inline-key-----END OPENSSH PRIVATE KEY----- tail\n" +
				"last " + process.token
			want := "token=[REDACTED]suffix\n" +
				"[REDACTED]/[REDACTED] [REDACTED]\n" +
				"before [REDACTED] after\n" +
				"[REDACTED] [REDACTED]\n" +
				"github-cred model-api-secret\n" +
				"trace aUtHoRiZaTiOn: [REDACTED]\n" +
				"Proxy-Authorization: [REDACTED]\n" +
				"key=[REDACTED]\n\n\n done\n" +
				"[REDACTED] tail\n" +
				"last [REDACTED]\n"
			// Fragment every value, newline and delimiter across source reads.
			if err := log.Read(iotest.OneByteReader(strings.NewReader(input))); err != nil {
				t.Fatal(err)
			}
			// Check before Finish and before any delivery: no raw spool exists.
			content, err := os.ReadFile(log.Path())
			if err != nil || string(content) != want {
				t.Fatalf("saved log differs from redacted output: err=%v", err)
			}
			if err := log.Finish(false); err != nil {
				t.Fatal(err)
			}
			f.loseEventAck.Store(true) // Retries must contain only redacted bytes too.
			if err := await(t, startLogDelivery(t, log)); err != nil {
				t.Fatal(err)
			}
			if registrar.calls != 1 || !registrar.artifact.Redacted || registrar.content != want {
				t.Fatal("registrar must receive redacted=true and only sanitized bytes")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, reader := f.stream(ctx)
			wantLines := strings.Split(strings.TrimSuffix(want, "\n"), "\n")
			for _, line := range wantLines {
				readLogFrame(t, reader, stream, line)
			}
			rows, err := f.db.Query(`SELECT payload FROM execution_events WHERE type='log' ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			var payloads []string
			for rows.Next() {
				var payload string
				if err := rows.Scan(&payload); err != nil {
					t.Fatal(err)
				}
				payloads = append(payloads, payload)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if len(payloads) != len(wantLines) {
				t.Fatal("redacted events must remain idempotent after a lost acknowledgement")
			}
			for _, secret := range []string{process.token, "github-credential", "Model-API-secret", "first-line", "second-line", "unknown-header-value", "unknown-proxy-value", "private-body-one", "private-body-two", "inline-key"} {
				if strings.Contains(strings.Join(payloads, "\n"), secret) {
					t.Fatal("secret persisted in events")
				}
			}
			var redacted int
			if err := f.db.QueryRow(`SELECT redacted FROM artifacts WHERE id=?`, log.id).Scan(&redacted); err != nil || redacted != 1 {
				t.Fatalf("artifacts.redacted=%d err=%v", redacted, err)
			}
			_, file, err := files.Open(ctx, logExecution, log.id)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			saved, err := io.ReadAll(file)
			if err != nil || string(saved) != want {
				t.Fatal("artifact store did not preserve sanitized bytes")
			}
		})
	}
}

func TestRedactionStreamBoundariesAndReadErrors(t *testing.T) {
	readFailure := errors.New("source failure with secret data")
	for _, test := range []struct {
		name, input, want string
		secrets           []string
		fail              bool
		changed           bool
	}{
		{"empty secret", "ordinary\n", "ordinary\n", []string{""}, false, false},
		{"partial and case sensitive", "secre Secret\n", "secre Secret\n", []string{"secret"}, false, false},
		{"multiline CRLF exact", "x first\r\nsecond y\r\n", "x [REDACTED] y\n", []string{"first\r\nsecond"}, false, true},
		{"newline normalization cannot recreate a secret", "first\r\nsecond\n", "[REDACTED]\n", []string{"first\nsecond"}, false, true},
		{"longest match at EOF", "abcdef abc", "[REDACTED] [REDACTED]", []string{"abc", "abcdef"}, false, true},
		{"unmatched prefix EOF", "abcde", "abcde", []string{"abcdef"}, false, false},
		{"unicode and adjacent", "秘密秘密", "[REDACTED][REDACTED]", []string{"秘密"}, false, true},
		{"unrelated UTF8 and binary bytes", "日本語\x00\xff 秘密 終わり", "日本語\x00\xff [REDACTED] 終わり", []string{"秘密"}, false, true},
		{"long value and refilled lookahead", "prefix " + strings.Repeat("a", 8192) + "z tail", "prefix [REDACTED] tail", []string{strings.Repeat("a", 8192) + "z"}, false, true},
		{"unterminated encrypted PEM", "-----BEGIN ENCRYPTED PRIVATE KEY-----\nbody\nlast", "[REDACTED]\n\n", nil, false, true},
		{"error in PEM", "-----BEGIN PRIVATE KEY-----\nbody", "[REDACTED]\n", nil, true, true},
		{"literal at read error", "x secret", "x [REDACTED]", []string{"secret"}, true, true},
		{"header before PEM", "Authorization: -----BEGIN PRIVATE KEY-----\nbody\n-----END PRIVATE KEY-----\nafter\n", "Authorization: [REDACTED]\n\n\nafter\n", nil, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var source io.Reader = iotest.OneByteReader(strings.NewReader(test.input))
			if test.fail {
				source = io.MultiReader(source, iotest.ErrReader(readFailure))
			}
			patterns := &patternReader{source: bufio.NewReader(source)}
			literals := newSecretReader(patterns, test.secrets)
			got, err := io.ReadAll(literals)
			if string(got) != test.want || (err != nil) != test.fail || (test.fail && !errors.Is(err, readFailure)) || (patterns.changed || literals.changed) != test.changed {
				t.Fatalf("output=%q err=%v changed=%v; want=%q fail=%v changed=%v", got, err, patterns.changed || literals.changed, test.want, test.fail, test.changed)
			}
		})
	}
}

func TestConcurrentLogReadersKeepIndependentPEMState(t *testing.T) {
	process, err := New(testConfig(t, "http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := process.NewLog(logExecution, "test", &recordingArtifactRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Finish(false)
	var group sync.WaitGroup
	for _, input := range []string{"-----BEGIN PRIVATE KEY-----\nkey-body-without-end", "other output\n"} {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := log.Read(strings.NewReader(input)); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines) != 2 || (!reflect.DeepEqual(lines, []string{"[REDACTED]", "other output"}) && !reflect.DeepEqual(lines, []string{"other output", "[REDACTED]"})) || !log.redacted {
		t.Fatal("PEM state crossed concurrent readers, or redaction metadata was lost")
	}
}

func TestRedactedLogReachesSSEBeforeProducerCloses(t *testing.T) {
	f := newLogControlPlane(t)
	process, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	registrar := &recordingArtifactRegistrar{}
	log, err := process.NewLog(logExecution, "harness", registrar)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Finish(false) })
	done := startLogDelivery(t, log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, events := f.stream(ctx)
	stdout, producer := io.Pipe()
	defer stdout.Close()
	defer producer.Close()
	readDone := make(chan error, 1)
	go func() { readDone <- log.Read(stdout) }()
	if _, err := io.WriteString(producer, "token="+process.token+"\n"); err != nil {
		t.Fatal(err)
	}
	readLogFrame(t, events, "harness", "token=[REDACTED]")
	content, err := os.ReadFile(log.Path())
	if err != nil || string(content) != "token=[REDACTED]\n" {
		t.Fatal("live log must already be sanitized")
	}
	// A source error can itself contain credentials. Preserve sanitized partial
	// output and return only the collector's fixed error text.
	producer.CloseWithError(errors.New(process.token))
	if err := await(t, readDone); err == nil || err.Error() != "runner: read log output failed" {
		t.Fatal("log read error exposed source details or was lost")
	}
	if err := log.Finish(false); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
	if !registrar.artifact.Redacted {
		t.Fatal("redaction metadata was lost on read error")
	}
}
