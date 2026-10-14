// Event times are entered and shown in the event's own zone, never the
// browser's. Intl knows every zone's rules, so it does all the work here.

const formats = new Map<string, Intl.DateTimeFormat>();
function wallFormat(tz: string) {
  let f = formats.get(tz);
  if (!f) {
    f = new Intl.DateTimeFormat("en-US", {
      timeZone: tz,
      hourCycle: "h23",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
    });
    formats.set(tz, f);
  }
  return f;
}

/** The wall-clock time in tz at instant ms, as if that wall clock were UTC. */
function wallAt(ms: number, tz: string): number {
  const p: Record<string, number> = {};
  for (const { type, value } of wallFormat(tz).formatToParts(ms)) p[type] = Number(value);
  return Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute);
}

const MINUTE = 60_000;
const DAY = 86_400_000;
const pad = (n: number) => String(n).padStart(2, "0");

/** An instant as the value of an <input type="datetime-local"> in tz. */
export function toLocalInput(iso: string, tz: string): string {
  const d = new Date(wallAt(Date.parse(iso), tz));
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}T${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
}

/**
 * A datetime-local value read in tz, as an ISO UTC instant. A time the clocks
 * repeat means its first occurrence; a time they skip means the moment they
 * land on afterwards.
 */
export function fromLocalInput(local: string, tz: string): string {
  const [y, mo, d, h, mi] = local.split(/[-T:]/).map(Number);
  const wall = Date.UTC(y, mo - 1, d, h, mi);
  // The zone's offset a day either side; one of them holds at wall unless
  // wall falls in a gap.
  const early = wall - (wallAt(wall - DAY, tz) - (wall - DAY));
  const late = wall - (wallAt(wall + DAY, tz) - (wall + DAY));
  const fits = [early, late].filter((t) => wallAt(t, tz) === wall);
  if (fits.length) return new Date(Math.min(...fits)).toISOString();
  // In a gap: find the first minute past it by bisection.
  let [lo, hi] = early < late ? [early, late] : [late, early];
  while (hi - lo > MINUTE) {
    const mid = lo + Math.floor((hi - lo) / 2 / MINUTE) * MINUTE;
    if (wallAt(mid, tz) - mid === wallAt(hi, tz) - hi) hi = mid;
    else lo = mid;
  }
  return new Date(hi).toISOString();
}

/** A friendly date and time in tz, for lists and headings. */
export function formatWhen(iso: string, tz: string): string {
  return new Intl.DateTimeFormat(undefined, {
    timeZone: tz,
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(iso));
}

/** Every zone this browser knows, with a short list if it can't say. */
export function timeZones(): string[] {
  try {
    const all = Intl.supportedValuesOf("timeZone");
    if (all.length) return all.includes("UTC") ? all : [...all, "UTC"];
  } catch {}
  return [
    "UTC",
    "America/New_York",
    "America/Chicago",
    "America/Denver",
    "America/Los_Angeles",
    "America/Anchorage",
    "Pacific/Honolulu",
    "Europe/London",
    "Europe/Paris",
    "Australia/Sydney",
  ];
}

/** The browser's own zone, which new events start in. */
export const browserZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
