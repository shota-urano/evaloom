-- Gate 1 (test-change diff check) was removed by owner decision Q36.
-- The CHECK values for gate1_* causes, resume_point 'gate2' and gate IN (1,2,3) remain because changing them requires table rebuilds.
ALTER TABLE executions DROP COLUMN gate1_base_sha;
ALTER TABLE projects DROP COLUMN test_file_patterns;
