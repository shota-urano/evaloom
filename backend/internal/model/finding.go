package model

// Finding is the Gate finding in spec 02, section 5.5.
type Finding struct {
	Rule    string `json:"rule"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}
