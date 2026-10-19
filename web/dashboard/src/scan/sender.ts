import type { components } from "@ticketslinger/api-client";
import { ApiFailure, must } from "../api";
import { withKey } from "../idempotency";
import type { CheckinResult } from "./result";

type Body = components["schemas"]["CreateCheckin"];
export type Post = (
  body: Body,
  key: string,
) => Promise<{ data?: CheckinResult; error?: unknown; response: Response }>;

export type Outcome =
  | { kind: "result"; result: CheckinResult }
  | { kind: "network" }
  | { kind: "wait"; ms: number; message: string }
  | { kind: "error"; message: string };

const MANUAL_WAIT = "Too many check-ins at once. Give it a few seconds, then try again.";

/**
 * One check-in at a time. A new send gets a fresh Idempotency-Key; retry()
 * resends the last body under the same key, so a scan that may have landed
 * still answers green. Anything sent while one is in flight is dropped (null).
 */
export function createSender(post: Post) {
  const key = { current: null as string | null };
  let pending: Body | null = null;
  let busy = false;

  async function run(body: Body): Promise<Outcome> {
    busy = true;
    let retryAfter = 0;
    try {
      const result = await withKey(key, async (k) => {
        const res = await post(body, k);
        retryAfter = Number(res.response.headers.get("Retry-After")) || 0;
        return must(res);
      });
      return { kind: "result", result };
    } catch (e) {
      if (e instanceof ApiFailure && e.status === 429) {
        const ms = Math.min(retryAfter || 5, 60) * 1000;
        return { kind: "wait", ms, message: body.qr ? e.message : MANUAL_WAIT };
      }
      if (!(e instanceof ApiFailure) || e.status === 0 || e.status >= 500)
        return { kind: "network" };
      return { kind: "error", message: e.message };
    } finally {
      busy = false;
    }
  }

  return {
    get busy() {
      return busy;
    },
    async send(body: Body): Promise<Outcome | null> {
      if (busy) return null;
      key.current = null;
      pending = body;
      return run(body);
    },
    async retry(): Promise<Outcome | null> {
      if (busy || !pending) return null;
      return run(pending);
    },
  };
}
