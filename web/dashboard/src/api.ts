import { startAuthentication } from "@simplewebauthn/browser";
import { type ApiError, createApi } from "@ticketslinger/api-client";

type Fetch = (req: Request) => Promise<Response>;

/** Reads the CSRF cookie: `__Host-ts_csrf` in production, `ts_csrf` in dev. */
export function csrfToken(cookies: string): string {
  const jar = new Map<string, string>();
  for (const part of cookies.split(";")) {
    const i = part.indexOf("=");
    if (i > 0) jar.set(part.slice(0, i).trim(), part.slice(i + 1).trim());
  }
  return jar.get("__Host-ts_csrf") ?? jar.get("ts_csrf") ?? "";
}

/** Copies the CSRF cookie into X-CSRF-Token on every write. */
export function makeCsrfFetch(cookies: () => string, base: Fetch = (r) => fetch(r)): Fetch {
  return (req) => {
    if (req.method !== "GET" && req.method !== "HEAD") {
      req.headers.set("X-CSRF-Token", csrfToken(cookies()));
    }
    return base(req);
  };
}

const csrfFetch = makeCsrfFetch(() => document.cookie);

export const api = createApi("/api", csrfFetch);

/** An API error with a message already written for people. */
export class ApiFailure extends Error {
  constructor(
    readonly code: string,
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

const fallback = (status: number) =>
  status === 429
    ? "That's a lot of tries in a row. Give it a minute, then try again."
    : "Something went wrong on our side. Try again in a moment.";

function failure(status: number, body: unknown): ApiFailure {
  const e = (body as Partial<ApiError> | undefined)?.error;
  return new ApiFailure(e?.code ?? "unknown", e?.message || fallback(status), status);
}

/** Unwraps an openapi-fetch result, throwing ApiFailure on any non-2xx. */
export function must<T>(r: { data?: T; error?: unknown; response: Response }): T {
  if (!r.response.ok) throw failure(r.response.status, r.error);
  return r.data as T;
}

async function post(path: string, body?: unknown): Promise<unknown> {
  const res = await csrfFetch(
    new Request(path, {
      method: "POST",
      headers: body === undefined ? {} : { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    }),
  );
  const data = res.status === 204 ? undefined : await res.json().catch(() => undefined);
  if (!res.ok) throw failure(res.status, data);
  return data;
}

export const session = {
  signup: (name: string, email: string, password: string) =>
    post("/session/signup", { name, email, password }),
  login: (email: string, password: string, totp?: string) =>
    post("/session/login", { email, password, ...(totp ? { totp_code: totp } : {}) }),
  async passkeyLogin() {
    const { challenge_id, options } = must(await api.POST("/auth/passkeys/login"));
    const optionsJSON = (
      options as { publicKey: Parameters<typeof startAuthentication>[0]["optionsJSON"] }
    ).publicKey;
    let assertion: Awaited<ReturnType<typeof startAuthentication>>;
    try {
      assertion = await startAuthentication({ optionsJSON });
    } catch {
      throw new ApiFailure("cancelled", "That passkey didn't go through. Give it another try.", 0);
    }
    return post(`/session/passkey/${encodeURIComponent(challenge_id)}`, assertion);
  },
  logout: () => post("/session/logout"),
};
