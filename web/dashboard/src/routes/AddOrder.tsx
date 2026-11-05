import type { components } from "@ticketslinger/api-client";
import { useRef, useState } from "react";
import { ApiFailure, api, must } from "../api";
import { type Problem, useProblem } from "../forms";
import { withKey } from "../idempotency";
import { formatCents, parseDollars } from "../money";
import { ErrorText, Field, navigate, useAction, useLoad } from "../ui";
import type { Event } from "./EventForm";
import type { Org } from "./Organizer";

type Body = components["schemas"]["CreateManualOrder"];
type Method = NonNullable<Body["method"]>;
type Availability = components["schemas"]["Availability"];

export type AddOrderForm = {
  kind: Body["kind"];
  email: string;
  buyerName: string;
  lines: { ticket_type_id: string; quantity: string }[];
  /** One name per line, in ticket order. */
  names: string;
  method: Method;
  amount: string;
  sendTickets: boolean;
  note: string;
};

const METHODS: Record<Method, string> = {
  cash: "Cash",
  cheque: "Cheque",
  invoice: "Invoice: they'll pay you later",
  other: "Something else",
};

// The API's ceiling on amount_cents: $10 million.
const MAX_ORDER_CENTS = 1_000_000_000;

const count = (q: string) => (/^\d{1,3}$/.test(q.trim()) ? Number(q) : 0);

/** The form as the API wants it: only the lines with tickets, and no method or amount on a comp. */
export function addOrderBody(f: AddOrderForm): Body {
  const names = f.names.trimEnd();
  const buyer = f.buyerName.trim();
  const note = f.note.trim();
  return {
    kind: f.kind,
    email: f.email.trim(),
    ...(buyer ? { buyer_name: buyer } : {}),
    lines: f.lines
      .filter((l) => count(l.quantity) > 0)
      .map((l) => ({ ticket_type_id: l.ticket_type_id, quantity: count(l.quantity) })),
    ...(names ? { attendees: names.split("\n").map((n) => n.trim()) } : {}),
    ...(f.kind === "manual" ? { method: f.method, amount_cents: parseDollars(f.amount) ?? 0 } : {}),
    send_tickets: f.sendTickets,
    ...(note ? { note } : {}),
  };
}

/** What's wrong before it's sent, or null. The API checks the rest. */
export function addOrderProblem(f: AddOrderForm): Problem | null {
  if (f.kind !== "manual") return null;
  if (f.amount.trim() === "")
    return { field: "amount_cents", message: "Enter what they paid, or $0." };
  const cents = parseDollars(f.amount);
  if (cents === null || cents > MAX_ORDER_CENTS)
    return { field: "amount_cents", message: "Enter it in dollars, like 40 or 12.50." };
  return null;
}

/** sold_out, said per ticket type that's short. Empty when none is (the stock moved again). */
export function soldOutText(
  f: AddOrderForm,
  types: { id: string; name: string }[],
  available: Availability[],
): string {
  return f.lines
    .flatMap((l) => {
      const left = available.find((a) => a.ticket_type_id === l.ticket_type_id)?.remaining;
      const name = types.find((t) => t.id === l.ticket_type_id)?.name;
      return left !== undefined && count(l.quantity) > left ? [`Only ${left} ${name} left.`] : [];
    })
    .join(" ");
}

// The form's own name for each request property the API can name.
const FIELDS: Record<string, string> = {
  email: "email",
  buyer_name: "buyer-name",
  lines: "lines",
  attendees: "names",
  method: "method",
  amount_cents: "amount_cents",
  note: "note",
};

/** Comp and manual orders: tickets an organizer issues by hand. */
export function AddOrder({ org, event }: { org: Org; event: Event }) {
  const path = { organizer: org.slug, event: event.id };
  const { data: types, error: loadError } = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events/{event}/ticket-types", { params: { path } }),
    ),
  );
  const [f, setF] = useState<AddOrderForm | null>(null);
  const [local, setLocal] = useState<Problem | null>(null);
  const action = useAction();
  const p = useProblem("add", local, action.error, action.field, FIELDS);
  // One key per submission; a retry of the same one after a dropped connection reuses it.
  const key = useRef<string | null>(null);

  const form: AddOrderForm | null =
    f ??
    (types
      ? {
          kind: "comp",
          email: "",
          buyerName: "",
          lines: types.map((t) => ({ ticket_type_id: t.id, quantity: "" })),
          names: "",
          method: "cash",
          amount: "",
          sendTickets: true,
          note: "",
        }
      : null);

  const set = (patch: Partial<AddOrderForm>) => {
    if (!form) return;
    key.current = null; // a changed form is a different order
    setLocal(null);
    action.clear();
    setF({ ...form, ...patch });
  };

  const submit = () => {
    if (!form || !types) return;
    const problem = addOrderProblem(form);
    if (problem) return setLocal(problem);
    action.run(async () => {
      const made = await withKey(key, async (k) => {
        const r = await api.POST("/organizers/{organizer}/events/{event}/orders", {
          params: { path, header: { "Idempotency-Key": k } },
          body: addOrderBody(form),
        });
        const e = r.error?.error;
        const short = e?.code === "sold_out" ? soldOutText(form, types, e.available ?? []) : "";
        if (short) throw new ApiFailure("sold_out", short, r.response.status, "lines");
        return must(r);
      });
      navigate(`/o/${org.slug}/events/${event.id}/orders/${made.id}`, false, true);
    });
  };

  const manual = form?.kind === "manual";
  return (
    <>
      <h2>Add order</h2>
      <p className="lede">
        For guests you're letting in free, and for anyone who paid you some other way. Their tickets
        come out of the same capacity as online sales, and no card is charged.
      </p>
      <ErrorText>{loadError?.message}</ErrorText>
      {!form && !loadError && <p className="muted">Loading…</p>}
      {form && types && (
        <form
          className="narrow"
          noValidate
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
        >
          <fieldset className="choices">
            <legend>What kind of order</legend>
            <label className="check">
              <input
                type="radio"
                name="add-kind"
                checked={form.kind === "comp"}
                onChange={() => set({ kind: "comp" })}
              />
              <span>Comp: free</span>
            </label>
            <label className="check">
              <input
                type="radio"
                name="add-kind"
                checked={manual}
                onChange={() => set({ kind: "manual" })}
              />
              <span>Manual: paid outside TicketSlinger</span>
            </label>
          </fieldset>
          <Field
            id="add-email"
            label="Buyer's email"
            type="email"
            required
            maxLength={254}
            autoComplete="off"
            aria-invalid={p.bad("email")}
            value={form.email}
            onChange={(e) => set({ email: e.target.value })}
          />
          <Field
            id="add-buyer-name"
            label="Buyer's name (optional)"
            maxLength={120}
            autoComplete="off"
            aria-invalid={p.bad("buyer-name")}
            value={form.buyerName}
            onChange={(e) => set({ buyerName: e.target.value })}
          />
          <fieldset
            className="choices"
            id="add-lines"
            tabIndex={-1}
            aria-describedby={p.describe("lines")}
          >
            <legend>Tickets</legend>
            {types.length === 0 && <p className="hint">Add a ticket first, then come back.</p>}
            {types.map((t, i) => (
              <Field
                key={t.id}
                id={`add-qty-${t.id}`}
                label={`${t.name}${t.hidden ? " (hidden)" : ""} · ${formatCents(t.price_cents)}`}
                inputMode="numeric"
                maxLength={3}
                value={form.lines[i]?.quantity ?? ""}
                onChange={(e) =>
                  set({
                    lines: form.lines.map((l, j) =>
                      j === i ? { ...l, quantity: e.target.value } : l,
                    ),
                  })
                }
              />
            ))}
          </fieldset>
          <div className="field">
            <label htmlFor="add-names">Names on the tickets (optional)</label>
            <textarea
              id="add-names"
              rows={3}
              aria-invalid={p.bad("names")}
              aria-describedby="add-names-hint"
              value={form.names}
              onChange={(e) => set({ names: e.target.value })}
            />
            <p className="hint" id="add-names-hint">
              One per line, in the order of the tickets above. You can fill them in later too.
            </p>
          </div>
          {manual && (
            <>
              <div className="field">
                <label htmlFor="add-method">How they paid</label>
                <select
                  id="add-method"
                  aria-invalid={p.bad("method")}
                  value={form.method}
                  onChange={(e) => set({ method: e.target.value as Method })}
                >
                  {(Object.keys(METHODS) as Method[]).map((m) => (
                    <option key={m} value={m}>
                      {METHODS[m]}
                    </option>
                  ))}
                </select>
              </div>
              <Field
                id="add-amount_cents"
                label="What they paid (USD)"
                hint="The order's total. For an invoice, what you'll bill them."
                inputMode="decimal"
                aria-invalid={p.bad("amount_cents")}
                value={form.amount}
                onChange={(e) => set({ amount: e.target.value })}
              />
            </>
          )}
          <label className="check">
            <input
              type="checkbox"
              checked={form.sendTickets}
              onChange={(e) => set({ sendTickets: e.target.checked })}
            />
            <span>Email the tickets to the buyer</span>
          </label>
          <div className="field">
            <label htmlFor="add-note">Note for your team (optional)</label>
            <textarea
              id="add-note"
              rows={3}
              maxLength={2000}
              aria-invalid={p.bad("note")}
              aria-describedby="add-note-hint"
              value={form.note}
              onChange={(e) => set({ note: e.target.value })}
            />
            <p className="hint" id="add-note-hint">
              Only your team sees it. The buyer never does.
            </p>
          </div>
          <ErrorText id="add-error">{p.message}</ErrorText>
          <button type="submit" className="inline" disabled={action.busy}>
            Add order
          </button>
        </form>
      )}
    </>
  );
}
