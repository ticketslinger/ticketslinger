// Checkout holds and storefront reads against the load instance on the droplet
// (infra scripts/load-test.sh). Never point it at production: it sells out an event.
// Env: BASE (e.g. http://127.0.0.1:3100), TICKET_TYPE (the seeded ticket type's id).
import { check } from "k6";
import http from "k6/http";
import { Counter } from "k6/metrics";

const BASE = __ENV.BASE;
const held = new Counter("held");
const soldOut = new Counter("sold_out");

export const options = {
  scenarios: {
    holds: {
      executor: "constant-arrival-rate",
      rate: 200,
      timeUnit: "1s",
      duration: "60s",
      preAllocatedVUs: 50,
      maxVUs: 200,
      exec: "hold",
    },
    reads: {
      executor: "constant-arrival-rate",
      rate: 100,
      timeUnit: "1s",
      duration: "60s",
      preAllocatedVUs: 20,
      exec: "read",
    },
  },
  thresholds: {
    "http_req_duration{name:hold}": ["p(95)<150"],
    "http_req_duration{name:read}": ["p(95)<50"],
    checks: ["rate==1"],
  },
};

// 409 sold_out is the expected end of the quota, not a failure.
http.setResponseCallback(http.expectedStatuses(200, 201, 409));

const octet = () => Math.floor(Math.random() * 256);

export function hold() {
  const body = JSON.stringify({
    organizer: "load",
    event: "load",
    items: [{ ticket_type_id: __ENV.TICKET_TYPE, quantity: 1 }],
  });
  // Loopback is a trusted proxy, so each request counts as a different buyer
  // for the per-address limiter, as it would through Nginx.
  const r = http.post(`${BASE}/v1/checkouts`, body, {
    headers: {
      "Content-Type": "application/json",
      "X-Real-IP": `10.${octet()}.${octet()}.${octet()}`,
    },
    tags: { name: "hold" },
  });
  const isSoldOut = r.status === 409 && r.json("error.code") === "sold_out";
  check(r, { "hold: 201, or 409 sold_out": () => r.status === 201 || isSoldOut });
  if (r.status === 201) held.add(1);
  if (isSoldOut) soldOut.add(1);
}

export function read() {
  const r = http.get(`${BASE}/v1/storefront/load/load`, { tags: { name: "read" } });
  check(r, { "read: 200": () => r.status === 200 });
}

// The script reads HELD from stdout and checks it against the database.
export function handleSummary(data) {
  const m = data.metrics;
  const count = (name) => (m[name] ? m[name].values.count : 0);
  const p95 = (name) => (m[name] ? m[name].values["p(95)"].toFixed(1) : "n/a");
  const failed = Object.entries(m)
    .filter(([, v]) => v.thresholds && Object.values(v.thresholds).some((t) => !t.ok))
    .map(([k]) => k);
  return {
    stdout: [
      `HELD=${count("held")}`,
      `SOLD_OUT=${count("sold_out")}`,
      `HOLD_P95_MS=${p95("http_req_duration{name:hold}")}`,
      `READ_P95_MS=${p95("http_req_duration{name:read}")}`,
      `CHECKS_RATE=${m.checks ? m.checks.values.rate : "n/a"}`,
      `THRESHOLDS_FAILED=${failed.join(",") || "none"}`,
      "",
    ].join("\n"),
  };
}
