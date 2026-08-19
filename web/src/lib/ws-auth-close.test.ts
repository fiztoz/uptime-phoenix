/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import {
  isWsAuthFailure,
  WS_CLOSE_AUTH_MAX,
  WS_CLOSE_POLICY_VIOLATION,
  WS_CLOSE_UNAUTHORIZED,
} from "./ws-auth-close";

describe("isWsAuthFailure", () => {
  test("treats the hub's 4001 unauthorized close as a dead session", () => {
    expect(isWsAuthFailure(WS_CLOSE_UNAUTHORIZED)).toBe(true);
    expect(isWsAuthFailure(4002)).toBe(true);
    expect(isWsAuthFailure(WS_CLOSE_AUTH_MAX)).toBe(true);
  });

  test("treats pre-fix 1008 policy-violation as a dead session", () => {
    expect(isWsAuthFailure(WS_CLOSE_POLICY_VIOLATION)).toBe(true);
  });

  test("does not treat a normal close or drop as logout", () => {
    expect(isWsAuthFailure(1000)).toBe(false);
    expect(isWsAuthFailure(1001)).toBe(false);
    expect(isWsAuthFailure(1006)).toBe(false);
    expect(isWsAuthFailure(1011)).toBe(false);
    expect(isWsAuthFailure(4000)).toBe(false);
    expect(isWsAuthFailure(4004)).toBe(false);
  });
});
