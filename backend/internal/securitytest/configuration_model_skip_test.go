//go:build !security_model

// Spec 14 §3.1 uses real CLIs and dedicated model credentials on a Mac mini.
// From backend, follow README.md §3.1, then run make security-model-test.
package securitytest

import "testing"

func TestModelConfigurationIsolation(t *testing.T) {
	t.Skip("Codex / Claude Codeの実CLIによる設定分離テストは専用モデル認証情報とDockerを要するため、通常検証ではスキップ。Mac miniでREADME.md §3.1の準備後、make security-model-testを実行する（security_modelタグで有効化）")
}
