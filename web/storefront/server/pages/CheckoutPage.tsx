/** @jsxImportSource preact */
import type { components } from "@ticketslinger/api-client";
import type { CheckoutData } from "../../islands/checkout";
import { eventPath } from "../../islands/links";
import { formatCents } from "../money";
import { Layout } from "./Layout";

type Checkout = components["schemas"]["Checkout"];
type CheckoutEvent = components["schemas"]["CheckoutEvent"];

export const EXPIRED =
  "Your 15 minutes ran out and the tickets went back on sale. Start again and we'll hold them for you.";

/** The island's data, with < escaped so nothing in it can end the script block. */
export function checkoutData(token: string, c: Checkout, e: CheckoutEvent): string {
  const data: CheckoutData = {
    token,
    expires_at: c.expires_at,
    quote: c.quote,
    items: c.items,
    ticket_names: Object.fromEntries(e.ticket_types.map((t) => [t.id, t.name])),
    questions: e.questions,
    event_name: e.name,
    timezone: e.timezone,
    organizer: e.organizer,
    event_slug: e.slug,
  };
  return JSON.stringify(data).replace(/</g, "\\u003c");
}

const eventUrl = (e: CheckoutEvent) => eventPath({ organizer: e.organizer, event_slug: e.slug });

/**
 * What the buyer is paying for: a line per ticket type, then discount, tax added on top, fee
 * and total, so the lines add up. Tax already inside a price (charge = price − discount) is
 * noted under the total instead. Each ticket type sets its own, so a cart can have both.
 */
function Quote({ c, e }: { c: Checkout; e: CheckoutEvent }) {
  const q = c.quote;
  const names = new Map(e.ticket_types.map((t) => [t.id, t.name]));
  const lines = c.items.map((it) => {
    const cents = q.tickets
      .filter((t) => t.ticket_type_id === it.ticket_type_id)
      .reduce((sum, t) => sum + t.price_cents, 0);
    return [`${it.quantity} × ${names.get(it.ticket_type_id) ?? "Ticket"}`, formatCents(cents)];
  });
  if (q.discount_cents > 0) lines.push(["Discount", `−${formatCents(q.discount_cents)}`]);
  let added = 0;
  let included = 0;
  for (const t of q.tickets) {
    const net = t.price_cents - t.discount_cents;
    if (t.charge_cents === net) included += t.tax_cents;
    else added += t.charge_cents - net;
  }
  if (added > 0) lines.push(["Tax", formatCents(added)]);
  if (q.fee_mode === "pass" && q.fee_cents > 0)
    lines.push(["Service fee", formatCents(q.fee_cents)]);
  return (
    <ul class="quote">
      {lines.map(([label, amount]) => (
        <li key={label}>
          <span>{label}</span>
          <span>{amount}</span>
        </li>
      ))}
      <li class="total">
        <span>Total</span>
        <span>{formatCents(q.total_cents)}</span>
      </li>
      {included > 0 && <li class="included">Includes {formatCents(included)} tax</li>}
    </ul>
  );
}

export function CheckoutPage({
  token,
  checkout,
  event,
}: {
  token: string;
  checkout: Checkout;
  event: CheckoutEvent;
}) {
  const over = checkout.status === "expired";
  return (
    <Layout title={`Checkout · ${event.name}`}>
      <p class="organizer">Checkout</p>
      <h1>{event.name}</h1>
      <Quote c={checkout} e={event} />
      {over ? (
        <div class="pay">
          <p class="notice" role="alert">
            {EXPIRED}
          </p>
          {eventUrl(event) && (
            <a class="button" href={eventUrl(event)}>
              Back to {event.name}
            </a>
          )}
        </div>
      ) : (
        <>
          <div id="checkout">
            <noscript>
              <p class="notice">Paying needs JavaScript. Turn it on, then reload this page.</p>
            </noscript>
          </div>
          <script
            type="application/json"
            id="checkout-data"
            // JSON with < escaped (checkoutData), never HTML.
            dangerouslySetInnerHTML={{ __html: checkoutData(token, checkout, event) }}
          />
          <script type="module" src="/assets/checkout.js" />
        </>
      )}
    </Layout>
  );
}
