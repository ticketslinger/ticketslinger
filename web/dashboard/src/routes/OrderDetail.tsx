import type { components } from "@ticketslinger/api-client";
import { useRef, useState } from "react";
import { api, must } from "../api";
import { toggle } from "../forms";
import { withKey } from "../idempotency";
import { formatCents } from "../money";
import { formatWhen } from "../time";
import { ErrorText, focusOnMount, Link, useAction, useLoad } from "../ui";
import { canEdit, type Org } from "./Organizer";

type Detail = components["schemas"]["OrderDetail"];
type Refund = components["schemas"]["Refund"];

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
  const refund = useAction();
  const key = useRef<string | null>(null);

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
            {order.email}
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
              <li key={t.id}>
                {t.status === "valid" && canEdit(org) ? (
                  <label>
                    <input
                      type="checkbox"
                      checked={picked.includes(t.id)}
                      disabled={refund.busy}
                      onChange={() => change(() => setPicked(toggle(picked, t.id)))}
                    />{" "}
                    {t.ticket_type_name}, {t.attendee_name}
                  </label>
                ) : (
                  <>
                    {t.ticket_type_name}, {t.attendee_name}
                  </>
                )}
                {t.status === "void" && (
                  <>
                    {" "}
                    <span className={`pill ${refundOf(t.refund_id)?.status ?? ""}`}>
                      {voidLabel(t.refund_id)}
                    </span>
                  </>
                )}
              </li>
            ))}
          </ul>

          {canEdit(org) && valid.length > 0 && (
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
