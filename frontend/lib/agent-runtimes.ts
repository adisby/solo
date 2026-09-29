export const SUPPORTED_AGENT_RUNTIME_TYPES = new Set(['openclaw', 'hermes', 'claude', 'opencode', 'codex', 'dsh']);

export function isSupportedAgentRuntime(type: string): boolean {
  return SUPPORTED_AGENT_RUNTIME_TYPES.has(type);
}
