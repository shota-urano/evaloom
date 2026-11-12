package logredact

import (
	"bytes"
	"testing"
)

func TestArtifactBytesPreserveUnrelatedContent(t *testing.T) {
	for _, test := range []struct {
		name        string
		input, want []byte
		secrets     []string
	}{
		{"binary and CRLF", []byte("\x00\xff\r\nkeep\r\n"), []byte("\x00\xff\r\nkeep\r\n"), []string{"secret"}},
		{"multiline CRLF value", []byte("before=first\r\nsecond\r\nafter"), []byte("before=[REDACTED]\r\nafter"), []string{"first\r\nsecond"}},
		{"exact byte match", []byte("first\nsecond"), []byte("first\nsecond"), []string{"first\r\nsecond"}},
		{"longest value", []byte("abcdef abc"), []byte("[REDACTED] [REDACTED]"), []string{"abc", "abcdef"}},
		{"authorization", []byte("Authorization: Bearer private\r\nkeep\r\n"), []byte("Authorization: [REDACTED]\r\nkeep\r\n"), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Bytes(test.input, test.secrets); !bytes.Equal(got, test.want) {
				t.Fatalf("got=%q want=%q", got, test.want)
			}
		})
	}
}
