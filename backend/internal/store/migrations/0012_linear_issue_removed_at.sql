-- docs/specs/15-linear-intake.md 3.3, 3.4, 3.8 (Q78, decided 2026-11-08): a
-- deleted or archived Issue keeps its linear_issue_states row with a removed
-- mark so attach_seq survives. Existing rows get NULL; rows already deleted
-- by the earlier implementation cannot be restored.
ALTER TABLE linear_issue_states ADD COLUMN removed_at TEXT;
