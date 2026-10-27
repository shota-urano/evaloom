package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Artifact describes a file whose StoragePath is relative to the configured
// data directory. IDs are supplied by the caller, as for other store records.
type Artifact struct {
	ID, ExecutionID, Kind string
	StoragePath, SHA256   string
	SizeBytes             int64
	Redacted              bool
	CreatedAt             string
}

// ArtifactStore stores bytes separately from SQLite, under dataDir/artifacts.
// The Control Plane owns it. Call Close when it is no longer needed.
type ArtifactStore struct {
	db   *sql.DB
	root *os.Root
}

// NewArtifactStore opens an explicitly configured data directory. The caller
// must keep it outside Git tracking; the repository's /data/ is ignored for
// local use. A dedicated directory outside the repository also works.
func NewArtifactStore(db *sql.DB, dataDir string) (*ArtifactStore, error) {
	if db == nil || strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("store: database and artifact data directory are required")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, err
	}
	if err := root.MkdirAll("artifacts", 0700); err != nil {
		root.Close()
		return nil, err
	}
	return &ArtifactStore{db: db, root: root}, nil
}

func (s *ArtifactStore) Close() error { return s.root.Close() }

// Save writes already-redacted bytes, computes metadata from those exact bytes,
// and inserts the artifact only after the complete file is synced and closed.
// On a write or database failure it removes only its newly created file.
// Redacted records whether the caller applied redaction; Save does not do so.
func (s *ArtifactStore) Save(ctx context.Context, id, executionID, kind string, redacted bool, content io.Reader) (_ Artifact, err error) {
	return s.save(ctx, s.db, id, executionID, kind, redacted, content)
}

// SaveTx keeps the caller's ownership check and registration in one transaction.
// As with a process crash, a failed outer commit can leave an unregistered file.
func (s *ArtifactStore) SaveTx(ctx context.Context, tx *sql.Tx, id, executionID, kind string, redacted bool, content io.Reader) (Artifact, error) {
	return s.save(ctx, tx, id, executionID, kind, redacted, content)
}

func (s *ArtifactStore) save(ctx context.Context, writer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, id, executionID, kind string, redacted bool, content io.Reader) (_ Artifact, err error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if id == "" || executionID == "" || content == nil {
		return Artifact{}, fmt.Errorf("store: artifact ID, execution ID and content are required")
	}
	// A generated filename avoids interpreting record IDs as filesystem paths.
	path := filepath.Join("artifacts", rand.Text())
	file, err := s.root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Artifact{}, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.root.Remove(path))
		}
	}()
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hash), content)
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if err = errors.Join(copyErr, file.Close()); err != nil {
		return Artifact{}, err
	}
	artifact := Artifact{
		ID: id, ExecutionID: executionID, Kind: kind, StoragePath: path,
		SHA256: hex.EncodeToString(hash.Sum(nil)), SizeBytes: size,
		Redacted: redacted, CreatedAt: recordTime(),
	}
	_, err = writer.ExecContext(ctx, `INSERT INTO artifacts
		(id, execution_id, kind, storage_path, sha256, size_bytes, redacted, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, artifact.ID, artifact.ExecutionID, artifact.Kind,
		artifact.StoragePath, artifact.SHA256, artifact.SizeBytes, artifact.Redacted, artifact.CreatedAt)
	if err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

// Open reads metadata and opens its file within the configured data directory.
// The execution ID scopes the lookup. The caller owns closing the returned file.
func (s *ArtifactStore) Open(ctx context.Context, executionID, id string) (Artifact, *os.File, error) {
	var artifact Artifact
	err := s.db.QueryRowContext(ctx, `SELECT id, execution_id, kind, storage_path,
		sha256, size_bytes, redacted, created_at FROM artifacts WHERE execution_id = ? AND id = ?`,
		executionID, id).Scan(&artifact.ID, &artifact.ExecutionID, &artifact.Kind, &artifact.StoragePath,
		&artifact.SHA256, &artifact.SizeBytes, &artifact.Redacted, &artifact.CreatedAt)
	if err != nil {
		return Artifact{}, nil, err
	}
	file, err := s.root.Open(artifact.StoragePath)
	if err != nil {
		return Artifact{}, nil, err
	}
	return artifact, file, nil
}
