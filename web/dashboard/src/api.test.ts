import { describe, expect, test } from "bun:test";
import { csrfToken, makeCsrfFetch } from "./api";

function recorder() {
  const seen: Request[] = [];
  const fetchImpl = async (req: Request) => {
    seen.push(req);
    return new Response(null, { status: 204 });
  };
  return { seen, fetchImpl };
}

describe("csrfFetch", () => {
  test("adds the CSRF header on POST", async () => {
    const { seen, fetchImpl } = recorder();
    const f = makeCsrfFetch(() => "a=1; ts_csrf=tok123; b=2", fetchImpl);
    await f(new Request("http://x/api/organizers", { method: "POST", body: "{}" }));
    expect(seen[0]?.headers.get("x-csrf-token")).toBe("tok123");
  });

  test("leaves GET alone", async () => {
    const { seen, fetchImpl } = recorder();
    const f = makeCsrfFetch(() => "ts_csrf=tok123", fetchImpl);
    await f(new Request("http://x/api/me"));
    expect(seen[0]?.headers.has("x-csrf-token")).toBe(false);
  });
});

describe("csrfToken", () => {
  test("prefers the __Host- cookie", () => {
    expect(csrfToken("ts_csrf=dev; __Host-ts_csrf=prod")).toBe("prod");
  });
  test("falls back to the dev cookie", () => {
    expect(csrfToken("x=1;ts_csrf=dev")).toBe("dev");
  });
  test("is empty with no cookie", () => {
    expect(csrfToken("")).toBe("");
  });
});
