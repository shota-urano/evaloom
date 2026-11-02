package api

import (
	"database/sql"
	"net/http"
	"reflect"

	"github.com/shota-urano/evaloom/backend/internal/store"
)

// Acknowledge the start SHA before stage 4 pins Project Context. Repeating the
// same measurement is safe after a lost acknowledgement; changing it is not.
// This uses only the existing result endpoint and executions.base_sha column.
func saveBaseResult(r *http.Request, tx *sql.Tx, execution store.ContextExecution, input resultInput) error {
	if input.FromStatus != "preparing" || input.Status != "preparing" {
		return &Error{Code: Conflict, Message: "Start SHA must be reported during preparation"}
	}
	expected := resultInput{FromStatus: input.FromStatus, Status: input.Status, BaseSHA: input.BaseSHA}
	if !reflect.DeepEqual(input, expected) {
		return invalid("baseSha", "measurement_only")
	}
	if err := validateResult(r, tx, execution, input); err != nil {
		return err
	}
	return saveResult(r, tx, execution, input)
}
