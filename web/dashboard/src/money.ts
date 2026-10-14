/** "$1,200.50" → 120050 cents; null for anything that isn't a plain non-negative dollar amount. */
export function parseDollars(input: string): number | null {
  const m = /^\$?\s*(\d{1,3}(?:,\d{3})+|\d+)(?:\.(\d{1,2}))?$/.exec(input.trim());
  if (!m) return null;
  const cents =
    Number((m[1] ?? "").replaceAll(",", "")) * 100 + Number((m[2] ?? "").padEnd(2, "0"));
  return Number.isSafeInteger(cents) ? cents : null;
}

/** 120000 → "$1,200.00". Integer maths, USD only. */
export function formatCents(cents: number): string {
  const abs = Math.abs(cents);
  const whole = Math.trunc(abs / 100).toLocaleString("en-US");
  return `${cents < 0 ? "-" : ""}$${whole}.${String(abs % 100).padStart(2, "0")}`;
}
