import { expect, test } from "bun:test";
import type { CheckinResult } from "./result";
import { createSender, type Post } from "./sender";

const ok: CheckinResult = { result: "accepted", ticket: null, checked_in: 1, sold: 2 };
const answer = (status: number, body: unknown, headers: Record<string, string> = {}) => ({
  data: status === 200 ? (body as CheckinResult) : undefined,
  error: status === 200 ? undefined : body,
  response: new Response(null, { status, headers }),
});

test("a second send while one is in flight is dropped, not posted", async () => {
  const keys: string[] = [];
  let release: () => void = () => {};
  const post: Post = (_body, key) => {
    keys.push(key);
    return new Promise((done) => {
      release = () => done(answer(200, ok));
    });
  };
  const s = createSender(post);
  const first = s.send({ ticket_id: "t1" });
  expect(s.busy).toBe(true);
  expect(await s.send({ ticket_id: "t1" })).toBeNull();
  expect(await s.retry()).toBeNull();
  release();
  expect(await first).toEqual({ kind: "result", result: ok });
  expect(keys).toHaveLength(1);
  expect(s.busy).toBe(false);
});

test("Try again reuses the failed scan's key; the next scan gets a new one", async () => {
  const keys: string[] = [];
  let fail = true;
  const post: Post = async (_body, key) => {
    keys.push(key);
    if (fail) throw new TypeError("network down");
    return answer(200, ok);
  };
  const s = createSender(post);
  expect(await s.send({ qr: "abc" })).toEqual({ kind: "network" });
  fail = false;
  expect(await s.retry()).toEqual({ kind: "result", result: ok });
  await s.send({ qr: "def" });
  expect(keys[1]).toBe(keys[0]);
  expect(keys[2]).not.toBe(keys[0]);
});

test("a 5xx is a network result too", async () => {
  const s = createSender(async () => answer(502, { error: { code: "bad_gateway", message: "x" } }));
  expect(await s.send({ qr: "abc" })).toEqual({ kind: "network" });
});

test("429 is a short wait, worded for how the check-in was made", async () => {
  const busy = { error: { code: "rate_limited", message: "That's a lot of scans in one minute." } };
  const s = createSender(async () => answer(429, busy, { "Retry-After": "7" }));
  expect(await s.send({ qr: "abc" })).toEqual({
    kind: "wait",
    ms: 7_000,
    message: "That's a lot of scans in one minute.",
  });
  expect(await s.send({ ticket_id: "t1" })).toEqual({
    kind: "wait",
    ms: 7_000,
    message: "Too many check-ins at once. Give it a few seconds, then try again.",
  });
});

test("any other refusal carries the API's message", async () => {
  const s = createSender(async () => answer(403, { error: { code: "forbidden", message: "No." } }));
  expect(await s.send({ qr: "abc" })).toEqual({ kind: "error", message: "No." });
});
