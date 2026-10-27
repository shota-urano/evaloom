package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Log collects one Execution stream. Read may be called concurrently for stdout
// and stderr. Deliver runs separately, so network failures never block Read.
// Call Finish after all readers stop, and keep Deliver alive until it returns.
// Input must already have passed the Runner's redaction policy. Redaction itself
// belongs to the next implementation unit in spec 05, section 9.
type Log struct {
	runner                                    *Runner
	id, executionID, stream, path             string
	mu                                        sync.Mutex
	file                                      *os.File
	size                                      int64
	finished, delivering, delivered, redacted bool
	writeErr                                  error
	retryInterval                             time.Duration
}

func (runner *Runner) NewLog(executionID, stream string) (*Log, error) {
	if id, err := ulid.ParseStrict(executionID); err != nil || id.String() != executionID {
		return nil, fmt.Errorf("runner: invalid log execution ID")
	}
	if stream != "harness" && stream != "setup" && stream != "test" {
		return nil, fmt.Errorf("runner: invalid artifact log stream")
	}
	dir := filepath.Join(runner.dataDir, "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	id := ulid.Make().String()
	path := filepath.Join(dir, id+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	return &Log{runner: runner, id: id, executionID: executionID, stream: stream,
		path: path, file: file, retryInterval: time.Second}, nil
}

// Path is the Runner-local log, retained even after successful delivery.
func (log *Log) Path() string { return log.path }

// Read has no Scanner token limit. It records a final unterminated line too,
// normalizing LF/CRLF and the final line to one LF in the persisted log.
// A read failure preserves any partial output before returning the error.
func (log *Log) Read(source io.Reader) error {
	reader := bufio.NewReader(source)
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if err := log.appendLine(text); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("runner: read log output failed")
		}
	}
}

func (log *Log) appendLine(text string) error {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.finished {
		return fmt.Errorf("runner: log is finished")
	}
	if log.writeErr != nil {
		return log.writeErr
	}
	n, err := io.WriteString(log.file, text+"\n")
	if err == nil && n != len(text)+1 {
		err = io.ErrShortWrite
	}
	if err != nil {
		log.writeErr = err
		return err
	}
	log.size += int64(n)
	return nil
}

// Finish closes collection independently of delivery. Pass true when the
// upstream redactor has processed this stream; never store unredacted secrets.
// Delivery can continue with its own context after an Execution is cancelled.
func (log *Log) Finish(redacted bool) error {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.finished {
		return log.writeErr
	}
	log.finished, log.redacted = true, redacted
	log.writeErr = errors.Join(log.writeErr, log.file.Sync(), log.file.Close())
	return log.writeErr
}

type logEvent struct {
	ID     string `json:"id"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// Deliver tails the file in bounded batches and retries failed HTTP requests
// every second, including after Finish. Cancellation stops only delivery, leaves
// the file intact, and permits another Deliver call on this Log to replay it.
// A new Runner boot does not recover logs automatically (spec 05, section 3.1).
func (log *Log) Deliver(ctx context.Context) error {
	log.mu.Lock()
	if log.delivering {
		log.mu.Unlock()
		return fmt.Errorf("runner: log delivery already running")
	}
	if log.delivered {
		log.mu.Unlock()
		return nil
	}
	log.delivering = true
	log.mu.Unlock()
	defer func() { log.mu.Lock(); log.delivering = false; log.mu.Unlock() }()
	file, err := os.Open(log.path)
	if err != nil {
		return err
	}
	defer file.Close()
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		log.mu.Lock()
		size, finished, redacted, writeErr := log.size, log.finished, log.redacted, log.writeErr
		log.mu.Unlock()
		if offset < size {
			reader := bufio.NewReader(io.NewSectionReader(file, offset, size-offset))
			events := []logEvent{}
			next := offset
			for len(events) < 100 && next-offset < 256<<10 {
				line, err := reader.ReadString('\n')
				if err == io.EOF && line == "" {
					break
				}
				if err != nil {
					return fmt.Errorf("runner: read saved log failed")
				}
				events = append(events, logEvent{fmt.Sprintf("%s:%d", log.id, next), log.stream, strings.TrimSuffix(line, "\n")})
				next += int64(len(line))
			}
			body, err := json.Marshal(struct {
				Events []logEvent `json:"events"`
			}{events})
			if err != nil {
				return err
			}
			if log.post(ctx, "events", bytes.NewReader(body), "application/json") == nil {
				offset = next
				continue
			}
		} else if finished {
			if writeErr != nil {
				return writeErr
			}
			// Use a fresh descriptor per attempt. The HTTP transport owns its
			// request body and may still be closing it when Do returns an error.
			artifact, err := os.Open(log.path)
			if err != nil {
				return err
			}
			endpoint := "artifacts/" + log.id + "?stream=" + log.stream + "&redacted=" + fmt.Sprint(redacted)
			err = log.post(ctx, endpoint, artifact, "text/plain; charset=utf-8")
			artifact.Close()
			if err == nil {
				log.mu.Lock()
				log.delivered = true
				log.mu.Unlock()
				return nil
			}
		}
		timer := time.NewTimer(log.retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (log *Log) post(ctx context.Context, suffix string, body io.Reader, contentType string) error {
	endpoint, err := url.JoinPath(log.runner.controlPlaneURL, "api", "executions", log.executionID)
	if err != nil {
		return fmt.Errorf("runner: invalid log URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/"+suffix, body)
	if err != nil {
		return fmt.Errorf("runner: create log request failed")
	}
	request.Header.Set("Authorization", "Bearer "+log.runner.token)
	request.Header.Set("Content-Type", contentType)
	response, err := log.runner.client.Do(request)
	if err != nil {
		return fmt.Errorf("runner: log transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("runner: log returned HTTP %d", response.StatusCode)
	}
	return nil
}
