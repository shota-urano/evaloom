-- docs/specs/16-linear-feedback.md 3.4.1, 3.7 (Q83, decided 2026-11-08):
-- 1 = the result of the last send that was started is unknown, so the
-- Issue's comments are read before the row is sent again. Rows that are
-- pending and were already tried get 1, because the previous implementation
-- treated rows tried before a restart as needing that check. Other rows get 0.
ALTER TABLE linear_outbox ADD COLUMN outcome_unknown INTEGER NOT NULL DEFAULT 0
    CHECK (outcome_unknown IN (0, 1));
UPDATE linear_outbox SET outcome_unknown = 1 WHERE status = 'pending' AND attempts >= 1;
