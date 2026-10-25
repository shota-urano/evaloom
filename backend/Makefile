# harness-kit: templates/go@d61c58c-dirty (deployed 2026-10-25)
# ==== harness-kit: verify契約（Go） ====
# エージェントの検証入口は verify / verify-fast の2つだけ。
# 最終行に「VERIFY: PASS」が出ない限り、完了と見なさない（Default-FAIL）。
#
# bootstrap が確定する項目: RUN_CMD（cmd/ 配下の実在する main パッケージ）/
# FAST_CHECKS・CHECKS（lint は staticcheck / golangci-lint の導入有無で確定し、
# 無ければ変数から外す。ターゲット定義は消さない）。
# 検証項目がゼロのときは PASS を出さず FAIL する（空PASSの禁止）。

GO := go

# 軽い検査（Stop hook 用）。fmt=gofmt、vet=go vet、test=go test
FAST_CHECKS := fmt vet test
# フル検証 = FAST_CHECKS + build。lint（staticcheck / golangci-lint）導入時は FAST_CHECKS に足す
CHECKS := $(FAST_CHECKS) build

.PHONY: verify verify-fast fmt vet lint test build run

verify: $(CHECKS)
	@test -n "$(strip $(CHECKS))" || { echo "VERIFY: FAIL (検証項目がゼロ)"; exit 1; }
	@echo "VERIFY: PASS"

verify-fast: $(FAST_CHECKS)
	@test -n "$(strip $(FAST_CHECKS))" || { echo "VERIFY-FAST: FAIL (検証項目がゼロ)"; exit 1; }
	@echo "VERIFY-FAST: PASS"

# フォーマット検査（書き換えない）。gofmt -l は差分があっても exit 0 なので、出力の有無で判定する
fmt:
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*')); \
	 if [ -n "$$out" ]; then echo "gofmt 未適用:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

# 導入済みの方に bootstrap が書き換える（例: golangci-lint run ./...）
lint:
	staticcheck ./...

test:
	$(GO) test ./...

build:
	$(GO) build ./...

# ==== 人間が「動いているところ」を見るための入口 ====
# 起動コマンドの正本。AGENTS.md には「起動は make run」とだけ書けばよい。
# bootstrap が cmd/ 配下の実在する main パッケージに確定する。
RUN_CMD := ./cmd/evaloomd
run:
	$(GO) run $(RUN_CMD)

# artifacts（動画回収）は対象外（画面が無い）。UI を持つ場合は web テンプレの artifacts を移植する。
