-- docs/specs/15-linear-intake.md 3.6.3, 3.8 (Q81, decided 2026-11-08): the
-- recheck mark an Evaloom setting change puts on an Issue whose held task the
-- change may release. Existing rows get NULL (no recheck requested).
ALTER TABLE linear_issue_states ADD COLUMN recheck_requested_at TEXT;
