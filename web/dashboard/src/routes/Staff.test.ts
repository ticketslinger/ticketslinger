import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { bannerText, minutesLeft } from "../StaffBanner";
import { staffRow } from "./Audit";
import { menuItems } from "./Home";
import { reasonOk, staffAuditLine } from "./Staff";

test("minutesLeft rounds down and never goes below 0", () => {
  const now = Date.parse("2026-10-20T10:00:00Z");
  expect(minutesLeft("2026-10-20T10:47:59Z", now)).toBe(47);
  expect(minutesLeft("2026-10-20T10:00:30Z", now)).toBe(0);
  expect(minutesLeft("2026-10-20T09:00:00Z", now)).toBe(0);
});

test("the banner says what the spec says", () => {
  expect(bannerText({ organizer_name: "Acme Events" }, 47)).toBe(
    "Support mode: viewing Acme Events, read-only. 47 min left.",
  );
});

test("only staff get a Support menu item", () => {
  expect(menuItems({ staff: false })).not.toContain("Support");
  expect(menuItems({ staff: true })).toContain("Support");
});

test("a reason under 10 characters, or over 500, keeps the button off", () => {
  expect(reasonOk("too short")).toBe(false);
  expect(reasonOk("   padded   ")).toBe(false);
  expect(reasonOk("ticket email never arrived")).toBe(true);
  expect(reasonOk("x".repeat(501))).toBe(false);
});

test("audit rows read as the spec sentence", () => {
  expect(staffAuditLine("James Conway", "Buyer says ticket email never arrived")).toBe(
    'TicketSlinger support (James Conway) viewed your organizer: "Buyer says ticket email never arrived"',
  );
  expect(
    staffRow({ action: "staff.session_started", actor_name: "Sam" }, "Checking a refund"),
  ).toBe('TicketSlinger support (Sam) viewed your organizer: "Checking a refund"');
  expect(staffRow({ action: "staff.session_started", actor_name: "Sam" })).toBeNull();
});

// WCAG relative luminance.
const lum = (hex: string) => {
  const [r = 0, g = 0, b = 0] = [1, 3, 5].map((i) => {
    const c = Number.parseInt(hex.slice(i, i + 2), 16) / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};
const ratio = (a: string, b: string) => {
  const [hi = 0, lo = 0] = [lum(a), lum(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
};

test("the banner reads at 4.5:1 in light and dark, and the colour is used once", () => {
  const css = readFileSync(`${import.meta.dir}/../styles.css`, "utf8");
  const pairs = [...css.matchAll(/--staff-banner: (#\w+);\s*--staff-banner-text: (#\w+);/g)];
  expect(pairs.length).toBe(2); // light, then dark
  for (const [, bg = "", fg = ""] of pairs) expect(ratio(bg, fg)).toBeGreaterThanOrEqual(4.5);
  expect(css.match(/var\(--staff-banner\)/g)?.length).toBe(1);
});
