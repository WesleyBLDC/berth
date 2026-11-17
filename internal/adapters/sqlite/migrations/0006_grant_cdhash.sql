-- The code directory hash berthd read from a granted executable (for a script, its interpreter's),
-- 40 lowercase hex characters; berth run compares it with the suspended child's live cdhash.
-- Grants made before this migration have none and must be granted again.
ALTER TABLE grants ADD COLUMN cdhash TEXT NOT NULL DEFAULT '' CHECK (cdhash = '' OR (kind = 'exe' AND length(cdhash) = 40));
