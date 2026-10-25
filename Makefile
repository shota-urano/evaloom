# harness-kit: templates/monorepo@d61c58c (deployed 2026-10-25)
# ==== harness-kit: verify契約（モノレポ ルート集約） ====
# エージェントの検証入口は、モノレポでも常に「ルートの verify / verify-fast」の2つだけ。
# ルートは各パッケージの verify へ委譲し、全パッケージ PASS のときだけ最終行に
# 「VERIFY: PASS」を出す（機械判定は最終行を見る。パッケージ側の PASS 行が途中に
# 混ざるのは問題ない）。
#
# bootstrap が確定する項目: PACKAGES（実在するパッケージディレクトリのみ。各ディレクトリには
# 対応プラットフォームのテンプレ Makefile が deploy-template.sh で配置済みであること）。
# PACKAGES が空のときは PASS を出さず FAIL する（空PASSの禁止）。
#
# E2E_PORT 等の環境変数は再帰 make ($(MAKE) -C) にそのまま伝播する。

# 例: PACKAGES := frontend backend
PACKAGES := frontend backend

.PHONY: verify verify-fast run artifacts preview-url

verify:
	@test -n "$(strip $(PACKAGES))" || { echo "VERIFY: FAIL (パッケージがゼロ)"; exit 1; }
	@for p in $(PACKAGES); do $(MAKE) -C $$p verify || { echo "VERIFY: FAIL ($$p)"; exit 1; }; done
	@echo "VERIFY: PASS"

verify-fast:
	@test -n "$(strip $(PACKAGES))" || { echo "VERIFY-FAST: FAIL (パッケージがゼロ)"; exit 1; }
	@for p in $(PACKAGES); do $(MAKE) -C $$p verify-fast || { echo "VERIFY-FAST: FAIL ($$p)"; exit 1; }; done
	@echo "VERIFY-FAST: PASS"

# ==== 人間が「動いているところ」を見るための入口 ====
# ルートの run は「主たるアプリ」1つだけを起動する（全パッケージ同時起動はしない——
# 人間が見たいのは画面であって、プロセス一覧ではない）。bootstrap が RUN_PACKAGE を確定する。
RUN_PACKAGE := frontend
run:
	@test -n "$(strip $(RUN_PACKAGE))" || { echo "RUN: FAIL (RUN_PACKAGE 未設定)"; exit 1; }
	@# for ループにするのは make -n 対策。$(MAKE) を含む行は -n でも実行されるため、
	@# 素の `$(MAKE) -C $(RUN_PACKAGE) run` だと未設定時に `make -C ""` が走って落ちる
	@for p in $(RUN_PACKAGE); do $(MAKE) -C $$p run; done

# preview-url も RUN_PACKAGE へ委譲する（契約の説明は各パッケージの Makefile を参照）。
# 単一バイナリが web を配信する構成など、ルート側で起動するプロジェクトは
# ここを直接定義して RUN_PACKAGE を空のままにしてよい。
preview-url:
	@test -n "$(strip $(RUN_PACKAGE))" || { echo "PREVIEW-URL: FAIL (RUN_PACKAGE 未設定)"; exit 1; }
	@for p in $(RUN_PACKAGE); do $(MAKE) -s -C $$p preview-url; done

# artifacts は各パッケージへ委譲し、定義しているものだけ実行する。
artifacts:
	@for p in $(PACKAGES); do \
	  if $(MAKE) -C $$p -n artifacts >/dev/null 2>&1; then \
	    $(MAKE) -C $$p artifacts ARTIFACT_DIR=$(CURDIR)/docs/loop-artifacts; fi; \
	done
