import type { components } from "@ticketslinger/api-client";
import { api, must } from "../api";
import { formatCents } from "../money";
import { formatWhen } from "../time";
import { ErrorText, useLoad } from "../ui";
import type { Event } from "./EventForm";
import type { Org } from "./Organizer";

type OrderSummary = components["schemas"]["OrderSummary"];

const STATUS: Record<OrderSummary["status"], string> = {
  paid: "Paid",
  cancelled: "Cancelled",
  refunded: "Refunded",
  partially_refunded: "Partly refunded",
};

/** What the event has sold. Only paid orders count; cancelled and refunded ones are money back or never kept. */
export function summarize(orders: OrderSummary[]) {
  const sum = { orders: 0, tickets: 0, grossCents: 0, feeCents: 0 };
  for (const o of orders) {
    if (o.status !== "paid") continue;
    sum.orders += 1;
    sum.tickets += o.ticket_count;
    sum.grossCents += o.total_cents;
    sum.feeCents += o.fee_cents;
  }
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
                    <td>{o.code}</td>
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
