import { expect, test } from "bun:test";
import { serverOptions } from "./options";

test("secure cookies and loopback binding unless told otherwise", () => {
  const o = serverOptions({});
  expect(o.app.secureCookies).toBe(true);
  expect(o.hostname).toBe("127.0.0.1");
  expect(o.port).toBe(3002);
  expect(o.app.apiUrl).toBe("http://127.0.0.1:3000/v1");
  expect(o.app.staticDir).toBe("./dist");
  // NODE_ENV no longer decides; only the explicit dev flag turns Secure off.
  expect(serverOptions({ NODE_ENV: "development" }).app.secureCookies).toBe(true);
  expect(serverOptions({ TS_DASHBOARD_DEV: "0" }).app.secureCookies).toBe(true);
  expect(serverOptions({ TS_DASHBOARD_DEV: "1" }).app.secureCookies).toBe(false);
});

test("environment overrides", () => {
  const o = serverOptions({
    TS_DASHBOARD_HOST: "0.0.0.0",
    TS_DASHBOARD_PORT: "4000",
    TS_API_URL: "http://api/v1",
    TS_DASHBOARD_STATIC: "/srv/dash",
  });
  expect(o).toEqual({
    hostname: "0.0.0.0",
    port: 4000,
    app: {
      apiUrl: "http://api/v1",
      staticDir: "/srv/dash",
      secureCookies: true,
      turnstileSiteKey: "",
    },
  });
});

test("Turnstile site key", () => {
  expect(serverOptions({}).app.turnstileSiteKey).toBe("");
  expect(
    serverOptions({ TS_TURNSTILE_SITEKEY: "1x00000000000000000000AA" }).app.turnstileSiteKey,
  ).toBe("1x00000000000000000000AA");
});
