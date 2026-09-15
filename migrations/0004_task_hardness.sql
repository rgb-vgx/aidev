-- Remember how hard each task stated it was, without editing an applied
-- migration. The column follows the style of the existing TEXT columns on
-- tasks (NOT NULL with an empty-string default, where '' means the task did
-- not state a hardness), and the CHECK constraint mirrors
-- task.AllHardnesses exactly (see internal/store/schema_parity_test.go).
ALTER TABLE tasks ADD COLUMN hardness TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD CONSTRAINT tasks_hardness_valid CHECK (hardness IN (
    '', 'TRIVIAL', 'STANDARD', 'HARD'));
