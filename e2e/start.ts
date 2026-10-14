// Starts the whole stack for the end-to-end suite: a fresh database, the API
// in test mode on :3001, and the built dashboard on :3002. Playwright runs it.
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SQL } from "bun";

const root = join(import.meta.dir, "..");
const dashboard = join(root, "web/dashboard");
const pg = process.env.E2E_PG ?? "localhost:54329";
const DB = "ticketslinger_e2e";
const url = (role: string, db: string) => `postgres://${role}:${role}@${pg}/${db}?sslmode=disable`;
const bin = join(tmpdir(), "ticketslinger-e2e");

const apiEnv = {
  ...process.env,
  TS_DATABASE_URL: url("ts_app", DB),
  TS_MIGRATE_DATABASE_URL: url("ts_owner", DB),
  // Fixed, test-only key: 32 zero bytes.
  TS_SECRET_KEY: Buffer.alloc(32).toString("base64"),
  TS_HTTP_ADDR: "127.0.0.1:3001",
  TS_PUBLIC_URL: "http://127.0.0.1:3001",
  TS_DASHBOARD_ORIGIN: "http://localhost:3002",
  TS_WEBAUTHN_RPID: "localhost",
  TS_TEST_MODE: "1",
  // Test mode always takes payments through the built-in fake provider.
  TS_STRIPE_WEBHOOK_SECRET: "whsec_e2e",
  // Cloudflare's published test secret: every token passes.
  TS_TURNSTILE_SECRET: "1x0000000000000000000000000000000AA",
};

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

const children = [Bun.spawn([bin, "serve"], { env: apiEnv, stdout: "inherit", stderr: "inherit" })];
const stop = (code: number) => {
  for (const c of children) c.kill();
  process.exit(code);
};
process.on("SIGTERM", () => stop(0));
process.on("SIGINT", () => stop(0));

for (let i = 0; ; i++) {
  if (
    await fetch("http://127.0.0.1:3001/v1/health").then(
      (r) => r.ok,
      () => false,
    )
  )
    break;
  if (i === 100) stop(1);
  await Bun.sleep(100);
}

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

// If either server dies, take the other down too so Playwright sees the failure.
await Promise.race(children.map((c) => c.exited));
stop(1);
