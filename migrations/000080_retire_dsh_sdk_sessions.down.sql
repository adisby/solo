-- No-op: the retired statuses are not recoverable, and reopening a Session would
-- make the ACP transport resume an id the SDK transport minted for one process.
SELECT 1;
