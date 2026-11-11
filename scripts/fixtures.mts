import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { run } from './evaloomd.mts'

export type Repo = { path: string; url: string }

export function git(cwd: string, ...args: string[]): string {
  return run('git', ['-c', 'user.name=evaloom-fixture', '-c', 'user.email=fixture@example.invalid', '-c', 'commit.gpgsign=false', '-c', 'tag.gpgsign=false', ...args], { cwd }).trim()
}

/** origin（bare リポジトリ）に main を push 済みの Git リポジトリを作る。 */
export function createRepo(root: string, name: string): Repo {
  const url = join(root, 'remotes', `${name}.git`)
  const path = join(root, 'repos', name)
  mkdirSync(url, { recursive: true })
  mkdirSync(path, { recursive: true })
  git(url, 'init', '--quiet', '--bare', '-b', 'main')
  git(path, 'init', '--quiet', '-b', 'main')
  git(path, 'commit', '--quiet', '--allow-empty', '-m', 'init')
  git(path, 'remote', 'add', 'origin', url)
  git(path, 'push', '--quiet', 'origin', 'main')
  return { path, url }
}

/** 02-data-model 4.3 の Profile 定義。adapter は Harness と一致させる。 */
export function tagProfile(repo: string, version: string, promptVersion: string): string {
  // A contract v1 Profile (docs/contract/harness-contract-v1.md 5章) and the
  // files it refers to. promptVersion only makes each version's commit differ.
  const files: Record<string, string> = {
    'evaloom/work/AGENTS.md': `E2E instructions (${promptVersion})\n`,
    'evaloom/check/run': '#!/bin/sh\n',
    'evaloom/publish/run': '#!/bin/sh\n',
    'evaloom/profile.yaml': `contractVersion: "1"
profileVersion: "${version}"
name: "e2e-harness"
description: "E2E special harness (${promptVersion})"
acceptanceCriteria: optional
stages:
  work:
    microHarness: "claude_code"
    model: "claude-e2e-model"
    instructions: ["evaloom/work/AGENTS.md"]
    timeoutSec: 600
  check:
    command: ["evaloom/check/run"]
    timeoutSec: 60
  publish:
    command: ["evaloom/publish/run"]
    timeoutSec: 60
`,
  }
  for (const [name, content] of Object.entries(files)) {
    mkdirSync(join(repo, dirname(name)), { recursive: true })
    writeFileSync(join(repo, name), content)
  }
  git(repo, 'add', 'evaloom')
  git(repo, 'commit', '--quiet', '-m', `profile ${version} (${promptVersion})`)
  git(repo, 'tag', '-f', `profile/v${version}`)
  return git(repo, 'rev-parse', `refs/tags/profile/v${version}^{commit}`)
}
