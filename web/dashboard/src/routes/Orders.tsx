import type { components } from "@ticketslinger/api-client";
import { api, must } from "../api";
import { formatCents } from "../money";
import { formatWhen } from "../time";
import { ErrorText, Link, useLoad } from "../ui";
import type { Event } from "./EventForm";
import { canEdit, type Org } from "./Organizer";

type OrderSummary = components["schemas"]["OrderSummary"];

const STATUS: Record<OrderSummary["status"], string> = {
  paid: "Paid",
  cancelled: "Cancelled",
  refunded: "Refunded",
  partially_refunded: "Partly refunded",
};

/** The badge a comp or manual order gets beside its status; null for one bought online. */
export const sourceBadge = (o: { source: string; manual_method: string | null }): string | null =>
  o.source === "comp" ? "Comp" : o.source === "manual" ? `Manual · ${o.manual_method}` : null;

/**
 * What the event has sold and given back. Gross counts every online order: the list only holds
 * orders that took money. Money taken by hand and comp tickets are counted apart.
 */
export function summarize(orders: OrderSummary[]) {
  const sum = {
    orders: 0,
    tickets: 0,
    grossCents: 0,
    refundedCents: 0,
    feeCents: 0,
    netCents: 0,
    manualCents: 0,
    compTickets: 0,
  };
  for (const o of orders) {
    const standing = o.status === "paid" || o.status === "partially_refunded";
    if (standing) {
      sum.orders += 1;
      sum.tickets += o.ticket_count;
    }
    if (o.source === "manual" && standing) sum.manualCents += o.total_cents;
    if (o.source === "comp") sum.compTickets += o.ticket_count;
    if (o.source !== "online") continue;
    sum.grossCents += o.total_cents;
    sum.refundedCents += o.refunded_cents;
    sum.feeCents += o.fee_cents - o.fee_refunded_cents;
  }
  sum.netCents = sum.grossCents - sum.refundedCents - sum.feeCents;
  return sum;
}

const EMAIL_TAG: Partial<Record<NonNullable<OrderSummary["ticket_email"]["status"]>, string>> = {
  failing: "Email failing",
  gave_up: "Email not sent",
  bounced: "Email bounced",
};

/** The tag an order gets when its ticket email didn't get through; null otherwise. */
export const emailTag = (o: OrderSummary) =>
  (o.ticket_email.status && EMAIL_TAG[o.ticket_email.status]) ?? null;

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
      {canEdit(org) && (
        <p>
          <Link className="button" href={`/o/${org.slug}/events/${event.id}/orders/new`}>
            Add order
          </Link>
        </p>
      )}
      <ErrorText>{error?.message}</ErrorText>
      {!data && !error && <p className="muted">Loading…</p>}
      {data?.length === 0 && <p>No orders yet. Once someone buys, they'll show up here.</p>}
      {sum && data && data.length > 0 && (
        <>
          <p>
            {plural(sum.orders, "order")} · {plural(sum.tickets, "ticket")} ·{" "}
            {formatCents(sum.grossCents)} in sales
            {sum.manualCents > 0 && ` · Taken by hand ${formatCents(sum.manualCents)}`}
          </p>
          <p>
            Refunded or on its way {formatCents(sum.refundedCents)} · Our fee{" "}
            {formatCents(sum.feeCents)} · Yours {formatCents(sum.netCents)}
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
                      {sourceBadge(o) && (
                        <>
                          {" "}
                          <span className="pill source">{sourceBadge(o)}</span>
                        </>
                      )}
                      {emailTag(o) && (
                        <>
                          {" "}
                          <span className="pill failed">{emailTag(o)}</span>
                        </>
                      )}
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
