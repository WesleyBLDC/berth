-- The agent session a proxy session was authorized through, empty on the owner socket, so the
-- end of that agent session stops the proxy session.
ALTER TABLE proxy_sessions ADD COLUMN agent_session TEXT NOT NULL DEFAULT '';

CREATE INDEX proxy_sessions_by_agent_session ON proxy_sessions (agent_session) WHERE agent_session != '';
