import type { components } from "@ticketslinger/api-client";
import { useState } from "react";
import { api, must } from "../api";
import { formatCents } from "../money";
import { formatDay } from "../time";
import { ErrorText, useLoad } from "../ui";
import type { Event } from "./EventForm";
import type { Org } from "./Organizer";

type EventReport = components["schemas"]["EventReport"];

export const EMPTY_REPORT =
  "No sales yet. The figures fill in as soon as the first order comes in.";

export const figures = (r: EventReport): [label: string, value: string][] => [
  ["Ticket sales", formatCents(r.gross_cents)],
  ["Refunded", formatCents(r.refunded_cents)],
  ["Our fee", formatCents(r.platform_fee_cents)],
  ["Yours before Stripe's fees", formatCents(r.net_cents)],
];

export const exportHref = (
  slug: string,
  eventId: string,
  kind: "orders.csv" | "attendees.csv" | "checkins.csv",
) => `/api/organizers/${slug}/events/${eventId}/exports/${kind}`;

/** True when nothing has sold and no money has moved, so the figures would all read zero. */
export const isEmpty = (r: EventReport) => r.gross_cents === 0 && r.orders === 0 && r.sold === 0;

/** Says why the Orders tab can list fewer orders than this figure. */
export const ordersNote = (n: number) =>
  `${n} ${n === 1 ? "order" : "orders"}, refunded ones included. The Orders tab counts only the ones still standing.`;

/** The file name in a Content-Disposition header, or the fallback. */
export function fileName(header: string | null, fallback: string): string {
  const m = /filename="([^"\\]+)"/.exec(header ?? "") ?? /filename=([^;\s]+)/.exec(header ?? "");
  return m?.[1] ?? fallback;
}

async function download(href: string, kind: string): Promise<string | null> {
  try {
    const res = await fetch(href);
    if (!res.ok) {
      const body = (await res.json().catch(() => null)) as { error?: { message?: string } } | null;
      return body?.error?.message || "Something went wrong on our side. Try again in a moment.";
    }
    const url = URL.createObjectURL(await res.blob());
    const a = document.createElement("a");
    a.href = url;
    a.download = fileName(res.headers.get("Content-Disposition"), kind);
    document.body.append(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
    return null;
  } catch {
    return "Couldn't reach the server. Check your connection and try again.";
  }
}

const DOWNLOADS = [
  ["orders.csv", "Download orders (CSV)"],
  ["attendees.csv", "Download attendees (CSV)"],
  ["checkins.csv", "Download check-ins (CSV)"],
] as const;

export function Report({ org, event }: { org: Org; event: Event }) {
  const { data, error } = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events/{event}/report", {
        params: { path: { organizer: org.slug, event: event.id } },
      }),
    ),
  );

  const [saveError, setSaveError] = useState<string | null>(null);
  const empty = data ? isEmpty(data) : false;

  return (
    <>
      <ErrorText>{error?.message}</ErrorText>
      {!data && !error && <p className="muted">Loading…</p>}
      {data && empty && <p>{EMPTY_REPORT}</p>}
      {data && !empty && (
        <>
          <dl>
            {figures(data).map(([label, value]) => (
              <div key={label}>
                <dt>{label}</dt>
                <dd>{value}</dd>
              </div>
            ))}
          </dl>
          <p className="muted">
            Stripe takes its processing fees from your Stripe balance, so they aren't in these
            figures. Your Stripe dashboard shows them.
          </p>
          <p>{ordersNote(data.orders)}</p>

          <h3>By ticket</h3>
          <Table
            head={["Ticket", "Sold", "Checked in"]}
            rows={data.ticket_types.map((t) => [t.id, t.name, t.sold, t.checked_in])}
          />
          <h3>Capacity</h3>
          <Table
            head={["Capacity", "Size", "Left"]}
            rows={data.quotas.map((q) => [q.id, q.name, q.size, q.remaining])}
          />
          <h3>Sales by day</h3>
          <Table
            head={["Day", "Orders", "Tickets", "Sales"]}
            rows={data.sales_by_day.map((d) => [
              d.day,
              formatDay(d.day),
              d.orders,
              d.tickets,
              formatCents(d.gross_cents),
            ])}
          />
          <h3>Promo codes</h3>
          <Table
            head={["Code", "Used", "Discount given"]}
            rows={data.promo_codes.map((p) => [
              p.code,
              p.code,
              p.max_uses == null ? p.used_count : `${p.used_count} of ${p.max_uses}`,
              formatCents(p.discount_cents),
            ])}
          />
        </>
      )}
      <ErrorText>{saveError}</ErrorText>
      <p>
        {DOWNLOADS.map(([kind, label]) => (
          <button
            key={kind}
            type="button"
            onClick={async () => {
              setSaveError(null);
              setSaveError(await download(exportHref(org.slug, event.id, kind), kind));
            }}
          >
            {label}
          </button>
        ))}
      </p>
    </>
  );
}

/** First cell of each row is its key and is not shown. */
function Table({ head, rows }: { head: string[]; rows: (string | number)[][] }) {
  if (rows.length === 0) return <p className="muted">Nothing here yet.</p>;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            {head.map((h) => (
              <th key={h} scope="col">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map(([key, ...cells]) => (
            <tr key={key}>
              {cells.map((c, i) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: cells are fixed by column
                <td key={i}>{c}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
