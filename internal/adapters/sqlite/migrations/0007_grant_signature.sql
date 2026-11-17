-- The kind of code signature berthd verified on a granted executable, so a platform binary,
-- which names no team, is not shown as unsigned. Grants made before this migration keep '' unless
-- they name a team.
ALTER TABLE grants ADD COLUMN signature TEXT NOT NULL DEFAULT ''
    CHECK (signature IN ('', 'unsigned', 'adhoc', 'apple', 'team', 'other'));
UPDATE grants SET signature = 'team' WHERE team_id <> '';
