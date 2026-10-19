/** @jsxImportSource preact */
import type { components } from "@ticketslinger/api-client";
import { formatCents } from "../money";
import { when } from "./EventPage";
import { Layout } from "./Layout";

type Order = components["schemas"]["Order"];

// The refund rows decide what the buyer reads; the order's status only when there are none
// (free tickets give nothing back). refunded_cents counts refunds still on their way too.
export function orderStatus(order: Order): string {
  const e = order.event;
  const cancelled = e.status === "cancelled";
  const total = order.totals.total_cents;
  if (order.status === "cancelled") {
    // A free order voided: there was never a payment to give back.
    if (total === 0) {
      return cancelled
        ? `${e.name} was cancelled, so these tickets won't get you in any more.`
        : "These tickets were cancelled.";
    }
    return cancelled
      ? `${e.name} was cancelled while your payment was on its way, so we couldn't give you tickets. We're refunding you in full.`
      : "Your payment arrived after the last tickets had gone, so this order was cancelled. We're refunding you in full.";
  }
  const back = formatCents(order.refunded_cents);
  const going = order.tickets.some((t) => t.status === "valid");
  const fee = order.totals.fee_cents;
  if (order.refunded_cents > 0 && !going && fee > 0 && order.refunded_cents + fee === total) {
    return `You're getting everything back except the ${formatCents(fee)} service fee: ${back} is on its way to your card.`;
  }
  if (order.refunded_cents > 0 && order.refunded_cents < total) {
    return `Part of this order was refunded: ${back} is on its way back to your card.`;
  }
  if (order.refunded_cents > 0) {
    return cancelled
      ? `${e.name} was cancelled, so this order was refunded in full: ${back} is on its way back to your card.`
      : `This order was refunded in full: ${back} is on its way back to your card.`;
  }
  if (order.returned_cents > 0 || going) return going ? "You're going." : "";
  if (order.status === "partially_refunded") return "Part of this order was refunded.";
  return cancelled
    ? `${e.name} was cancelled, so these tickets won't get you in any more.`
    : "This order was refunded in full.";
}

export function OrderPage({ order }: { order: Order }) {
  const e = order.event;
  const status = orderStatus(order);
  return (
    <Layout title={`Order ${order.code} · ${e.name}`}>
      <p class="organizer">{e.organizer_name}</p>
      <h1>{e.name}</h1>
      <p class="when">{when(e.starts_at, e.timezone)}</p>
      <p class="where">
        {e.venue_name}
        {e.venue_address && <span class="address">{e.venue_address}</span>}
      </p>
      {status && <p class="order-status">{status}</p>}
      {order.returned_cents > 0 && (
        <p class="notice">
          Your bank sent {formatCents(order.returned_cents)} of your refund back, so it's with{" "}
          {e.organizer_name} again. Get in touch with them to sort out another way to get it to you.
        </p>
      )}
      <p>
        Order {order.code} · {formatCents(order.totals.total_cents)} · sent to {order.email}
      </p>
      {order.tickets.length > 0 && (
        <ul class="order-tickets">
          {order.tickets.map((t, i) => (
            <li key={t.id} class={t.status === "void" ? "void" : undefined}>
              <h2>
                Ticket {i + 1}: {t.ticket_type_name}
              </h2>
              <p>{t.attendee_name || "No name given"}</p>
              {t.status === "void" && <p class="status">{t.refunded ? "Refunded" : "Cancelled"}</p>}
            </li>
          ))}
        </ul>
      )}
      <p class="keep">Keep this link to yourself: anyone with it can see your tickets.</p>
    </Layout>
  );
}

export function NoOrder() {
  return (
    <Layout title="Order not found">
      <h1>That link doesn't open an order. Check your email for the latest one.</h1>
    </Layout>
  );
}
