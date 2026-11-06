import type { components } from "@ticketslinger/api-client";
import { useRef, useState } from "react";
import { api, must } from "../api";
import { FormDialog, toggle } from "../forms";
import { withKey } from "../idempotency";
import { formatCents } from "../money";
import { formatWhen } from "../time";
import { ErrorText, focusOnMount, Link, useAction, useLoad } from "../ui";
import { sourceBadge } from "./Orders";
import { canEdit, type Org } from "./Organizer";

type Detail = components["schemas"]["OrderDetail"];
type Refund = components["schemas"]["Refund"];
type Note = components["schemas"]["OrderNote"];
type OrderPath = { organizer: string; event: string; order: string };

/**
 * What the API will refund for this choice, for the button label only: the same rule the
 * server uses (empty `picked` means every valid ticket). The API's answer is what we show after.
 */
export function refundPreview(order: Detail, picked: string[], feeBack: boolean) {
  const chosen = order.tickets.filter(
    (t) => t.status === "valid" && (picked.length === 0 || picked.includes(t.id)),
  );
  const charge = chosen.reduce((n, t) => n + t.charge_cents, 0);
  const fee = chosen.reduce((n, t) => n + t.fee_cents, 0);
  return {
    tickets: chosen.length,
    amountCents: charge + (order.fee_mode === "pass" && feeBack ? fee : 0),
    feeCents: feeBack ? fee : 0,
  };
}

const REASON: Record<Refund["reason"], string> = {
  organizer: "You refunded it",
  event_cancelled: "Event cancelled",
  late_payment: "Paid after the tickets had gone",
  platform: "TicketSlinger refunded it",
};
const STATE: Record<Refund["status"], string> = {
  requested: "On its way",
  succeeded: "Refunded",
  failed: "Failed",
};
const WHY: Record<NonNullable<Refund["failure"]>, string> = {
  stripe_failed: "Stripe couldn't send it.",
  no_access:
    "We can't reach that Stripe account any more. Refund this one from your Stripe dashboard.",
  gave_up:
    "We couldn't get this through to Stripe, so we stopped trying. Try again, or refund it from your Stripe dashboard.",
};

/** Why a failed refund failed, in the organizer's terms. */
export function failureText(r: Refund): string {
  if (r.returned_by_bank) {
    return "This one went through, then the buyer's bank sent it back, so the money is in your Stripe balance again. Contact the buyer to find another way to pay them back.";
  }
  return r.failure ? WHY[r.failure] : WHY.stripe_failed;
}

/** A bank that sent a refund back would only send a retry back too: the organizer settles it with the buyer. */
export const retryable = (r: Refund) => r.status === "failed" && !r.returned_by_bank;

/** A payment that landed after the event was cancelled was refunded because of the cancellation. */
export const reasonText = (r: Refund, eventCancelled: boolean) =>
  r.reason === "late_payment" && eventCancelled ? REASON.event_cancelled : REASON[r.reason];

export const RENAME_HINT =
  "Their QR code doesn't change. The email they already have shows the old name until you resend it.";

/** What the buyer paid for the tickets a cancel names, a passed-on fee included: the refund preview's rule. */
export const cancelCharge = (order: Detail, ids: string[]) =>
  refundPreview(order, ids, true).amountCents;

/** The cancel dialog's warning: what happens to the money, then whether the buyer hears. */
export function cancelWarning(source: string, chargeCents: number, notify: boolean): string {
  const money =
    source === "online"
      ? `Nothing is refunded: the buyer's ${formatCents(chargeCents)} stays paid.`
      : source === "manual"
        ? `Nothing is refunded here. If they paid you ${formatCents(chargeCents)}, give it back yourself.`
        : "They paid nothing, so there's nothing to refund.";
  // A cancelled ticket can't be refunded later (P7b may add that), so the confirm says so.
  const after =
    source === "online"
      ? " You can't refund these tickets afterwards. To give the money back, use Refund instead."
      : "";
  return `${money} Their tickets stop working straight away.${notify ? " We'll email the buyer to say so." : " The buyer won't be told."}${after}`;
}

/** The cancel request: the reason only goes in the email, so it's sent only with one. */
export const cancelBody = (ids: string[], notify: boolean, reason: string) => ({
  notify,
  ...(ids.length > 0 ? { ticket_ids: ids } : {}),
  ...(notify && reason.trim() ? { reason: reason.trim() } : {}),
});

export const canRenameTicket = (t: { status: string }, org: Org) =>
  t.status === "valid" && canEdit(org);

function RenameTicket({
  path,
  ticket,
  onSaved,
  onCancel,
}: {
  path: { organizer: string; event: string; order: string };
  ticket: Detail["tickets"][number];
  onSaved: () => Promise<void>;
  onCancel: () => void;
}) {
  const [name, setName] = useState(ticket.attendee_name);
  const action = useAction();
  const id = `rename-${ticket.id}`;
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        action.run(async () => {
          must(
            await api.PATCH(
              "/organizers/{organizer}/events/{event}/orders/{order}/tickets/{ticket}",
              {
                params: { path: { ...path, ticket: ticket.id } },
                body: { attendee_name: name },
              },
            ),
          );
          await onSaved();
          onCancel();
        });
      }}
    >
      <label htmlFor={id}>{ticket.ticket_type_name}, name on ticket</label>{" "}
      <input
        id={id}
        value={name}
        maxLength={120}
        ref={focusOnMount}
        aria-describedby={`${id}-hint ${id}-error`}
        onChange={(e) => setName(e.target.value)}
      />{" "}
      <button type="submit" className="inline" disabled={action.busy}>
        Save
      </button>{" "}
      <button type="button" className="inline secondary" onClick={onCancel}>
        Cancel
      </button>
      <p className="hint" id={`${id}-hint`}>
        {RENAME_HINT}
      </p>
      <ErrorText id={`${id}-error`}>{action.error}</ErrorText>
    </form>
  );
}

type Ticket = Detail["tickets"][number];

/**
 * One ticket in the list. The refund checkbox stays on screen while the name is being edited,
 * so a ticket that is picked for refund is always one the organizer can see is picked.
 */
export function TicketLine({
  t,
  org,
  path,
  picked,
  busy,
  renaming,
  refocus,
  refund,
  voidLabel,
  refundable,
  onPick,
  onEdit,
  onClose,
  onSaved,
  onCancel,
}: {
  t: Ticket;
  org: Org;
  path: { organizer: string; event: string; order: string };
  picked: boolean;
  busy: boolean;
  renaming: boolean;
  refocus: boolean;
  refund?: Refund;
  voidLabel: string;
  /** Online orders only: comp and manual ones have no payment to refund. */
  refundable: boolean;
  onPick: () => void;
  onEdit: () => void;
  onClose: () => void;
  onSaved: () => Promise<void>;
  onCancel: () => void;
}) {
  const editable = canRenameTicket(t, org);
  const pickable = editable && refundable;
  const nameText = `${t.ticket_type_name}, ${t.attendee_name || "no name"}`;
  return (
    <li>
      {renaming ? (
        <>
          {pickable && (
            <>
              <input
                type="checkbox"
                aria-label={`Refund ${t.ticket_type_name} ticket`}
                checked={picked}
                disabled={busy}
                onChange={onPick}
              />{" "}
            </>
          )}
          <RenameTicket path={path} ticket={t} onSaved={onSaved} onCancel={onClose} />
        </>
      ) : pickable ? (
        <label>
          <input type="checkbox" checked={picked} disabled={busy} onChange={onPick} /> {nameText}
        </label>
      ) : (
        nameText
      )}
      {!renaming && editable && (
        <>
          {" "}
          <button
            type="button"
            className="inline secondary"
            disabled={busy}
            ref={refocus ? focusOnMount : undefined}
            onClick={onEdit}
          >
            Edit name
          </button>{" "}
          <TicketMenu busy={busy} onCancel={onCancel} />
        </>
      )}
      {t.status === "void" && (
        <>
          {" "}
          <span className={`pill ${refund?.status ?? ""}`}>{voidLabel}</span>
        </>
      )}
    </li>
  );
}

/** A ticket's other actions, folded away under More. */
function TicketMenu({ busy, onCancel }: { busy: boolean; onCancel: () => void }) {
  return (
    <details className="menu">
      <summary>More</summary>
      <button type="button" className="inline secondary" disabled={busy} onClick={onCancel}>
        Cancel ticket
      </button>
    </details>
  );
}

/** Cancel without refund: the tickets named, or every valid one when `ids` is empty. */
function CancelDialog({
  order,
  path,
  ids,
  onClose,
  onDone,
}: {
  order: Detail;
  path: OrderPath;
  ids: string[];
  onClose: () => void;
  onDone: () => Promise<void>;
}) {
  const [notify, setNotify] = useState(true);
  const [reason, setReason] = useState("");
  const action = useAction();
  const key = useRef<string | null>(null);
  const charge = cancelCharge(order, ids);
  const change = (fn: () => void) => {
    key.current = null; // a different choice is a different press
    fn();
  };
  return (
    <FormDialog
      title={ids.length === 0 ? `Cancel order ${order.code}?` : "Cancel this ticket?"}
      dirty={reason !== ""}
      onClose={onClose}
      busy={action.busy}
      submitLabel={ids.length === 0 ? "Cancel order" : "Cancel ticket"}
      closeLabel="Keep it"
      describedBy="cancel-warning"
      onSubmit={() =>
        action.run(async () => {
          await withKey(key, async (k) =>
            must(
              await api.POST("/organizers/{organizer}/events/{event}/orders/{order}/cancel", {
                params: { path, header: { "Idempotency-Key": k } },
                body: cancelBody(ids, notify, reason),
              }),
            ),
          );
          await onDone();
        })
      }
    >
      <p id="cancel-warning">{cancelWarning(order.source, charge, notify)}</p>
      <label className="check">
        <input
          type="checkbox"
          checked={notify}
          onChange={(e) => change(() => setNotify(e.target.checked))}
        />
        <span>Email the buyer</span>
      </label>
      {notify && (
        <div className="field">
          <label htmlFor="cancel-reason">Reason (optional)</label>
          <textarea
            id="cancel-reason"
            rows={3}
            maxLength={500}
            aria-describedby="cancel-reason-hint"
            value={reason}
            onChange={(e) => change(() => setReason(e.target.value))}
          />
          <p className="hint" id="cancel-reason-hint">
            The buyer sees this in the email.
          </p>
        </div>
      )}
      <ErrorText>{action.error}</ErrorText>
    </FormDialog>
  );
}

/** One team note. The body is text, never markup. */
export function NoteItem({
  note,
  timezone,
  canDelete,
  path,
  onDeleted,
}: {
  note: Note;
  timezone: string;
  canDelete: boolean;
  path: OrderPath;
  onDeleted: () => void;
}) {
  const [asking, setAsking] = useState<boolean | null>(null);
  const remove = useAction();
  return (
    <li>
      <p>
        <strong>{note.author_name}</strong>{" "}
        <span className="muted">· {formatWhen(note.created_at, timezone)}</span>
      </p>
      <p className="note-body">{note.body}</p>
      {canDelete &&
        note.can_delete &&
        (asking ? (
          <>
            <p id={`note-${note.id}-question`}>Delete this note? It can't be brought back.</p>
            <div className="row">
              <button
                type="button"
                className="inline"
                ref={focusOnMount}
                aria-describedby={`note-${note.id}-question`}
                disabled={remove.busy}
                onClick={() =>
                  remove.run(async () => {
                    must(
                      await api.DELETE(
                        "/organizers/{organizer}/events/{event}/orders/{order}/notes/{note}",
                        { params: { path: { ...path, note: note.id } } },
                      ),
                    );
                    onDeleted();
                  })
                }
              >
                Delete note
              </button>
              <button type="button" className="inline secondary" onClick={() => setAsking(false)}>
                Keep it
              </button>
            </div>
          </>
        ) : (
          <button
            type="button"
            className="inline secondary"
            ref={asking === false ? focusOnMount : undefined}
            aria-label={`Delete ${note.author_name}'s note`}
            onClick={() => setAsking(true)}
          >
            Delete
          </button>
        ))}
      <ErrorText>{remove.error}</ErrorText>
    </li>
  );
}

/** The team's private notes on the order: owners and admins only. */
function Notes({ org, path, timezone }: { org: Org; path: OrderPath; timezone: string }) {
  const { data, error, reload } = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events/{event}/orders/{order}/notes", {
        params: { path },
      }),
    ),
  );
  const [body, setBody] = useState("");
  const add = useAction();
  const key = useRef<string | null>(null);
  return (
    <section>
      <h2>Notes</h2>
      <p className="muted">Only your team sees these. The buyer never does.</p>
      <ErrorText>{error?.message}</ErrorText>
      {data?.length === 0 && <p>No notes yet.</p>}
      {data && data.length > 0 && (
        <ul className="plain">
          {data.map((n) => (
            <NoteItem
              key={n.id}
              note={n}
              timezone={timezone}
              canDelete={canEdit(org)}
              path={path}
              onDeleted={reload}
            />
          ))}
        </ul>
      )}
      {canEdit(org) && (
        <form
          className="narrow"
          onSubmit={(e) => {
            e.preventDefault();
            add.run(async () => {
              await withKey(key, async (k) =>
                must(
                  await api.POST("/organizers/{organizer}/events/{event}/orders/{order}/notes", {
                    params: { path, header: { "Idempotency-Key": k } },
                    body: { body },
                  }),
                ),
              );
              setBody("");
              await reload();
            });
          }}
        >
          <div className="field">
            <label htmlFor="note-body">Add a note</label>
            <textarea
              id="note-body"
              rows={3}
              maxLength={2000}
              required
              value={body}
              onChange={(e) => {
                key.current = null; // different words are a different note
                setBody(e.target.value);
              }}
            />
          </div>
          <ErrorText>{add.error}</ErrorText>
          <button type="submit" className="inline" disabled={add.busy || body.trim() === ""}>
            Add note
          </button>
        </form>
      )}
    </section>
  );
}

const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? "" : "s"}`;

type OrderEmail = components["schemas"]["OrderEmail"];
const PROBLEM: Record<NonNullable<OrderEmail["problem"]>, string> = {
  rate_limited: "the mail service asked us to slow down",
  quota: "we've hit our sending limit for now",
  refused: "the mail service turned the message down",
  unreachable: "we couldn't reach the mail service",
  bad_address: "that address can't take mail",
  config: "our mail settings need fixing (that one's on us)",
};

/** The "Ticket email" line: how the order's email is getting on. null before there is one. */
export function emailLine(e: OrderEmail, when: (iso: string) => string): string | null {
  const at = e.updated_at ? ` ${when(e.updated_at)}` : "";
  const last = e.problem ? ` Last problem: ${PROBLEM[e.problem]}.` : "";
  switch (e.status) {
    case null:
      return null;
    case "queued":
      return "On its way.";
    case "sent":
      return `Sent${at}.`;
    case "delivered":
      return `Delivered${at}.`;
    case "failing":
      return `Not sent yet. We've tried ${e.attempts === 1 ? "once" : `${e.attempts} times`} and we'll keep going for a while.${last}`;
    case "gave_up":
      return `We gave up after ${e.attempts === 1 ? "one try" : `${e.attempts} tries`}.${last}`;
    case "bounced":
      return "Bounced. The buyer's mail server turned it away, so the address may have a typo. Check it with them.";
    case "complained":
      return "Delivered, then the buyer marked it as spam.";
  }
}

/** Sending again waits until the last email has gone, or we've stopped trying. */
export const canResend = (e: OrderEmail) =>
  e.status !== null && e.status !== "queued" && e.status !== "failing";

function ResendEmail({
  path,
  onSent,
}: {
  path: { organizer: string; event: string; order: string };
  onSent: () => Promise<void>;
}) {
  const action = useAction();
  const key = useRef<string | null>(null);
  return (
    <>
      <button
        type="button"
        className="inline secondary"
        disabled={action.busy}
        onClick={() =>
          action.run(async () => {
            await withKey(key, async (k) =>
              must(
                await api.POST("/organizers/{organizer}/events/{event}/orders/{order}/email", {
                  params: { path, header: { "Idempotency-Key": k } },
                }),
              ),
            );
            await onSent();
          })
        }
      >
        Send the tickets again
      </button>
      <ErrorText>{action.error}</ErrorText>
    </>
  );
}

function RefundRow({
  r,
  canRetry,
  eventCancelled,
  onRetried,
  org,
  ev,
  order,
}: {
  r: Refund;
  canRetry: boolean;
  eventCancelled: boolean;
  onRetried: () => void;
  org: string;
  ev: string;
  order: string;
}) {
  const action = useAction();
  const key = useRef<string | null>(null);
  return (
    <li>
      <p>
        <strong>{formatCents(r.amount_cents)}</strong> ·{" "}
        {r.fee_back ? "with our fee" : "without our fee"} · {reasonText(r, eventCancelled)} ·{" "}
        <span className={`pill ${r.status}`}>{STATE[r.status]}</span>
      </p>
      {r.status === "failed" && (
        <>
          <p>{failureText(r)}</p>
          {canRetry && retryable(r) && (
            <button
              type="button"
              className="inline secondary"
              disabled={action.busy}
              onClick={() =>
                action.run(async () => {
                  await withKey(key, async (k) =>
                    must(
                      await api.POST(
                        "/organizers/{organizer}/events/{event}/orders/{order}/refunds/{refund}/retry",
                        {
                          params: {
                            path: { organizer: org, event: ev, order, refund: r.id },
                            header: { "Idempotency-Key": k },
                          },
                        },
                      ),
                    ),
                  );
                  onRetried();
                })
              }
            >
              Try again
            </button>
          )}
          <ErrorText>{action.error}</ErrorText>
        </>
      )}
    </li>
  );
}

export function OrderDetail({
  org,
  eventId,
  orderId,
}: {
  org: Org;
  eventId: string;
  orderId: string;
}) {
  const path = { organizer: org.slug, event: eventId, order: orderId };
  const { data: event } = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events/{event}", {
        params: { path: { organizer: org.slug, event: eventId } },
      }),
    ),
  );
  const {
    data: order,
    error,
    reload,
  } = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events/{event}/orders/{order}", { params: { path } }),
    ),
  );
  const [picked, setPicked] = useState<string[]>([]);
  // Unchecked unless the organizer decides otherwise: the platform keeps its fee by default.
  const [feeBack, setFeeBack] = useState(false);
  const [asking, setAsking] = useState<boolean | null>(null);
  const [resentTo, setResentTo] = useState("");
  const [renaming, setRenaming] = useState<string | null>(null);
  // The ticket whose Edit name button gets focus back after Save or Cancel.
  const [refocus, setRefocus] = useState<string | null>(null);
  const refund = useAction();
  const key = useRef<string | null>(null);
  // The tickets the cancel dialog is about: [] for the whole order, null while it's closed.
  const [cancelling, setCancelling] = useState<string[] | null>(null);

  const change = (fn: () => void) => {
    key.current = null; // a different choice is a different press
    setAsking(null);
    fn();
  };

  const refundOf = (id: string | null) => order?.refunds.find((r) => r.id === id);
  const voidLabel = (id: string | null) => {
    const r = refundOf(id);
    if (!r) return "Cancelled";
    return r.status === "succeeded"
      ? "Refunded"
      : r.status === "requested"
        ? "Refund on its way"
        : "Refund failed";
  };

  const mailLine =
    order && event && emailLine(order.ticket_email, (iso) => formatWhen(iso, event.timezone));
  const valid = order?.tickets.filter((t) => t.status === "valid") ?? [];
  const preview = order && refundPreview(order, picked, feeBack);
  const label =
    picked.length === 0
      ? "Refund the whole order"
      : `Refund ${plural(preview?.tickets ?? 0, "ticket")}`;
  const money = formatCents(preview?.amountCents ?? 0);

  const confirm = () =>
    refund.run(async () => {
      if (!preview) return;
      await withKey(key, async (k) =>
        must(
          await api.POST("/organizers/{organizer}/events/{event}/orders/{order}/refunds", {
            params: { path, header: { "Idempotency-Key": k } },
            body: {
              refund_fee: feeBack && preview.feeCents > 0,
              ...(picked.length > 0 ? { ticket_ids: picked } : {}),
            },
          }),
        ),
      );
      setPicked([]);
      setAsking(null);
      await reload();
    });

  return (
    <>
      <p className="crumbs">
        <Link href={`/o/${org.slug}/events/${eventId}/orders`}>{event?.name ?? "Orders"}</Link>
      </p>
      <h1 tabIndex={-1}>{order ? `Order ${order.code}` : "Order"}</h1>
      {error?.status === 404 ? (
        <p>There's no such order.</p>
      ) : (
        <ErrorText>{error?.message}</ErrorText>
      )}
      {order && (
        <>
          <p>
            {order.buyer_name && `${order.buyer_name} · `}
            {order.email}
            {sourceBadge(order) && (
              <>
                {" "}
                <span className="pill source">{sourceBadge(order)}</span>
              </>
            )}
            {order.paid_at && event && (
              <span className="muted"> · paid {formatWhen(order.paid_at, event.timezone)}</span>
            )}
          </p>
          {mailLine && (
            <p>
              <strong>Ticket email</strong> · {mailLine}
            </p>
          )}
          {resentTo && <p role="status">Sending again to {resentTo}.</p>}
          {canEdit(org) && canResend(order.ticket_email) && (
            <ResendEmail
              path={path}
              onSent={async () => {
                setResentTo(order.email);
                await reload();
              }}
            />
          )}
          <p>
            Total {formatCents(order.total_cents)} · Refunded or on its way{" "}
            {formatCents(order.refunded_cents)} · Our fee{" "}
            {formatCents(order.fee_cents - order.fee_refunded_cents)}
          </p>

          <h2>Tickets</h2>
          <ul className="plain">
            {order.tickets.map((t) => (
              <TicketLine
                key={t.id}
                t={t}
                org={org}
                path={path}
                picked={picked.includes(t.id)}
                busy={refund.busy}
                renaming={renaming === t.id}
                refocus={refocus === t.id}
                refund={refundOf(t.refund_id)}
                voidLabel={voidLabel(t.refund_id)}
                refundable={order.source === "online"}
                onCancel={() => setCancelling([t.id])}
                onPick={() => change(() => setPicked(toggle(picked, t.id)))}
                onEdit={() => {
                  setRefocus(null);
                  setRenaming(t.id);
                }}
                onClose={() => {
                  setRefocus(t.id);
                  setRenaming(null);
                }}
                onSaved={reload}
              />
            ))}
          </ul>

          {canEdit(org) && valid.length > 0 && (
            <p>
              <button
                type="button"
                className="inline secondary"
                disabled={refund.busy}
                onClick={() => setCancelling([])}
              >
                Cancel order
              </button>
            </p>
          )}
          {cancelling && (
            <CancelDialog
              order={order}
              path={path}
              ids={cancelling}
              onClose={() => setCancelling(null)}
              onDone={async () => {
                setCancelling(null);
                setPicked([]);
                await reload();
              }}
            />
          )}

          {canEdit(org) && order.source === "online" && valid.length > 0 && (
            <div className="status-box">
              <label>
                <input
                  type="checkbox"
                  checked={feeBack}
                  disabled={refund.busy}
                  onChange={(e) => change(() => setFeeBack(e.target.checked))}
                />{" "}
                {order.fee_mode === "absorb"
                  ? "Give back our fee on these tickets too (it comes back to you, not the buyer)"
                  : "Give back our service fee too"}
              </label>
              {asking ? (
                <>
                  <p id="refund-question">
                    {preview?.amountCents
                      ? `Refund ${money} to the card they paid with? Their tickets stop working straight away. This can't be undone.`
                      : "Cancel these free tickets? They stop working straight away. This can't be undone."}
                  </p>
                  <div className="row">
                    <button
                      type="button"
                      className="inline"
                      ref={focusOnMount}
                      aria-describedby="refund-question"
                      disabled={refund.busy}
                      onClick={confirm}
                    >
                      Yes, refund
                    </button>
                    <button
                      type="button"
                      className="inline secondary"
                      onClick={() => change(() => setAsking(false))}
                    >
                      Keep it
                    </button>
                  </div>
                </>
              ) : (
                <button
                  type="button"
                  className="inline"
                  ref={asking === false ? focusOnMount : undefined}
                  onClick={() => setAsking(true)}
                >
                  {label} · {money}
                </button>
              )}
              <ErrorText>{refund.error}</ErrorText>
            </div>
          )}

          {(org.role === "owner" || org.role === "admin") && event && (
            <Notes org={org} path={path} timezone={event.timezone} />
          )}

          {order.refunds.length > 0 && (
            <>
              <h2>Refunds</h2>
              <ul className="plain">
                {order.refunds.map((r) => (
                  <RefundRow
                    key={r.id}
                    r={r}
                    canRetry={canEdit(org)}
                    eventCancelled={event?.status === "cancelled"}
                    onRetried={reload}
                    org={org.slug}
                    ev={eventId}
                    order={orderId}
                  />
                ))}
              </ul>
            </>
          )}
        </>
      )}
    </>
  );
}
