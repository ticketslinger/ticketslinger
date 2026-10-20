/** @jsxImportSource preact */
import type { components } from "@ticketslinger/api-client";
import { formatCents } from "../money";
import { Layout } from "./Layout";

export type StorefrontEvent = components["schemas"]["StorefrontEvent"];
type TicketType = components["schemas"]["StorefrontTicketType"];

export type EventPageProps = {
  data: StorefrontEvent;
  siteKey: string;
  /** The code as the buyer typed it; "" for none. */
  code: string;
  idem: string;
  /** Quantities the buyer picked last time, by ticket type id. */
  chosen?: Record<string, number>;
  /** Shown next to the Get tickets button. */
  message?: string;
  /** Shown next to the code box. */
  codeMessage?: string;
  /** After a sold-out answer: what's left of the types the buyer wanted more of. */
  left?: Record<string, number>;
};

// ICU puts a narrow no-break space before AM/PM; plain spaces read the same and match what we test.
const fmt = (iso: string, timeZone: string, o: Intl.DateTimeFormatOptions) =>
  new Intl.DateTimeFormat("en-US", { timeZone, ...o }).format(new Date(iso)).replace(/\s/g, " ");

export const when = (iso: string, tz: string) =>
  fmt(iso, tz, {
    weekday: "long",
    month: "long",
    day: "numeric",
    year: "numeric",
    hour: "numeric",
    minute: "2-digit",
    timeZoneName: "short",
  });

// "May 1, 10:00 AM"
const onSaleAt = (iso: string, tz: string) =>
  `${fmt(iso, tz, { month: "short", day: "numeric" })}, ${fmt(iso, tz, { hour: "numeric", minute: "2-digit" })}`;

/** What sits inside the all-in price, or "" when nothing does. */
export function priceNote(fee: number, tax: number): string {
  if (fee > 0 && tax > 0)
    return `Includes ${formatCents(fee)} service fee and ${formatCents(tax)} tax`;
  if (fee > 0) return `Includes ${formatCents(fee)} service fee`;
  if (tax > 0) return `Includes ${formatCents(tax)} tax`;
  return "";
}

/** The most of this type one order can take right now; below min_per_order nothing can be bought. */
const most = (t: TicketType) => Math.min(t.max_per_order, t.remaining, 100);

const paragraphs = (text: string) =>
  text
    .split(/\n\s*\n/)
    .map((p) => p.trim())
    .filter(Boolean);

// An API from before all-in prices sends none; show the old price rather than "$NaN".
const hasAllIn = (t: TicketType) => typeof t.all_in_cents === "number";

/** [price cell, note beneath it]. */
function priceOf(t: TicketType, pass: boolean): [string, string] {
  if (!hasAllIn(t)) {
    const old = t.price_cents === 0 ? "Free" : formatCents(t.price_cents);
    return [old, pass && t.price_cents > 0 ? "A service fee is added at checkout." : ""];
  }
  const cell = t.all_in_cents === 0 ? "Free" : formatCents(t.all_in_cents);
  return [cell, priceNote(t.all_in_fee_cents ?? 0, t.all_in_tax_cents ?? 0)];
}

function Picker({
  t,
  tz,
  chosen,
  left,
}: {
  t: TicketType;
  tz: string;
  chosen: number;
  left?: number;
}) {
  if (!t.on_sale) {
    if (t.sales_start && Date.parse(t.sales_start) > Date.now()) {
      return <p class="status">On sale {onSaleAt(t.sales_start, tz)}</p>;
    }
    return <p class="status">Sales have closed</p>;
  }
  const max = most(t);
  if (max < t.min_per_order) return <p class="status">Sold out</p>;
  const pick = Math.min(chosen, max);
  const id = `qty_${t.id}`;
  const options = [0];
  for (let n = Math.max(1, t.min_per_order); n <= max; n++) options.push(n);
  return (
    <div class="picker">
      <label for={id}>How many {t.name}?</label>
      <select id={id} name={id}>
        {options.map((n) => (
          <option key={n} value={String(n)} selected={n === pick}>
            {n}
          </option>
        ))}
      </select>
      {left !== undefined && left > 0 && (
        <p class="notice" role="alert">
          Only {left} {t.name} left.
        </p>
      )}
    </div>
  );
}

export function EventPage(p: EventPageProps) {
  const { organizer, event, ticket_types, promo } = p.data;
  const tz = event.timezone;
  const home = `/${encodeURIComponent(organizer.slug)}/${encodeURIComponent(event.slug)}`;
  const ended = event.state === "ended";
  const buyable = ticket_types.some((t) => t.on_sale && most(t) >= t.min_per_order);

  return (
    <Layout title={`${event.name} · ${organizer.name}`}>
      <header class="event">
        <p class="organizer">{organizer.name}</p>
        <h1>{event.name}</h1>
        <p class="when">{when(event.starts_at, tz)}</p>
        <p class="where">
          {event.venue_name}
          {event.venue_address && <span class="address">{event.venue_address}</span>}
        </p>
      </header>
      {paragraphs(event.description).map((text) => (
        <p class="description" key={text}>
          {text}
        </p>
      ))}

      {ended ? (
        <>
          <p class="ended">This event has ended.</p>
          {p.message && (
            <p class="notice" role="alert">
              {p.message}
            </p>
          )}
        </>
      ) : (
        <>
          <form class="buy" method="post" action={`${home}/checkout`}>
            <ul class="tickets">
              {ticket_types.map((t) => {
                const [cell, note] = priceOf(t, event.fee_mode === "pass");
                return (
                  <li class="ticket" key={t.id}>
                    <div class="ticket-head">
                      <h2>{t.name}</h2>
                      <span class="price">{cell}</span>
                    </div>
                    {note && <p class="price-note">{note}</p>}
                    {t.description && <p class="ticket-description">{t.description}</p>}
                    <Picker t={t} tz={tz} chosen={p.chosen?.[t.id] ?? 0} left={p.left?.[t.id]} />
                  </li>
                );
              })}
            </ul>
            <input type="hidden" name="idem" value={p.idem} />
            {promo && <input type="hidden" name="code" value={p.code} />}
            {p.message && (
              <p class="notice" role="alert">
                {p.message}
              </p>
            )}
            {buyable && (
              <>
                {p.siteKey && (
                  <>
                    <div class="cf-turnstile" data-sitekey={p.siteKey} />
                    {/* Loads Turnstile once the buyer starts on this form. */}
                    <script type="module" src="/assets/turnstile.js" />
                  </>
                )}
                <button type="submit">Get tickets</button>
              </>
            )}
          </form>

          <form class="code" method="get" action={home}>
            <label for="code">Have a code?</label>
            <div class="code-row">
              <input id="code" name="code" value={p.code} maxLength={64} autocomplete="off" />
              <button type="submit">Apply</button>
            </div>
            {p.codeMessage && (
              <p class="notice" role="alert">
                {p.codeMessage}
              </p>
            )}
          </form>
        </>
      )}
      <p class="order-link">
        <a href={`${home}/order-link`}>Already bought? Get your ticket link again</a>
      </p>
    </Layout>
  );
}
