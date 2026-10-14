import type { components } from "@ticketslinger/api-client";
import { useState } from "react";
import { api, must } from "../api";
import { dollarsInput, formatCents, parseDollars } from "../money";
import { formatWhen, fromLocalInput, toLocalInput } from "../time";
import { ErrorText, Field, useAction, useLoad } from "../ui";
import { bpToPercent, type Event, percentToBp } from "./EventForm";
import { canEdit, type Org } from "./Organizer";
import { FormDialog, LOCAL, type Problem, Remove, toggle, useProblem } from "./Tickets";

type Promo = components["schemas"]["PromoCode"];
type TicketType = components["schemas"]["TicketType"];

const MAX_CENTS = 10_000_000;
const MAX_USES = 1_000_000_000;

/** What the API stores: trimmed and upper-case, so spring and SPRING are one code. */
export const normalizeCode = (s: string) => s.trim().toUpperCase();

/** Which form field an API error code is about; "" when it's about the form as a whole. */
export function promoFieldFor(code: string, message: string): string {
  switch (code) {
    case "bad_code":
    case "code_taken":
      return "code";
    case "bad_discount":
    case "value_required_with_kind":
      return "value";
    case "bad_max_uses":
      return "max";
    case "bad_validity":
      return "until";
    case "applies_to_conflict":
    case "applies_to_required":
    case "unknown_ticket_type":
      return "types";
    case "unlock_not_hidden":
      return "unlocks";
    case "missing_field":
      return message.includes("value") ? "value" : message.includes("code") ? "code" : "";
    default:
      return "";
  }
}

export type PromoDraft = {
  code: string;
  kind: "percent" | "amount";
  value: string;
  maxUses: string;
  from: string;
  until: string;
  all: boolean;
  types: string[];
  unlocks: string[];
};

export const emptyPromo: PromoDraft = {
  code: "",
  kind: "percent",
  value: "",
  maxUses: "",
  from: "",
  until: "",
  all: true,
  types: [],
  unlocks: [],
};

export const promoDraftOf = (p: Promo, tz: string): PromoDraft => ({
  code: p.code,
  kind: p.kind,
  value: p.kind === "percent" ? bpToPercent(p.value) : dollarsInput(p.value),
  maxUses: p.max_uses === null ? "" : String(p.max_uses),
  from: p.valid_from ? toLocalInput(p.valid_from, tz) : "",
  until: p.valid_until ? toLocalInput(p.valid_until, tz) : "",
  all: p.applies_to_all,
  types: p.applies_to,
  unlocks: p.unlocks,
});

/** Checks a promo form and turns it into the API's fields, or says which field is wrong. */
export function promoBody(
  d: PromoDraft,
  tz: string,
): { body: components["schemas"]["PromoCodeFields"] } | Problem {
  const code = normalizeCode(d.code);
  const value = d.kind === "percent" ? percentToBp(d.value) : parseDollars(d.value);
  const max = d.maxUses.trim();
  if (!/^[A-Z0-9-]{3,32}$/.test(code))
    return { field: "code", message: "Use 3 to 32 letters, numbers and hyphens, like SPRING25." };
  if (
    d.value.trim() === "" ||
    value === null ||
    value < 1 ||
    value > (d.kind === "percent" ? 10000 : MAX_CENTS)
  )
    return {
      field: "value",
      message:
        d.kind === "percent"
          ? "Enter a discount from 0.01 to 100, with up to two decimals, like 10 or 12.5."
          : "Enter an amount from $0.01 to $100,000, like 5 or 7.50.",
    };
  if (max !== "" && !(/^\d{1,10}$/.test(max) && Number(max) >= 1 && Number(max) <= MAX_USES))
    return {
      field: "max",
      message: "Use a whole number of 1 or more, or leave it empty for no limit.",
    };
  if (d.from && !LOCAL.test(d.from))
    return { field: "from", message: "Enter that as a date and a time." };
  if (d.until && !LOCAL.test(d.until))
    return { field: "until", message: "Enter that as a date and a time." };
  if (d.from && d.until && fromLocalInput(d.until, tz) <= fromLocalInput(d.from, tz))
    return { field: "until", message: "The code has to stop working after it starts." };
  if (!d.all && d.types.length === 0)
    return {
      field: "types",
      message: "Pick at least one ticket, or choose all tickets.",
    };
  return {
    body: {
      code,
      kind: d.kind,
      value,
      max_uses: max === "" ? null : Number(max),
      valid_from: d.from ? fromLocalInput(d.from, tz) : null,
      valid_until: d.until ? fromLocalInput(d.until, tz) : null,
      applies_to_all: d.all,
      applies_to: d.all ? [] : d.types,
      unlocks: d.unlocks,
    },
  };
}

export const discountText = (p: Pick<Promo, "kind" | "value">) =>
  p.kind === "percent" ? `${bpToPercent(p.value)}% off` : `${formatCents(p.value)} off`;

export const usesText = (p: Pick<Promo, "used_count" | "max_uses">) =>
  `${p.used_count} / ${p.max_uses ?? "no limit"}`;

function PromoDialog({
  org,
  event,
  promo,
  tickets,
  onClose,
  onSaved,
}: {
  org: Org;
  event: Event;
  promo: Promo | null;
  tickets: TicketType[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const start = promo ? promoDraftOf(promo, event.timezone) : emptyPromo;
  const [d, setD] = useState(start);
  const [local, setLocal] = useState<Problem | null>(null);
  const { busy, error, code, clear, run } = useAction();
  const p = useProblem("pc", local, error, code, promoFieldFor);
  const set = (patch: Partial<PromoDraft>) => {
    setLocal(null);
    clear();
    setD((old) => ({ ...old, ...patch }));
  };
  const hidden = tickets.filter((t) => t.hidden);

  function save() {
    const r = promoBody(d, event.timezone);
    if ("field" in r) return setLocal(r);
    run(async () => {
      const path = { organizer: org.slug, event: event.id };
      if (promo) {
        must(
          await api.PATCH("/organizers/{organizer}/events/{event}/promo-codes/{promoCode}", {
            params: { path: { ...path, promoCode: promo.id } },
            body: r.body,
          }),
        );
      } else {
        must(
          await api.POST("/organizers/{organizer}/events/{event}/promo-codes", {
            params: { path },
            body: {
              ...r.body,
              code: r.body.code ?? "",
              kind: r.body.kind ?? "percent",
              value: r.body.value ?? 0,
            },
          }),
        );
      }
      onSaved();
    });
  }

  return (
    <FormDialog
      title={promo ? "Edit promo code" : "Add a promo code"}
      dirty={JSON.stringify(d) !== JSON.stringify(start)}
      onClose={onClose}
      onSubmit={save}
      busy={busy}
      submitLabel={promo ? "Save changes" : "Add promo code"}
    >
      <fieldset className="plain">
        <Field
          id="pc-code"
          label="Code"
          hint="What buyers type. Capitals don't matter; we keep it in capitals."
          required
          maxLength={32}
          autoCapitalize="characters"
          spellCheck={false}
          aria-invalid={p.bad("code")}
          value={d.code}
          onChange={(e) => set({ code: e.target.value.toUpperCase() })}
        />
        <div className="pair">
          <div className="field">
            <label htmlFor="pc-kind">Takes off</label>
            <select
              id="pc-kind"
              value={d.kind}
              onChange={(e) => set({ kind: e.target.value as PromoDraft["kind"], value: "" })}
            >
              <option value="percent">A percentage</option>
              <option value="amount">A dollar amount</option>
            </select>
          </div>
          <Field
            id="pc-value"
            label={d.kind === "percent" ? "Percent off" : "Dollars off (USD)"}
            hint={
              d.kind === "percent"
                ? "From 0.01 to 100, like 10 or 12.5."
                : "Taken off each ticket it applies to."
            }
            inputMode="decimal"
            required
            aria-invalid={p.bad("value")}
            value={d.value}
            onChange={(e) => set({ value: e.target.value })}
          />
        </div>
        <Field
          id="pc-max"
          label="Most uses"
          hint="Leave it empty for no limit."
          inputMode="numeric"
          aria-invalid={p.bad("max")}
          value={d.maxUses}
          onChange={(e) => set({ maxUses: e.target.value })}
        />
        <div className="pair">
          <Field
            id="pc-from"
            label="Works from"
            type="datetime-local"
            aria-invalid={p.bad("from")}
            hint={`In the event's time zone (${event.timezone.replaceAll("_", " ")}). Leave it empty to start right away.`}
            value={d.from}
            onChange={(e) => set({ from: e.target.value })}
          />
          <Field
            id="pc-until"
            label="Works until"
            type="datetime-local"
            aria-invalid={p.bad("until")}
            hint="Leave it empty for no end date."
            value={d.until}
            onChange={(e) => set({ until: e.target.value })}
          />
        </div>
        <fieldset
          className="choices"
          id="pc-types"
          tabIndex={-1}
          aria-describedby={p.describe("types")}
        >
          <legend>Discounts</legend>
          <label className="check">
            <input
              type="radio"
              name="pc-all"
              checked={d.all}
              onChange={() => set({ all: true, types: [] })}
            />
            <span>Every ticket</span>
          </label>
          <label className="check">
            <input
              type="radio"
              name="pc-all"
              checked={!d.all}
              onChange={() => set({ all: false })}
            />
            <span>Only the tickets I pick</span>
          </label>
          {!d.all && tickets.length === 0 && (
            <p className="hint">Add a ticket first, then come back to pick it.</p>
          )}
          {!d.all &&
            tickets.map((t) => (
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
        <fieldset
          className="choices"
          id="pc-unlocks"
          tabIndex={-1}
          aria-describedby={p.describe("unlocks")}
        >
          <legend>Shows hidden tickets</legend>
          <p className="hint">
            {hidden.length === 0
              ? "You have no hidden tickets. Mark a ticket as hidden and a code can reveal it."
              : "Buyers who enter this code can see and buy these."}
          </p>
          {hidden.map((t) => (
            <label className="check" key={t.id}>
              <input
                type="checkbox"
                checked={d.unlocks.includes(t.id)}
                onChange={() => set({ unlocks: toggle(d.unlocks, t.id) })}
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

/** The Promo codes tab. */
export function Promos({ org, event }: { org: Org; event: Event }) {
  const path = { organizer: org.slug, event: event.id };
  const codes = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/events/{event}/promo-codes", { params: { path } })),
  );
  const types = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events/{event}/ticket-types", { params: { path } }),
    ),
  );
  // undefined: closed; null: adding; otherwise editing that one.
  const [promo, setPromo] = useState<Promo | null | undefined>();
  const editor = canEdit(org);
  const tz = event.timezone;
  const tickets = types.data ?? [];
  const names = new Map(tickets.map((t) => [t.id, t.name]));
  const list = codes.data ?? [];
  const named = (ids: string[]) =>
    ids
      .map((id) => names.get(id))
      .filter(Boolean)
      .join(", ");

  return (
    <>
      <section aria-labelledby="promos-h">
        <div className="heading-row">
          <h2 id="promos-h">Promo codes</h2>
          {editor && (
            <button type="button" className="inline" onClick={() => setPromo(null)}>
              Add a promo code
            </button>
          )}
        </div>
        <ErrorText>{codes.error?.message ?? types.error?.message}</ErrorText>
        {!codes.data && !codes.error && <p className="muted">Loading…</p>}
        {codes.data?.length === 0 && (
          <p>
            No promo codes yet.{" "}
            {editor
              ? "Add one to give buyers a discount, or to show them a hidden ticket."
              : "They'll show up here once someone on your team adds them."}
          </p>
        )}
        {list.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Code</th>
                  <th scope="col">Discount</th>
                  <th scope="col">Used</th>
                  <th scope="col">Works</th>
                  <th scope="col">Applies to</th>
                  {editor && <th scope="col">Actions</th>}
                </tr>
              </thead>
              <tbody>
                {list.map((c) => (
                  <tr key={c.id}>
                    <td>{c.code}</td>
                    <td>{discountText(c)}</td>
                    <td>{usesText(c)}</td>
                    <td>
                      {c.valid_from || c.valid_until ? (
                        <>
                          {c.valid_from ? formatWhen(c.valid_from, tz) : "Right away"}
                          {" to "}
                          {c.valid_until ? formatWhen(c.valid_until, tz) : "no end date"}
                        </>
                      ) : (
                        "Any time"
                      )}
                    </td>
                    <td>
                      {c.applies_to_all
                        ? "Every ticket"
                        : named(c.applies_to) || <span className="muted">No tickets left</span>}
                      {c.unlocks.length > 0 && (
                        <span className="hint"> Shows: {named(c.unlocks)}</span>
                      )}
                    </td>
                    {editor && (
                      <td className="actions-cell">
                        <div className="row">
                          <button
                            type="button"
                            className="inline secondary"
                            aria-label={`Edit ${c.code}`}
                            onClick={() => setPromo(c)}
                          >
                            Edit
                          </button>
                          <Remove
                            label={c.code}
                            noun="promo code"
                            run={async () => {
                              must(
                                await api.DELETE(
                                  "/organizers/{organizer}/events/{event}/promo-codes/{promoCode}",
                                  { params: { path: { ...path, promoCode: c.id } } },
                                ),
                              );
                              await codes.reload();
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

      {promo !== undefined && (
        <PromoDialog
          org={org}
          event={event}
          promo={promo}
          tickets={tickets}
          onClose={() => setPromo(undefined)}
          onSaved={() => {
            setPromo(undefined);
            codes.reload();
          }}
        />
      )}
    </>
  );
}
