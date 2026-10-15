import type { components } from "@ticketslinger/api-client";
import { api, must } from "../api";
import { formatCents } from "../money";
import { formatWhen } from "../time";
import { ErrorText, Link, useLoad } from "../ui";
import type { Event } from "./EventForm";
import type { Org } from "./Organizer";

type OrderSummary = components["schemas"]["OrderSummary"];

const STATUS: Record<OrderSummary["status"], string> = {
  paid: "Paid",
  cancelled: "Cancelled",
  refunded: "Refunded",
  partially_refunded: "Partly refunded",
};

/** What the event has sold and given back. Gross counts every listed order: the list only holds orders that took money. */
export function summarize(orders: OrderSummary[]) {
  const sum = { orders: 0, tickets: 0, grossCents: 0, refundedCents: 0, feeCents: 0, netCents: 0 };
  for (const o of orders) {
    if (o.status === "paid" || o.status === "partially_refunded") {
      sum.orders += 1;
      sum.tickets += o.ticket_count;
    }
    sum.grossCents += o.total_cents;
    sum.refundedCents += o.refunded_cents;
    sum.feeCents += o.fee_cents - o.fee_refunded_cents;
  }
  sum.netCents = sum.grossCents - sum.refundedCents - sum.feeCents;
  return sum;
}

const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? "" : "s"}`;

export function Orders({ org, event }: { org: Org; event: Event }) {
  const { data, error } = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events/{event}/orders", {
        params: { path: { organizer: org.slug, event: event.id } },
      }),
    ),
  );
  const sum = data && summarize(data);

  return (
    <>
      <ErrorText>{error?.message}</ErrorText>
      {!data && !error && <p className="muted">Loading…</p>}
      {data?.length === 0 && <p>No orders yet. Once someone buys, they'll show up here.</p>}
      {sum && data && data.length > 0 && (
        <>
          <p>
            {plural(sum.orders, "order")} · {plural(sum.tickets, "ticket")} ·{" "}
            {formatCents(sum.grossCents)} in sales
          </p>
          <p>
            Refunded {formatCents(sum.refundedCents)} · Our fee {formatCents(sum.feeCents)} · Yours{" "}
            {formatCents(sum.netCents)}
          </p>
          <p className="muted">
            Yours is before Stripe's own processing fee, which Stripe takes in your Stripe account.
          </p>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Order</th>
                  <th scope="col">Email</th>
                  <th scope="col">Tickets</th>
                  <th scope="col">Total</th>
                  <th scope="col">Status</th>
                  <th scope="col">Paid</th>
                </tr>
              </thead>
              <tbody>
                {data.map((o) => (
                  <tr key={o.id}>
                    <td>
                      <Link href={`/o/${org.slug}/events/${event.id}/orders/${o.id}`}>
                        {o.code}
                      </Link>
                    </td>
                    <td>{o.email}</td>
                    <td>{o.ticket_count}</td>
                    <td>{formatCents(o.total_cents)}</td>
                    <td>
                      <span className={`pill ${o.status}`}>{STATUS[o.status]}</span>
                    </td>
                    <td>{o.paid_at ? formatWhen(o.paid_at, event.timezone) : "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </>
  );
}
