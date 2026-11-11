package contract

import (
	"fmt"

	"github.com/oklog/ulid/v2"
)

// AuthorizeRequest is the HTTP payload in spec 17 section 5. The channel's
// op discriminator is handled separately by the stage relay.
type AuthorizeRequest struct {
	Operation  string  `json:"operation" required:"true"`
	ApprovalID *string `json:"approvalId" required:"true" nullable:"true"`
	ContentID  string  `json:"contentId" required:"true"`
}

type AuthorizeResponse struct {
	Allowed bool   `json:"allowed" required:"true"`
	Reason  string `json:"reason,omitempty"`
}

func (r AuthorizeRequest) Validate() error {
	if !operationPattern.MatchString(r.Operation) {
		return fmt.Errorf("operation: expected name of up to 64 characters")
	}
	if r.ApprovalID != nil {
		id, err := ulid.ParseStrict(*r.ApprovalID)
		if err != nil || id.String() != *r.ApprovalID {
			return fmt.Errorf("approvalId: expected ULID or null")
		}
	}
	return contentID(r.ContentID)
}

func (r *AuthorizeRequest) UnmarshalJSON(data []byte) error {
	type plain AuthorizeRequest
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	if err := AuthorizeRequest(value).Validate(); err != nil {
		return err
	}
	*r = AuthorizeRequest(value)
	return nil
}

func (r *AuthorizeResponse) UnmarshalJSON(data []byte) error {
	type plain AuthorizeResponse
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	if value.Allowed {
		if value.Reason != "" {
			return fmt.Errorf("reason: unexpected for allowed response")
		}
	} else {
		switch value.Reason {
		case "cancelled", "content_changed", "not_approved", "already_used", "expired", "unavailable":
		default:
			return fmt.Errorf("reason: expected denial reason")
		}
	}
	*r = AuthorizeResponse(value)
	return nil
}
