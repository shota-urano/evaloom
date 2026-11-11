package runner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/secretstore"
)

func TestStageKeyPreflightPreventsStart(t *testing.T) {
	runner := &Runner{dataDir: filepath.Join(t.TempDir(), "data")}
	keys := secretstore.New(runner.dataDir)
	if err := keys.Set("test", "key", "write", []byte("write-value")); err != nil {
		t.Fatal(err)
	}
	started := 0
	start := func(ctx context.Context, resolved secretstore.Resolved) error {
		started++
		if resolved.Environment()["EVALOOM_SECRET_KEY"] != "write-value" {
			t.Error("stage did not receive selected key")
		}
		return nil
	}
	for _, tt := range []struct {
		stage model.Stage
		keys  []contract.Secret
	}{
		{model.StagePrepare, []contract.Secret{{Slot: "key", Access: "read"}}},
		{model.StagePublish, []contract.Secret{{Slot: "key", Access: "write"}, {Slot: "missing", Access: "read"}}},
		{model.StageWork, []contract.Secret{{Slot: "key", Access: "read"}}},
	} {
		err := runner.WithStageSecrets(context.Background(), "test", tt.stage, tt.keys, start)
		var config *secretstore.ConfigError
		if !errors.As(err, &config) || config.ErrorClass() != model.ErrorConfig || started != 0 {
			t.Fatalf("stage started or missing config_error: %v", err)
		}
	}
	if err := runner.WithStageSecrets(context.Background(), "test", model.StagePublish, []contract.Secret{{Slot: "key", Access: "write"}}, start); err != nil || started != 1 {
		t.Fatalf("valid stage did not start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runner.WithStageSecrets(ctx, "test", model.StagePublish, nil, start); !errors.Is(err, context.Canceled) || started != 1 {
		t.Fatal("cancelled stage started")
	}
}
