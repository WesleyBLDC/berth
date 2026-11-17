-- Environment variable names, beyond Berth's fixed allowlist, a grant lets berth run pass to its
-- consumer; comma separated, since names cannot contain a comma.
ALTER TABLE grants ADD COLUMN pass_env TEXT NOT NULL DEFAULT '';
