import { expect, test } from "bun:test";
import { serverOptions } from "./options";

test("loopback on 3001 unless told otherwise", () => {
  expect(serverOptions({})).toEqual({
    hostname: "127.0.0.1",
    port: 3001,
    app: { apiUrl: "http://127.0.0.1:3000/v1", staticDir: "./dist", turnstileSiteKey: "" },
  });
});

test("environment overrides", () => {
  expect(
    serverOptions({
      TS_STOREFRONT_HOST: "0.0.0.0",
      TS_STOREFRONT_PORT: "3003",
      TS_API_URL: "http://api/v1",
      TS_STOREFRONT_STATIC: "/srv/store",
      TS_TURNSTILE_SITEKEY: "1x00000000000000000000AA",
    }),
  ).toEqual({
    hostname: "0.0.0.0",
    port: 3003,
    app: {
      apiUrl: "http://api/v1",
      staticDir: "/srv/store",
      turnstileSiteKey: "1x00000000000000000000AA",
    },
  });
});
