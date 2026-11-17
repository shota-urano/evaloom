package contract

import (
	"fmt"
	"unicode/utf8"
)

// ValidateWorkQuestion checks contract 8 and spec 08 Q144. File type and size
// are checked by Runner before decoding; the Control Plane checks the shape
// again before opening a decision.
func ValidateWorkQuestion(raw []byte) error {
	var question struct {
		Question string   `json:"question" required:"true"`
		Options  []string `json:"options"`
	}
	if err := strictDecode(raw, &question); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(question.Question); n < 1 || n > 4000 || len(question.Options) > 10 {
		return fmt.Errorf("question: exceeds contract limits")
	}
	return nil
}
