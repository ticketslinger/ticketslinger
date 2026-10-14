import type { components } from "@ticketslinger/api-client";
import { useEffect, useRef, useState } from "react";
import { api, must } from "../api";
import { FormDialog, LOCAL, MAX_CENTS, type Problem, Remove, toggle, useProblem } from "../forms";
import { dollarsInput, formatCents, parseDollars } from "../money";
import { formatWhen, fromLocalInput, toLocalInput } from "../time";
import { ErrorText, Field, useAction, useLoad } from "../ui";
import { bpToPercent, type Event, percentToBp } from "./EventForm";
import { canEdit, type Org } from "./Organizer";
import { moveOption } from "./optionList";

type TicketType = components["schemas"]["TicketType"];
type Quota = components["schemas"]["Quota"];

/** Which form field an API error code is about; "" when it's about the form as a whole. */
export function fieldFor(code: string, message: string): string {
  switch (code) {
    case "invalid_name":
      return "name";
    case "invalid_description":
      return "description";
    case "bad_price":
      return "price";
    case "invalid_tax_rate":
      return "tax";
    case "bad_limits":
      return "max";
    case "bad_sales_window":
      return "end";
    case "bad_quota_size":
      return "size";
    case "unknown_quota":
    case "unknown_ticket_type":
    case "live_needs_capacity":
      return "choices";
    case "missing_field":
      return message.includes("price_cents") ? "price" : message.includes("size") ? "size" : "";
    default:
      return "";
  }
}

export type TicketDraft = {
  name: string;
  description: string;
  price: string;
  min: string;
  max: string;
  start: string;
  end: string;
  hidden: boolean;
  tax: string;
  includeTax: "" | "yes" | "no";
  quotas: string[];
};

export const emptyTicket: TicketDraft = {
  name: "",
  description: "",
  price: "",
  min: "1",
  max: "10",
  start: "",
  end: "",
  hidden: false,
  tax: "",
  includeTax: "",
  quotas: [],
};

export const ticketDraftOf = (t: TicketType, tz: string): TicketDraft => ({
  name: t.name,
  description: t.description,
  price: dollarsInput(t.price_cents),
  min: String(t.min_per_order),
  max: String(t.max_per_order),
  start: t.sales_start ? toLocalInput(t.sales_start, tz) : "",
  end: t.sales_end ? toLocalInput(t.sales_end, tz) : "",
  hidden: t.hidden,
  tax: t.tax_rate_bp === null ? "" : bpToPercent(t.tax_rate_bp),
  includeTax: t.prices_include_tax === null ? "" : t.prices_include_tax ? "yes" : "no",
  quotas: [],
});

const whole = (s: string) => (/^\d{1,3}$/.test(s.trim()) ? Number(s) : null);

/** Checks a ticket form and turns it into the API's fields, or says which field is wrong. */
export function ticketBody(
  d: TicketDraft,
  tz: string,
): { body: components["schemas"]["TicketTypeFields"] } | Problem {
  const price_cents = parseDollars(d.price);
  const min = whole(d.min);
  const max = whole(d.max);
  const tax = d.tax.trim() === "" ? null : percentToBp(d.tax);
  if (!d.name.trim()) return { field: "name", message: "Give this ticket a name." };
  if (price_cents === null || price_cents > MAX_CENTS)
    return {
      field: "price",
      message: "Enter a price from $0 to $100,000, like 25 or 12.50. Use 0 for a free ticket.",
    };
  if (min === null || min < 1 || min > 100)
    return {
      field: "min",
      message: "The fewest tickets per order is a whole number from 1 to 100.",
    };
  if (max === null || max < min || max > 100)
    return {
      field: "max",
      message:
        "The most tickets per order is a whole number up to 100, and no lower than the fewest.",
    };
  if ((d.start && !LOCAL.test(d.start)) || (d.end && !LOCAL.test(d.end)))
    return {
      field: d.start && !LOCAL.test(d.start) ? "start" : "end",
      message: "Enter that as a date and a time.",
    };
  if (d.start && d.end && fromLocalInput(d.end, tz) <= fromLocalInput(d.start, tz))
    return { field: "end", message: "Sales have to end after they start." };
  if (d.tax.trim() !== "" && tax === null)
    return {
      field: "tax",
      message: "Use a tax rate from 0 to 100, with up to two decimals, like 8.25.",
    };
  return {
    body: {
      name: d.name.trim(),
      description: d.description,
      price_cents,
      min_per_order: min,
      max_per_order: max,
      hidden: d.hidden,
      sales_start: d.start ? fromLocalInput(d.start, tz) : null,
      sales_end: d.end ? fromLocalInput(d.end, tz) : null,
      tax_rate_bp: tax,
      prices_include_tax: d.includeTax === "" ? null : d.includeTax === "yes",
    },
  };
}

export type QuotaDraft = { name: string; size: string; types: string[] };

/** Checks a capacity form and turns it into the API's fields, or says which field is wrong. */
export function quotaBody(d: QuotaDraft): { body: components["schemas"]["QuotaFields"] } | Problem {
  const digits = /^(?:\d{1,3}(?:,\d{3})+|\d+)$/.test(d.size.trim())
    ? d.size.trim().replaceAll(",", "")
    : "";
  const size = digits && digits.length <= 7 ? Number(digits) : null;
  if (!d.name.trim())
    return { field: "name", message: "Give this capacity a name, like General floor." };
  if (size === null || size > 1_000_000)
    return { field: "size", message: "Enter how many tickets it holds, from 0 to 1,000,000." };
  return { body: { name: d.name.trim(), size, ticket_type_ids: d.types } };
}

/** The API only insists on a capacity while an event is live (ended and cancelled events don't). */
export const needsCapacity = (status: Event["status"]) => status === "live";

const price = (c: number) => (c === 0 ? "Free" : formatCents(c));

function TicketDialog({
  org,
  event,
  ticket,
  quotas,
  onClose,
  onSaved,
}: {
  org: Org;
  event: Event;
  ticket: TicketType | null;
  quotas: Quota[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const start = ticket ? ticketDraftOf(ticket, event.timezone) : emptyTicket;
  const [d, setD] = useState(start);
  const [local, setLocal] = useState<Problem | null>(null);
  const { busy, error, code, clear, run } = useAction();
  const p = useProblem("tt", local, error, code, fieldFor);
  const set = (patch: Partial<TicketDraft>) => {
    setLocal(null);
    clear();
    setD((old) => ({ ...old, ...patch }));
  };
  const live = needsCapacity(event.status);

  function save() {
    const r = ticketBody(d, event.timezone);
    if ("field" in r) return setLocal(r);
    if (!ticket && live && d.quotas.length === 0)
      return setLocal({
        field: "choices",
        message:
          "This event is on sale, so pick the capacity this ticket draws on. Add one in Capacity first if you haven't.",
      });
    run(async () => {
      const path = { organizer: org.slug, event: event.id };
      if (ticket) {
        must(
          await api.PATCH("/organizers/{organizer}/events/{event}/ticket-types/{ticketType}", {
            params: { path: { ...path, ticketType: ticket.id } },
            body: r.body,
          }),
        );
      } else {
        must(
          await api.POST("/organizers/{organizer}/events/{event}/ticket-types", {
            params: { path },
            body: {
              ...r.body,
              price_cents: r.body.price_cents ?? 0,
              name: r.body.name ?? "",
              quota_ids: d.quotas,
            },
          }),
        );
      }
      onSaved();
    });
  }

  return (
    <FormDialog
      title={ticket ? "Edit ticket" : "Add a ticket"}
      dirty={JSON.stringify(d) !== JSON.stringify(start)}
      onClose={onClose}
      onSubmit={save}
      busy={busy}
      submitLabel={ticket ? "Save changes" : "Add ticket"}
    >
      <fieldset className="plain">
        <Field
          id="tt-name"
          label="Name"
          required
          maxLength={120}
          aria-invalid={p.bad("name")}
          value={d.name}
          onChange={(e) => set({ name: e.target.value })}
        />
        <div className="field">
          <label htmlFor="tt-description">Description</label>
          <textarea
            id="tt-description"
            rows={3}
            maxLength={2000}
            aria-invalid={p.bad("description")}
            value={d.description}
            onChange={(e) => set({ description: e.target.value })}
          />
        </div>
        <Field
          id="tt-price"
          label="Price (USD)"
          hint="Use 0 for a free ticket."
          inputMode="decimal"
          required
          aria-invalid={p.bad("price")}
          value={d.price}
          onChange={(e) => set({ price: e.target.value })}
        />
        <div className="pair">
          <Field
            id="tt-min"
            label="Fewest per order"
            inputMode="numeric"
            aria-invalid={p.bad("min")}
            value={d.min}
            onChange={(e) => set({ min: e.target.value })}
          />
          <Field
            id="tt-max"
            label="Most per order"
            inputMode="numeric"
            aria-invalid={p.bad("max")}
            value={d.max}
            onChange={(e) => set({ max: e.target.value })}
          />
        </div>
        <div className="pair">
          <Field
            id="tt-start"
            label="Sales start"
            type="datetime-local"
            aria-invalid={p.bad("start")}
            hint={`In the event's time zone (${event.timezone.replaceAll("_", " ")}). Leave it empty to sell right away.`}
            value={d.start}
            onChange={(e) => set({ start: e.target.value })}
          />
          <Field
            id="tt-end"
            label="Sales end"
            type="datetime-local"
            aria-invalid={p.bad("end")}
            hint="Leave it empty to sell until the event."
            value={d.end}
            onChange={(e) => set({ end: e.target.value })}
          />
        </div>
        <label className="check">
          <input
            type="checkbox"
            checked={d.hidden}
            onChange={(e) => set({ hidden: e.target.checked })}
          />
          <span>
            Hidden
            <span className="hint">Only buyers with a promo code can see this</span>
          </span>
        </label>
        <div className="pair">
          <Field
            id="tt-tax"
            label="Tax rate (%)"
            inputMode="decimal"
            aria-invalid={p.bad("tax")}
            hint={`Leave it empty to use the event's ${bpToPercent(event.tax_rate_bp)}%.`}
            value={d.tax}
            onChange={(e) => set({ tax: e.target.value })}
          />
          <div className="field">
            <label htmlFor="tt-include-tax">Tax and the price</label>
            <select
              id="tt-include-tax"
              value={d.includeTax}
              onChange={(e) => set({ includeTax: e.target.value as TicketDraft["includeTax"] })}
            >
              <option value="">
                Same as the event ({event.prices_include_tax ? "price includes tax" : "tax on top"})
              </option>
              <option value="yes">The price includes tax</option>
              <option value="no">Tax is added on top</option>
            </select>
          </div>
        </div>
        {!ticket && (
          <fieldset
            className="choices"
            id="tt-choices"
            tabIndex={-1}
            aria-describedby={p.describe("choices")}
          >
            <legend>Capacity</legend>
            <p className="hint">
              {quotas.length === 0
                ? "You haven't added any capacity yet. Add one below this list, then pick it here or on the ticket later."
                : live
                  ? "Pick at least one. A sale has to fit every capacity its ticket belongs to."
                  : "Optional now, but you'll need one before you go live."}
            </p>
            {quotas.map((q) => (
              <label className="check" key={q.id}>
                <input
                  type="checkbox"
                  checked={d.quotas.includes(q.id)}
                  onChange={() => set({ quotas: toggle(d.quotas, q.id) })}
                />
                <span>
                  {q.name} <span className="muted">({q.size.toLocaleString("en-US")} tickets)</span>
                </span>
              </label>
            ))}
          </fieldset>
        )}
      </fieldset>
      <ErrorText id={`${p.prefix}-error`}>{p.message}</ErrorText>
    </FormDialog>
  );
}

function QuotaDialog({
  org,
  event,
  quota,
  tickets,
  onClose,
  onSaved,
}: {
  org: Org;
  event: Event;
  quota: Quota | null;
  tickets: TicketType[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const start: QuotaDraft = quota
    ? { name: quota.name, size: String(quota.size), types: quota.ticket_type_ids }
    : { name: "", size: "", types: [] };
  const [d, setD] = useState(start);
  const [local, setLocal] = useState<Problem | null>(null);
  const { busy, error, code, clear, run } = useAction();
  const p = useProblem("q", local, error, code, fieldFor);
  const set = (patch: Partial<QuotaDraft>) => {
    setLocal(null);
    clear();
    setD((old) => ({ ...old, ...patch }));
  };

  function save() {
    const r = quotaBody(d);
    if ("field" in r) return setLocal(r);
    run(async () => {
      const path = { organizer: org.slug, event: event.id };
      if (quota) {
        must(
          await api.PATCH("/organizers/{organizer}/events/{event}/quotas/{quota}", {
            params: { path: { ...path, quota: quota.id } },
            body: r.body,
          }),
        );
      } else {
        must(
          await api.POST("/organizers/{organizer}/events/{event}/quotas", {
            params: { path },
            body: { ...r.body, name: r.body.name ?? "", size: r.body.size ?? 0 },
          }),
        );
      }
      onSaved();
    });
  }

  return (
    <FormDialog
      title={quota ? "Edit capacity" : "Add a capacity"}
      dirty={JSON.stringify(d) !== JSON.stringify(start)}
      onClose={onClose}
      onSubmit={save}
      busy={busy}
      submitLabel={quota ? "Save changes" : "Add capacity"}
    >
      <fieldset className="plain">
        <Field
          id="q-name"
          label="Name"
          required
          maxLength={120}
          aria-invalid={p.bad("name")}
          value={d.name}
          onChange={(e) => set({ name: e.target.value })}
        />
        <Field
          id="q-size"
          label="How many tickets"
          hint="Across every ticket type you pick below."
          inputMode="numeric"
          required
          aria-invalid={p.bad("size")}
          value={d.size}
          onChange={(e) => set({ size: e.target.value })}
        />
        <fieldset
          className="choices"
          id="q-choices"
          tabIndex={-1}
          aria-describedby={p.describe("choices")}
        >
          <legend>Tickets that draw on it</legend>
          {tickets.length === 0 && (
            <p className="hint">Add a ticket first, then come back to pick it.</p>
          )}
          {tickets.map((t) => (
            <label className="check" key={t.id}>
              <input
                type="checkbox"
                checked={d.types.includes(t.id)}
                onChange={() => set({ types: toggle(d.types, t.id) })}
              />
              <span>{t.name}</span>
            </label>
          ))}
        </fieldset>
      </fieldset>
      <ErrorText id={`${p.prefix}-error`}>{p.message}</ErrorText>
    </FormDialog>
  );
}

/** The Tickets tab: what you sell, and the capacity that limits it. */
export function Tickets({ org, event }: { org: Org; event: Event }) {
  const path = { organizer: org.slug, event: event.id };
  const types = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events/{event}/ticket-types", { params: { path } }),
    ),
  );
  const caps = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/events/{event}/quotas", { params: { path } })),
  );
  // undefined: closed; null: adding; otherwise editing that one.
  const [ticket, setTicket] = useState<TicketType | null | undefined>();
  const [quota, setQuota] = useState<Quota | null | undefined>();
  const editor = canEdit(org);
  const tz = event.timezone;
  const reload = () => Promise.all([types.reload(), caps.reload()]);
  const move = useAction();
  const focusAfter = useRef<string | undefined>(undefined);
  const tickets = types.data ?? [];
  const quotas = caps.data ?? [];
  const names = new Map(tickets.map((t) => [t.id, t.name]));
  const covered = new Set(quotas.flatMap((q) => q.ticket_type_ids));
  const uncovered = tickets.filter((t) => !covered.has(t.id));

  // After a move, focus follows the ticket; at an end that arrow is disabled, so use the other.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the reloaded list is the trigger
  useEffect(() => {
    if (!focusAfter.current) return;
    document.getElementById(focusAfter.current)?.focus();
    focusAfter.current = undefined;
  }, [types.data]);

  const step = (i: number, dir: -1 | 1) =>
    move.run(async () => {
      const ids = tickets.map((x) => x.id);
      const edge = i + dir === 0 || i + dir === ids.length - 1;
      focusAfter.current = `tt-${ids[i]}-${edge ? (dir === 1 ? "up" : "down") : dir === 1 ? "down" : "up"}`;
      try {
        must(
          await api.PUT("/organizers/{organizer}/events/{event}/ticket-types/order", {
            params: { path },
            body: { ids: moveOption(ids, i, dir) },
          }),
        );
      } catch (e) {
        focusAfter.current = undefined;
        await types.reload();
        throw e;
      }
      await types.reload();
    });

  return (
    <>
      <section aria-labelledby="tickets-h">
        <div className="heading-row">
          <h2 id="tickets-h">Tickets</h2>
          {editor && (
            <button type="button" className="inline" onClick={() => setTicket(null)}>
              Add a ticket
            </button>
          )}
        </div>
        <ErrorText>{types.error?.message ?? move.error}</ErrorText>
        {!types.data && !types.error && <p className="muted">Loading…</p>}
        {types.data?.length === 0 && (
          <p>
            No tickets yet.{" "}
            {editor
              ? "Add the first one, then give it a capacity so there's a limit on how many you sell."
              : "They'll show up here once someone on your team adds them."}
          </p>
        )}
        {event.status === "draft" && uncovered.length > 0 && (
          <p className="status-box">
            Before you go live, give each of these a capacity:{" "}
            {uncovered.map((t) => t.name).join(", ")}.
          </p>
        )}
        {tickets.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Ticket</th>
                  <th scope="col">Price</th>
                  <th scope="col">On sale</th>
                  <th scope="col">Per order</th>
                  {editor && <th scope="col">Actions</th>}
                </tr>
              </thead>
              <tbody>
                {tickets.map((t, i) => (
                  <tr key={t.id}>
                    <td>
                      {t.name} {t.hidden && <span className="pill">Hidden</span>}
                    </td>
                    <td>{price(t.price_cents)}</td>
                    <td>
                      {t.sales_start ? formatWhen(t.sales_start, tz) : "Right away"}
                      {" to "}
                      {t.sales_end ? formatWhen(t.sales_end, tz) : "the event"}
                    </td>
                    <td>
                      {t.min_per_order} to {t.max_per_order}
                    </td>
                    {editor && (
                      <td className="actions-cell">
                        <div className="row">
                          <button
                            type="button"
                            id={`tt-${t.id}-up`}
                            className="inline secondary"
                            aria-label={`Move ${t.name} up`}
                            disabled={i === 0 || move.busy}
                            onClick={() => step(i, -1)}
                          >
                            Up
                          </button>
                          <button
                            type="button"
                            id={`tt-${t.id}-down`}
                            className="inline secondary"
                            aria-label={`Move ${t.name} down`}
                            disabled={i === tickets.length - 1 || move.busy}
                            onClick={() => step(i, 1)}
                          >
                            Down
                          </button>
                          <button
                            type="button"
                            className="inline secondary"
                            aria-label={`Edit ${t.name}`}
                            onClick={() => setTicket(t)}
                          >
                            Edit
                          </button>
                          <Remove
                            label={t.name}
                            noun="ticket"
                            run={async () => {
                              must(
                                await api.DELETE(
                                  "/organizers/{organizer}/events/{event}/ticket-types/{ticketType}",
                                  { params: { path: { ...path, ticketType: t.id } } },
                                ),
                              );
                              await reload();
                            }}
                          />
                        </div>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="tickets-section" aria-labelledby="capacity-h">
        <div className="heading-row">
          <h2 id="capacity-h">Capacity</h2>
          {editor && (
            <button type="button" className="inline" onClick={() => setQuota(null)}>
              Add a capacity
            </button>
          )}
        </div>
        <p className="muted">
          Capacity is how many tickets you can sell. A sale has to fit every capacity its ticket
          belongs to. A ticket can belong to more than one: say 200 for the whole venue, and 50 for
          the early-bird price.
        </p>
        <ErrorText>{caps.error?.message}</ErrorText>
        {!caps.data && !caps.error && <p className="muted">Loading…</p>}
        {caps.data?.length === 0 && <p>No capacity yet.</p>}
        {quotas.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Tickets</th>
                  <th scope="col">Used by</th>
                  {editor && <th scope="col">Actions</th>}
                </tr>
              </thead>
              <tbody>
                {quotas.map((q) => (
                  <tr key={q.id}>
                    <td>{q.name}</td>
                    <td>{q.size.toLocaleString("en-US")}</td>
                    <td>
                      {q.ticket_type_ids
                        .map((id) => names.get(id))
                        .filter(Boolean)
                        .join(", ") || <span className="muted">No tickets yet</span>}
                    </td>
                    {editor && (
                      <td className="actions-cell">
                        <div className="row">
                          <button
                            type="button"
                            className="inline secondary"
                            aria-label={`Edit ${q.name}`}
                            onClick={() => setQuota(q)}
                          >
                            Edit
                          </button>
                          <Remove
                            label={q.name}
                            noun="capacity"
                            run={async () => {
                              must(
                                await api.DELETE(
                                  "/organizers/{organizer}/events/{event}/quotas/{quota}",
                                  { params: { path: { ...path, quota: q.id } } },
                                ),
                              );
                              await reload();
                            }}
                          />
                        </div>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {ticket !== undefined && (
        <TicketDialog
          org={org}
          event={event}
          ticket={ticket}
          quotas={quotas}
          onClose={() => setTicket(undefined)}
          onSaved={() => {
            setTicket(undefined);
            reload();
          }}
        />
      )}
      {quota !== undefined && (
        <QuotaDialog
          org={org}
          event={event}
          quota={quota}
          tickets={tickets}
          onClose={() => setQuota(undefined)}
          onSaved={() => {
            setQuota(undefined);
            reload();
          }}
        />
      )}
    </>
  );
}
