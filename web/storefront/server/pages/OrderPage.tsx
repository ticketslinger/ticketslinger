/** @jsxImportSource preact */
import type { components } from "@ticketslinger/api-client";
import { formatCents } from "../money";
import { when } from "./EventPage";
import { Layout } from "./Layout";

type Order = components["schemas"]["Order"];

const STATUS: Record<Order["status"], string> = {
  paid: "You're going.",
  cancelled:
    "Your payment arrived after the last tickets had gone, so this order was cancelled. We're refunding you in full.",
  refunded: "This order was refunded in full.",
  partially_refunded: "Part of this order was refunded.",
};

export function OrderPage({ order }: { order: Order }) {
  const e = order.event;
  return (
    <Layout title={`Order ${order.code} · ${e.name}`}>
      <p class="organizer">{e.organizer_name}</p>
      <h1>{e.name}</h1>
      <p class="when">{when(e.starts_at, e.timezone)}</p>
      <p class="where">
        {e.venue_name}
        {e.venue_address && <span class="address">{e.venue_address}</span>}
      </p>
      <p class="order-status">{STATUS[order.status]}</p>
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
              {t.status === "void" && <p class="status">No longer valid</p>}
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
