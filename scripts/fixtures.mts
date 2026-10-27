import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

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
  mkdirSync(join(repo, 'evaloom'), { recursive: true })
  writeFileSync(join(repo, 'evaloom', 'profile.yaml'), `profileVersion: "${version}"
harnessVersion: "${version}"
modelId: "claude-e2e-model"
promptVersion: "${promptVersion}"
skillVersions:
  - { name: "e2e-skill", version: "1" }
toolConfiguration:
  allowedTools: ["read", "write"]
contextPolicyVersion: "1"
executionConfiguration:
  adapter: "claude_code"
  args: []
collaborationPolicy: "solo"
evaluationPolicyVersion: null
`)
  git(repo, 'add', 'evaloom/profile.yaml')
  git(repo, 'commit', '--quiet', '-m', `profile ${version} (${promptVersion})`)
  git(repo, 'tag', '-f', `profile/v${version}`)
  return git(repo, 'rev-parse', `refs/tags/profile/v${version}^{commit}`)
}
