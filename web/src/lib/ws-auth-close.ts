/**
 * WebSocket close codes that mean the session is dead.
 *
 * The hub closes unauthorized sockets with 4001 (see internal/adapters/ws
 * StatusUnauthorized). 4001–4003 are the codes this client has always treated
 * as "wipe JWT, go to /login".
 *
 * Pre-fix servers used RFC 6455 1008 (Policy Violation) instead. That is
 * outside 4001–4003, so an expired JWT reconnected forever and the dashboard
 * sat on pending waiting for monitor.list. 1008 is still treated as auth
 * failure so a mixed-version deploy (old API, new frontend) stops looping.
 */
export const WS_CLOSE_POLICY_VIOLATION = 1008;
export const WS_CLOSE_UNAUTHORIZED = 4001;
export const WS_CLOSE_AUTH_MAX = 4003;

export function isWsAuthFailure(code: number): boolean {
  if (code >= WS_CLOSE_UNAUTHORIZED && code <= WS_CLOSE_AUTH_MAX) return true;
  return code === WS_CLOSE_POLICY_VIOLATION;
}
