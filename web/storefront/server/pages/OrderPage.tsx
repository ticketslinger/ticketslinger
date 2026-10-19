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

// What "Email me my tickets again" answered, by the sent= the form's redirect carries.
export type Sent = "1" | "soon" | "pending";

export function sentNotice(sent: Sent, email: string): string {
  if (sent === "1") {
    return `On its way to ${email}. Give it a minute or two, and check spam if it doesn't turn up.`;
  }
  // True whatever happened to the last one: sent, bounced or given up on.
  if (sent === "soon") {
    return "We tried sending these less than 10 minutes ago. Wait until it's been 10 minutes, then try again.";
  }
  // Queued or failing: still trying, with no promise it lands.
  return "We're still trying to send the latest one. Give it a little while, then check your spam folder too.";
}

export function OrderPage({ order, token, sent }: { order: Order; token: string; sent?: Sent }) {
  const e = order.event;
  const status = orderStatus(order);
  // Numbered as the email and the PDF number them: valid tickets only, in the API's order (by id).
  const valid = order.tickets.filter((t) => t.status === "valid");
  const t = encodeURIComponent(token);
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
      {sent && valid.length > 0 && (
        <p class="sent" role="status">
          {sentNotice(sent, order.email)}
        </p>
      )}
      {order.tickets.length > 0 && (
        <ul class="order-tickets">
          {order.tickets.map((tk) => {
            const n = valid.indexOf(tk) + 1;
            return (
              <li key={tk.id} class={tk.status === "void" ? "void" : undefined}>
                <h2>
                  {n > 0 ? `Ticket ${n} of ${valid.length}: ` : ""}
                  {tk.ticket_type_name}
                </h2>
                <p>{tk.attendee_name || "No name given"}</p>
                {n > 0 && tk.qr_png && (
                  <img
                    class="qr"
                    src={`data:image/png;base64,${tk.qr_png}`}
                    alt={`QR code for ticket ${n}`}
                    width={264}
                    height={264}
                  />
                )}
                {tk.status === "void" && (
                  <p class="status">{tk.refunded ? "Refunded" : "Cancelled"}</p>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {valid.length > 0 && (
        <>
          <p>
            <a href={`/orders/${order.code}/tickets.pdf?t=${t}`}>Download all tickets (PDF)</a>
          </p>
          <form method="post" action={`/orders/${order.code}/email`}>
            <input type="hidden" name="t" value={token} />
            <button type="submit">Email me my tickets again</button>
          </form>
        </>
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
