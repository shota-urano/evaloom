package store

import (
	"context"
	"database/sql"

	"github.com/shota-urano/evaloom/backend/internal/model"
)

const harnessProfileColumns = `id, harness_id, version, commit_sha, tag_name,
	definition, definition_hash, model_id, operation_status, registered_at`

func scanHarnessProfile(row interface{ Scan(...any) error }) (model.HarnessProfile, error) {
	var p model.HarnessProfile
	var definition string
	err := row.Scan(&p.ID, &p.HarnessID, &p.Version, &p.CommitSHA, &p.TagName,
		&definition, &p.DefinitionHash, &p.ModelID, &p.OperationStatus, &p.RegisteredAt)
	p.Definition = []byte(definition)
	return p, err
}

func GetHarnessProfile(ctx context.Context, reader projectReader, harnessID, version string) (model.HarnessProfile, error) {
	return scanHarnessProfile(reader.QueryRowContext(ctx, `SELECT `+harnessProfileColumns+`
		FROM harness_profiles WHERE harness_id = ? AND version = ?`, harnessID, version))
}

func GetHarnessProfileByID(ctx context.Context, reader projectReader, id string) (model.HarnessProfile, error) {
	return scanHarnessProfile(reader.QueryRowContext(ctx, `SELECT `+harnessProfileColumns+`
		FROM harness_profiles WHERE id = ?`, id))
}

func ListHarnessProfiles(ctx context.Context, db *sql.DB, harnessID, after string, limit int) ([]model.HarnessProfile, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+harnessProfileColumns+`
		FROM harness_profiles WHERE id > ? AND (? = '' OR harness_id = ?) ORDER BY id LIMIT ?`, after, harnessID, harnessID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := []model.HarnessProfile{}
	for rows.Next() {
		p, err := scanHarnessProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// InsertHarnessProfile never updates registered versions. The caller checks
// existing versions and records the audit log in the same transaction.
func InsertHarnessProfile(ctx context.Context, tx *sql.Tx, p model.HarnessProfile) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO harness_profiles (`+harnessProfileColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, p.ID, p.HarnessID, p.Version, p.CommitSHA,
		p.TagName, string(p.Definition), p.DefinitionHash, p.ModelID, p.OperationStatus, p.RegisteredAt)
	return err
}
