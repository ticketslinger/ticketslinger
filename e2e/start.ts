// Starts the whole stack for the end-to-end suite: a fresh database, the API
// in test mode on :3001, the built storefront on :3003 and the built dashboard
// on :3002. Playwright runs it.
import { join } from "node:path";
import { SQL } from "bun";
import { bin, DB, url } from "./env";

const root = join(import.meta.dir, "..");
const dashboard = join(root, "web/dashboard");
const storefront = join(root, "web/storefront");

const apiEnv: Record<string, string | undefined> = {
  ...process.env,
  TS_DATABASE_URL: url("ts_app", DB),
  TS_MIGRATE_DATABASE_URL: url("ts_owner", DB),
  // Fixed, test-only key: 32 zero bytes.
  TS_SECRET_KEY: Buffer.alloc(32).toString("base64"),
  TS_HTTP_ADDR: "127.0.0.1:3001",
  TS_PUBLIC_URL: "http://127.0.0.1:3001",
  TS_DASHBOARD_ORIGIN: "http://localhost:3002",
  // Order links in emails and on the checkout point here.
  TS_STOREFRONT_URL: "http://localhost:3003",
  // The storefront reaches the API on loopback and forwards each buyer's address.
  TS_TRUSTED_PROXIES: "127.0.0.1/32,::1/128",
  TS_WEBAUTHN_RPID: "localhost",
  // Staff mode refuses to start without somewhere to send its alerts.
  TS_MAIL_ALERT_TO: "alerts@example.com",
  TS_TEST_MODE: "1",
  // Test mode takes payments through the built-in fake provider, unless the
  // manual stripe-smoke run brings test keys (below).
  TS_STRIPE_WEBHOOK_SECRET: "whsec_e2e",
  // Cloudflare's published test secret: every token passes.
  TS_TURNSTILE_SECRET: "1x0000000000000000000000000000000AA",
};

// The stripe-smoke run: real test-mode Stripe, webhooks through `stripe listen`.
// Never with live keys, and never saying what the keys were.
if (process.env.STRIPE_SMOKE_ACCOUNT) {
  const { TS_STRIPE_SECRET_KEY: sk = "", TS_STRIPE_PUBLISHABLE_KEY: pk = "" } = process.env;
  if (!sk.startsWith("sk_test_") || !pk.startsWith("pk_test_")) {
    throw new Error("stripe smoke runs on test keys only");
  }
  if (!process.env.TS_STRIPE_WEBHOOK_SECRET?.startsWith("whsec_")) {
    throw new Error("stripe smoke needs TS_STRIPE_WEBHOOK_SECRET from stripe listen");
  }
  // The keys already came through with process.env; the secret replaces whsec_e2e.
  apiEnv.TS_STRIPE_WEBHOOK_SECRET = process.env.TS_STRIPE_WEBHOOK_SECRET;
} else {
  // Anything Stripe or TS_PAYMENTS in the caller's environment stays out of the normal suite.
  delete apiEnv.TS_STRIPE_SECRET_KEY;
  delete apiEnv.TS_STRIPE_PUBLISHABLE_KEY;
  delete apiEnv.TS_PAYMENTS;
}

function run(cmd: string[], opts: { cwd?: string; env?: Record<string, string | undefined> } = {}) {
  const r = Bun.spawnSync(cmd, { ...opts, stdout: "inherit", stderr: "inherit" });
  if (r.exitCode !== 0) throw new Error(`${cmd.join(" ")} exited ${r.exitCode}`);
}

// A fresh database every run, so nothing left over from the last one leaks in.
// This DROPS and recreates ticketslinger_e2e: only ever point E2E_PG at a throwaway Postgres.
const admin = new SQL(url("ts_owner", "ticketslinger"));
await admin.unsafe(`DROP DATABASE IF EXISTS ${DB} WITH (FORCE)`);
await admin.unsafe(`CREATE DATABASE ${DB}`);
await admin.close();

run(["go", "build", "-o", bin, "./cmd/ticketslinger"], { cwd: join(root, "api") });
run([bin, "migrate"], { env: apiEnv });
run(["bun", "run", "build"], { cwd: dashboard });
run(["bun", "run", "build"], { cwd: storefront });

const children = [Bun.spawn([bin, "serve"], { env: apiEnv, stdout: "inherit", stderr: "inherit" })];
const stop = (code: number) => {
  for (const c of children) c.kill();
  process.exit(code);
};
process.on("SIGTERM", () => stop(0));
process.on("SIGINT", () => stop(0));

async function ready(url: string) {
  for (let i = 0; ; i++) {
    if (
      await fetch(url).then(
        (r) => r.ok,
        () => false,
      )
    )
      return;
    if (i === 100) stop(1);
    await Bun.sleep(100);
  }
}

await ready("http://127.0.0.1:3001/v1/health");

children.push(
  Bun.spawn(["bun", "server/index.ts"], {
    cwd: storefront,
    env: {
      ...process.env,
      TS_API_URL: "http://127.0.0.1:3001/v1",
      TS_STOREFRONT_PORT: "3003",
      TS_STOREFRONT_STATIC: "./dist",
      // Cloudflare's published test site key: the widget always passes.
      TS_TURNSTILE_SITEKEY: "1x00000000000000000000AA",
    },
    stdout: "inherit",
    stderr: "inherit",
  }),
);
await ready("http://127.0.0.1:3003/healthz");

children.push(
  Bun.spawn(["bun", "server/index.ts"], {
    cwd: dashboard,
    env: {
      ...process.env,
      // Plain http on localhost: Secure cookies would never come back.
      TS_DASHBOARD_DEV: "1",
      TS_API_URL: "http://127.0.0.1:3001/v1",
      TS_DASHBOARD_PORT: "3002",
      TS_DASHBOARD_STATIC: "./dist",
      // Cloudflare's published test site key: the widget always passes.
      TS_TURNSTILE_SITEKEY: "1x00000000000000000000AA",
    },
    stdout: "inherit",
    stderr: "inherit",
  }),
);

// If any server dies, take the others down too so Playwright sees the failure.
await Promise.race(children.map((c) => c.exited));
stop(1);
