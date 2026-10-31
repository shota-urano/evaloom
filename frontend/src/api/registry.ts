import type { ApiClient } from './client'

// Registry の本人用 API（01-architecture 5.1、backend/internal/api の projects.go・project_harnesses.go・
// harnesses.go・harness_profiles.go・server.go）の形。

export type Page<T> = { items: T[]; nextCursor: string }

export type ProjectSettings = {
  name: string
  localRepoPath: string
  gitRemoteUrl: string
  runnerId: string
  baseBranch: string
  defaultRole: string
  reviewRole: string | null
  contextLocation: string
  testCommand: string | null
  testReportFormat: string | null
  testReportPath: string | null
  setupCommand: string | null
  copyAllowlist: string[]
  allowedOperations: Record<string, unknown>
  approvalPolicy: Record<string, unknown>
  timeouts: Partial<Record<TimeoutKey, number>>
}

export const TIMEOUT_KEYS = ['setupSec', 'harnessSec', 'testSec'] as const
export type TimeoutKey = (typeof TIMEOUT_KEYS)[number]

/** 役割ごとに固定した Harness Profile（GET /api/projects・/api/projects/{id} の harnesses）。 */
export type ProjectHarness = {
  projectId: string
  role: string
  harnessId: string
  profileId: string
  version: string
  commitSha: string
  assignedAt: string
}

export type Project = ProjectSettings & {
  id: string
  createdAt: string
  updatedAt: string
  harnesses?: ProjectHarness[]
}

/** POST / PATCH の応答。コピー許可リストに秘密情報らしい名前があると warnings が付く（03 3.3）。 */
export type SavedProject = Project & { warnings: string[] }

/** 本人が画面から送る項目。reviewRole と contextLocation は Phase 1 では固定なので送らない。 */
export type ProjectInput = Omit<ProjectSettings, 'reviewRole' | 'contextLocation'>

export type Runner = { id: string; name: string; status: 'online' | 'offline'; lastHeartbeatAt: string | null; createdAt: string }

/** 本人が画面で編集する Harness の項目（03 3.4、backend/internal/model/harness.go）。 */
export type HarnessInput = {
  name: string
  adapter: string
  repoPath: string
  repoUrl: string
  supportedModels: string[]
  availableTools: string[]
  toolPermissions: Record<string, unknown>
}

/** specialty・supportedTasks・contractVersion は Phase 1 では固定値（developer・["development"]・"1"）なので送らない。 */
export type Harness = HarnessInput & {
  id: string
  specialty: string
  supportedTasks: string[]
  contractVersion: string
  createdAt: string
  updatedAt: string
}

export type HarnessProfile = {
  id: string
  harnessId: string
  version: string
  commitSha: string
  modelId: string
  operationStatus: string
  registeredAt: string
}

export type Assignment = { projectId: string; role: string; harnessId: string; profileId: string; assignedAt: string }

/** 一覧 API を nextCursor が空になるまで読む。 */
export async function listAll<T>(api: ApiClient, path: string, signal?: AbortSignal): Promise<T[]> {
  const items: T[] = []
  let cursor = ''
  const separator = path.includes('?') ? '&' : '?'
  for (;;) {
    const query = `limit=200${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`
    const page = await api.get<Page<T>>(`${path}${separator}${query}`, { signal })
    items.push(...page.items)
    if (!page.nextCursor) return items
    cursor = page.nextCursor
  }
}

const projectPath = (id: string) => `/api/projects/${encodeURIComponent(id)}`
const harnessPath = (id: string) => `/api/harnesses/${encodeURIComponent(id)}`

export const registry = {
  listProjects: (api: ApiClient, signal?: AbortSignal) => listAll<Project>(api, '/api/projects', signal),
  getProject: (api: ApiClient, id: string, signal?: AbortSignal) => api.get<Project>(projectPath(id), { signal }),
  createProject: (api: ApiClient, id: string, input: ProjectInput) => api.post<SavedProject>('/api/projects', { id, ...input }),
  updateProject: (api: ApiClient, id: string, input: ProjectInput) => api.patch<SavedProject>(projectPath(id), input),
  assignHarness: (api: ApiClient, id: string, input: { role: string; harnessId: string; version: string }) =>
    api.post<Assignment>(`${projectPath(id)}/harnesses`, input),
  listRunners: (api: ApiClient, signal?: AbortSignal) => listAll<Runner>(api, '/api/runners', signal),
  listHarnesses: (api: ApiClient, signal?: AbortSignal) => listAll<Harness>(api, '/api/harnesses', signal),
  /** 個別の GET は API に無い（backend/internal/api/harnesses.md）ので、一覧から探す。無ければ undefined。 */
  getHarness: async (api: ApiClient, id: string, signal?: AbortSignal) =>
    (await listAll<Harness>(api, '/api/harnesses', signal)).find((harness) => harness.id === id),
  createHarness: (api: ApiClient, id: string, input: HarnessInput) => api.post<Harness>('/api/harnesses', { id, ...input }),
  updateHarness: (api: ApiClient, id: string, input: HarnessInput) => api.patch<Harness>(harnessPath(id), input),
  /** Profile の版の登録（03 3.5）。新規は 201、同じ commit SHA の再登録は 200 で既存の行が返る。 */
  registerHarnessProfile: (api: ApiClient, input: { harnessId: string; version: string }) =>
    api.post<HarnessProfile>('/api/harness-profiles', input),
  listHarnessProfiles: (api: ApiClient, harnessId: string, signal?: AbortSignal) =>
    listAll<HarnessProfile>(api, `/api/harness-profiles?harnessId=${encodeURIComponent(harnessId)}`, signal),
}

/** 02-data-model 3.2 harness_profiles.operation_status の表示名。 */
export function operationStatusLabel(status: string): string {
  if (status === 'provisional') return '暫定運用'
  if (status === 'official') return '正式運用'
  return status
}

export function shortSha(sha: string): string {
  return sha.slice(0, 7)
}
