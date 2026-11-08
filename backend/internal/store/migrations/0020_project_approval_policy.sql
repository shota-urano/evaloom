-- docs/specs/17-approval.md 3.2 (Q60). Phase 1 stored approval_policy
-- without a fixed shape (Q25). Every stored value that is not exactly
-- {"version": 1, "mergePullRequest": "require_approval" | "direct_push"}
-- becomes the default, which keeps PR merges behind the owner's approval.
-- CASE keeps the JSON functions away from text that is not valid JSON.
UPDATE projects
SET approval_policy = '{"mergePullRequest":"require_approval","version":1}'
WHERE NOT CASE
    WHEN NOT json_valid(approval_policy) THEN 0
    WHEN json_type(approval_policy) <> 'object' THEN 0
    ELSE (SELECT count(*) FROM json_each(approval_policy)) = 2
        AND json_type(approval_policy, '$.version') IS 'integer'
        AND json_extract(approval_policy, '$.version') = 1
        AND json_type(approval_policy, '$.mergePullRequest') IS 'text'
        AND json_extract(approval_policy, '$.mergePullRequest') IN ('require_approval', 'direct_push')
END;
