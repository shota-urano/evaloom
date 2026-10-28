package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRequiresPairedArtifactConfiguration(t *testing.T) {
	for _, flags := range [][]string{
		{"-data-dir", t.TempDir()},
		{"-artifact-max-bytes", "1024"},
		{"-data-dir", t.TempDir(), "-artifact-max-bytes", "-1"},
		{"-data-dir", t.TempDir(), "-artifact-max-bytes", "0"},
	} {
		args := append([]string{"-db", filepath.Join(t.TempDir(), "evaloom.db")}, flags...)
		err := run(context.Background(), args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "must be supplied together") {
			t.Fatalf("run(%v)=%v; want artifact configuration error", flags, err)
		}
	}
}
