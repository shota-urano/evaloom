package contract

import (
	"encoding/json"
	"fmt"
)

// ChannelRequest is the line protocol of contract 7.4. Authorization is kept
// separate from op so the HTTP relay can only send the existing API payload.
type ChannelRequest struct {
	Op            string
	Authorization AuthorizeRequest
}

func (r *ChannelRequest) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "$")
	if err != nil {
		return err
	}
	var op string
	if raw, ok := fields["op"]; !ok || json.Unmarshal(raw, &op) != nil || op == "" {
		return fmt.Errorf("op: expected operation")
	}
	delete(fields, "op")
	value := ChannelRequest{Op: op}
	switch op {
	case "cancel_check":
		if len(fields) != 0 {
			return fmt.Errorf("cancel_check: unexpected fields")
		}
	case "authorize":
		payload, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(payload, &value.Authorization); err != nil {
			return err
		}
	default:
		return fmt.Errorf("op: unsupported operation")
	}
	*r = value
	return nil
}
