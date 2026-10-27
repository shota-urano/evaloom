package gitcontext

import (
	"bytes"
	"encoding/json"
)

// []byte uses JSON's base64 encoding, preserving even invalid UTF-8 blob bytes.
// Keep the in-memory Content string for existing readers and adapters.
type fileJSON struct {
	Path            string `json:"path"`
	Content         []byte `json:"content"`
	ContentHash     string `json:"contentHash"`
	SourceCommitSHA string `json:"sourceCommitSha"`
}

func (f File) MarshalJSON() ([]byte, error) {
	return json.Marshal(fileJSON{f.Path, []byte(f.Content), f.ContentHash, f.SourceCommitSHA})
}

func (f *File) UnmarshalJSON(data []byte) error {
	var wire fileJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	// A custom decoder must retain the Runner API's strict field validation.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*f = File{Path: wire.Path, Content: string(wire.Content), ContentHash: wire.ContentHash, SourceCommitSHA: wire.SourceCommitSHA}
	return nil
}
