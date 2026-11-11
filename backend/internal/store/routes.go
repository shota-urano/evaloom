package store

import (
	"context"
	"database/sql"
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type Route struct {
	Label     string  `json:"label"`
	ProjectID *string `json:"projectId"`
	HarnessID *string `json:"harnessId"`
	CreatedAt string  `json:"createdAt"`
	UpdatedAt string  `json:"updatedAt"`
}

const routeColumns = `label, project_id, harness_id, created_at, updated_at`

func scanRoute(row rowScanner) (Route, error) {
	var route Route
	err := row.Scan(&route.Label, &route.ProjectID, &route.HarnessID, &route.CreatedAt, &route.UpdatedAt)
	return route, err
}

func GetRoute(ctx context.Context, reader projectReader, label string) (Route, error) {
	return scanRoute(reader.QueryRowContext(ctx, `SELECT `+routeColumns+` FROM routes WHERE label=?`, label))
}

func ListRoutes(ctx context.Context, db *sql.DB, after string, limit int) ([]Route, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+routeColumns+` FROM routes WHERE label>? ORDER BY label LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Route{}
	for rows.Next() {
		route, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, route)
	}
	return out, rows.Err()
}

var ErrRouteConflict = errors.New("route label or destination already registered")

func SaveRoute(ctx context.Context, tx *sql.Tx, route Route, create bool) error {
	statement := `UPDATE routes SET project_id=?, harness_id=?, created_at=?, updated_at=? WHERE label=?`
	if create {
		statement = `INSERT INTO routes (project_id,harness_id,created_at,updated_at,label) VALUES (?,?,?,?,?)`
	}
	_, err := tx.ExecContext(ctx, statement, route.ProjectID, route.HarnessID, route.CreatedAt, route.UpdatedAt, route.Label)
	var constraint *sqlite.Error
	if errors.As(err, &constraint) && (constraint.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE) {
		return ErrRouteConflict
	}
	return err
}
