import { expect, test } from "bun:test";
import { createApi } from "./index";

test("calls the health endpoint under /v1", async () => {
  const seen: string[] = [];
  const api = createApi("http://api.test/v1", async (req: Request) => {
    seen.push(req.url);
    return Response.json({ status: "ok" });
  });
  const { data } = await api.GET("/health");
  expect(data?.status).toBe("ok");
  expect(seen).toEqual(["http://api.test/v1/health"]);
});
