import type { TaskSummary } from '@/api/tasks'

/** Linear 起因のタスク（source=linear）の Issue の識別子と、Issue へのリンク（15-linear-intake 5章）。それ以外は何も出さない。 */
export function LinearIssueLink({ task, className }: { task: Pick<TaskSummary, 'source' | 'linear'>; className?: string }) {
  if (task.source !== 'linear' || !task.linear) return null
  return (
    <a href={task.linear.url} target="_blank" rel="noreferrer" className={className ?? 'underline underline-offset-4'} data-testid="linear-issue-link">
      {task.linear.identifier}
    </a>
  )
}
