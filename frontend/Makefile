# harness-kit: templates/web@d61c58c-dirty (deployed 2026-10-25)
# ==== harness-kit: verify契約（Web） ====
# エージェントの検証入口は verify / verify-fast の2つだけ。
# 最終行に「VERIFY: PASS」が出ない限り、完了と見なさない（Default-FAIL）。
#
# project-bootstrap は package.json の scripts と照合し、存在する検査だけを
# FAST_CHECKS / CHECKS に残す（ターゲット定義は消さない）。
# 検証項目がゼロのときは PASS を出さず FAIL する（空PASSの禁止）。

PM := npm

# build を含まない検査（Stop hook 用）。bootstrap が実在する scripts に合わせて編集する
FAST_CHECKS := typecheck lint test
# フル検証 = FAST_CHECKS + build + e2e（e2e は Playwright 導入済みプロジェクトのみ。
# 無ければ bootstrap が CHECKS から外し要確認リストへ）
CHECKS := $(FAST_CHECKS) build

.PHONY: verify verify-fast typecheck lint test build e2e run artifacts preview-url

verify: $(CHECKS)
	@test -n "$(strip $(CHECKS))" || { echo "VERIFY: FAIL (検証項目がゼロ)"; exit 1; }
	@echo "VERIFY: PASS"

verify-fast: $(FAST_CHECKS)
	@test -n "$(strip $(FAST_CHECKS))" || { echo "VERIFY-FAST: FAIL (検証項目がゼロ)"; exit 1; }
	@echo "VERIFY-FAST: PASS"

typecheck:
	@# Next.js の .next/types/*.ts は build 時に生成され、ブランチ切替後もそのまま残るため、
	@# 別ブランチにしか無いページを参照したまま stale 化して TS2307 で偽 FAIL する
	@# （dev-loop が検証ブランチで統合 verify → 別ブランチへ戻ると必ず再現。2026-07-26 実測）。
	@# Next.js 以外のスタックでは何もしない（存在しないディレクトリの rm -rf は無害）。
	@rm -rf .next/types
	$(PM) run typecheck

lint:
	$(PM) run lint

test:
	$(PM) run test

build:
	$(PM) run build

e2e:
	$(PM) run e2e

# ==== 人間が「動いているところ」を見るための2つの入口 ====
# 起動コマンドの正本をここに置く。AGENTS.md には「起動は make run」とだけ書けばよくなり、
# エージェントに毎回ポートや起動方法を説明するラリーが消える（2026-07-27 追加）。
run:
	$(PM) run dev

# E2E の録画・スクショを docs/loop-artifacts/ へ回収する（dev-loop の integrate-close が呼ぶ）。
# TASK_ID があればファイル名に前置してタスクと動画を対応づける。
# 動画は webm で出るので、Slack がインライン再生できる mp4 へ変換する（ffmpeg が無ければ webm のまま）。
# モノレポではルートの docs/loop-artifacts へ集める（ルートが ARTIFACT_DIR を渡す）。
# `:=` だと web/docs/loop-artifacts に散って夜勤ビューアが拾えない——`?=` を保つこと。
ARTIFACT_DIR ?= docs/loop-artifacts
artifacts:
	@mkdir -p $(ARTIFACT_DIR)
	@prefix="$${TASK_ID:+$$TASK_ID-}"; \
	 n=0; \
	 for f in $$(find test-results -name '*.webm' 2>/dev/null); do \
	   base=$$(basename "$$(dirname "$$f")"); \
	   if command -v ffmpeg >/dev/null 2>&1; then \
	     ffmpeg -y -loglevel error -i "$$f" -c:v libx264 -pix_fmt yuv420p -movflags +faststart \
	       "$(ARTIFACT_DIR)/$${prefix}$${base}.mp4" && n=$$((n+1)); \
	   else \
	     cp "$$f" "$(ARTIFACT_DIR)/$${prefix}$${base}.webm" && n=$$((n+1)); \
	   fi; \
	 done; \
	 for f in $$(find test-results -name '*.png' 2>/dev/null); do \
	   cp "$$f" "$(ARTIFACT_DIR)/$${prefix}$$(basename "$$f")"; \
	 done; \
	 echo "ARTIFACTS: $$n 本の動画を $(ARTIFACT_DIR)/ へ回収"

# ==== preview-url: 「この PJ をスマホで確認する URL」を1行で出す契約（2026-07-27 追加） ====
# dev-run の start-preview.sh が `make preview-url` → `make run` の順で叩き、
# 得た URL を朝の確認ページと報告へ載せる。中身はプラットフォームで変わってよいが
# **入口の名前は常に preview-url / run の2つ**（dev-run は中身を知らないまま扱える）:
#   web          … http://<Tailscale IP>:<port>/
#   React Native … exp://<Tailscale IP>:8081（Expo Go で開く）
#   iOS          … **リンクでは確認できない**（シミュレータは Mac 内・実機は署名配布が要る）。
#                   preview-url を定義しない＝録画での確認に倒す、が正しい振る舞い
#   Android      … APK を配って実機に入れる。URL を出すなら APK の配布先
#
# tailnet 内のどの端末からも同じ URL で開けるように Tailscale IP を使う。
# ローカル hostname（*.local）は tailnet 外から引けず、MagicDNS 名は端末側の DNS 設定に依存する
# ——IP が一番確実（iPhone の Safari にそのまま打てる）。無ければ hostname に落とす。
TS_CLI := /Applications/Tailscale.app/Contents/MacOS/Tailscale
TS_IP   = $(shell $(TS_CLI) ip -4 2>/dev/null | head -1 || hostname -s)
PREVIEW_PORT ?= 5173

preview-url:
	@echo "http://$(TS_IP):$(PREVIEW_PORT)/"
