-- The dsh adapter's SDK transport minted its provider session id for one process,
-- so those ids were never resumable. The ACP transport restores a session through
-- session/resume, and an id this adapter cannot restore would make every wake
-- after the first one start another conversation. Retire those rows: dispatch
-- drops a closed binding and cold-starts, while run history keeps its session
-- references.
UPDATE agent_sessions
   SET status = 'closed'
 WHERE provider = 'dsh'
   AND external_session_id IS NOT NULL
   AND status IN ('active', 'rollover_pending');
