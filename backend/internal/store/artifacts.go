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
	"unicode/utf8"
)

// Artifact describes a file whose StoragePath is relative to the configured
// data directory. IDs are supplied by the caller, as for other store records.
type Artifact struct {
	ID, ExecutionID, RouteProposalID string
	Stage, Kind, Name                string
	StoragePath, SHA256              string
	SizeBytes                        int64
	Redacted                         bool
	CreatedAt                        string
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
func (s *ArtifactStore) Save(ctx context.Context, artifact Artifact, content io.Reader) (_ Artifact, err error) {
	artifact, cleanup, err := s.write(ctx, artifact, content)
	if err != nil {
		return Artifact{}, err
	}
	if err := insertArtifact(ctx, s.db, artifact); err != nil {
		return Artifact{}, errors.Join(err, cleanup())
	}
	return artifact, nil
}

var ErrArtifactConflict = errors.New("artifact ID already used for another Execution or content")

// RegisterTx requires the caller to check Execution or proposal ownership in tx
// before calling it. The returned cleanup must run if tx is rolled back or commit
// fails. Duplicate uploads are removed here, leaving the original untouched.
func (s *ArtifactStore) RegisterTx(ctx context.Context, tx *sql.Tx, artifact Artifact, content io.Reader) (Artifact, func() error, error) {
	artifact, cleanup, err := s.write(ctx, artifact, content)
	if err != nil {
		return Artifact{}, nil, err
	}
	var previous Artifact
	err = tx.QueryRowContext(ctx, `SELECT id, coalesce(execution_id, ''), coalesce(route_proposal_id, ''), stage, kind, name, storage_path, sha256, size_bytes, redacted, created_at
		FROM artifacts WHERE id = ?`, artifact.ID).Scan(&previous.ID, &previous.ExecutionID, &previous.RouteProposalID, &previous.Stage, &previous.Kind, &previous.Name,
		&previous.StoragePath, &previous.SHA256, &previous.SizeBytes, &previous.Redacted, &previous.CreatedAt)
	if err == nil {
		if previous.ExecutionID != artifact.ExecutionID || previous.RouteProposalID != artifact.RouteProposalID || previous.SHA256 != artifact.SHA256 {
			err = ErrArtifactConflict
		}
		return previous, func() error { return nil }, errors.Join(err, cleanup())
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = insertArtifact(ctx, tx, artifact)
	}
	if err != nil {
		return Artifact{}, nil, errors.Join(err, cleanup())
	}
	return artifact, cleanup, nil
}

func insertArtifact(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, artifact Artifact) error {
	_, err := db.ExecContext(ctx, `INSERT INTO artifacts
		(id, execution_id, route_proposal_id, stage, kind, name, storage_path, sha256, size_bytes, redacted, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, artifact.ID, nullableArtifactID(artifact.ExecutionID), nullableArtifactID(artifact.RouteProposalID), artifact.Stage, artifact.Kind, artifact.Name,
		artifact.StoragePath, artifact.SHA256, artifact.SizeBytes, artifact.Redacted, artifact.CreatedAt)
	return err
}

func (s *ArtifactStore) write(ctx context.Context, artifact Artifact, content io.Reader) (_ Artifact, cleanup func() error, err error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, nil, err
	}
	if artifact.ID == "" || (artifact.ExecutionID == "") == (artifact.RouteProposalID == "") || artifact.Stage == "" || artifact.Name == "" || content == nil {
		return Artifact{}, nil, fmt.Errorf("store: artifact ID, exactly one owner, stage, name and content are required")
	}
	// A generated filename avoids interpreting record IDs as filesystem paths.
	path := filepath.Join("artifacts", rand.Text())
	file, err := s.root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Artifact{}, nil, err
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
		return Artifact{}, nil, err
	}
	artifact.StoragePath = path
	artifact.SHA256 = hex.EncodeToString(hash.Sum(nil))
	artifact.SizeBytes = size
	artifact.CreatedAt = recordTime()
	return artifact, func() error { return s.root.Remove(path) }, nil
}

// Open reads metadata and opens its file within the configured data directory.
// The execution ID scopes the lookup. The caller owns closing the returned file.
func (s *ArtifactStore) Open(ctx context.Context, executionID, id string) (Artifact, *os.File, error) {
	var artifact Artifact
	err := s.db.QueryRowContext(ctx, `SELECT id, execution_id, stage, kind, name, storage_path,
		sha256, size_bytes, redacted, created_at FROM artifacts WHERE execution_id = ? AND id = ?`,
		executionID, id).Scan(&artifact.ID, &artifact.ExecutionID, &artifact.Stage, &artifact.Kind, &artifact.Name, &artifact.StoragePath,
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

// HarnessSummaryTx reads out/summary.md from the work_summary artifact inside tx
// (linearoutbox.SummarySource). Several or no artifacts, an unreadable file or
// invalid UTF-8 make the summary unavailable rather than failing tx.
func (s *ArtifactStore) HarnessSummaryTx(ctx context.Context, tx *sql.Tx, executionID string) (string, bool) {
	var path sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN count(*)=1 THEN min(storage_path) END
		FROM artifacts WHERE execution_id=? AND kind='work_summary'`, executionID).Scan(&path); err != nil || !path.Valid {
		return "", false
	}
	file, err := s.root.Open(path.String)
	if err != nil {
		return "", false
	}
	defer file.Close()
	body, err := io.ReadAll(file)
	if err != nil || !utf8.Valid(body) {
		return "", false
	}
	return string(body), true
}

func nullableArtifactID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
