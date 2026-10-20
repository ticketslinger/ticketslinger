import type { components } from "@ticketslinger/api-client";
import { useState } from "react";
import { api, must, touchPasskey } from "../api";
import { FormDialog } from "../forms";
import { formatCents } from "../money";
import { ErrorText, Field, useAction, useLoad } from "../ui";
import { canCancel } from "./CancelEvent";
import { ORG_CHANGED } from "./Organizer";
import { reasonOk } from "./Staff";

type Kind = components["schemas"]["StaffActionKind"];
type Result = components["schemas"]["StaffRefundResult"];

/** The spec's dialog sentences (§13.7). */
export const actionText = (kind: Kind, organizer: string, event = "") =>
  kind === "suspend"
    ? `Suspend ${organizer}? Every event goes off sale now, and their team can't change anything until you lift it. Paid orders stay as they are.`
    : kind === "lift"
      ? `Lift the suspension of ${organizer}? Their events can sell again and their team can change things.`
      : `Refund every order for ${event}? Each buyer gets all their money back, our fee included. You can't undo this.`;

/** The confirm button: a good reason, and for a refund the event's name typed out. */
export const canConfirm = (kind: Kind, reason: string, typed: string, eventName: string) =>
  reasonOk(reason) && (kind !== "refund_event" || canCancel(typed, eventName));

export const resultText = (r: Result) =>
  `Refunded ${r.orders} order${r.orders === 1 ? "" : "s"}, ${formatCents(r.amount_cents)} in all, ${formatCents(r.fee_cents)} of it our fee. ${r.pending_orders} unfinished payment${r.pending_orders === 1 ? "" : "s"} stopped.`;

/** Totals, and any orders the database refused, which staff have to refund by hand. */
export function RefundResult({ result }: { result: Result }) {
  const failed = result.failed_orders ?? [];
  return (
    <div role="status">
      <p>{resultText(result)}</p>
      {failed.length > 0 && (
        <>
          <p>Refund these by hand:</p>
          <ul>
            {failed.map((f) => (
              <li key={f.order_id}>
                {f.order_id}: {f.reason}
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

const KIND: Record<Kind, string> = {
  suspend: "Suspended the organizer",
  lift: "Lifted the suspension",
  refund_event: "Refunded every order of an event",
};

/** The suspensions, lifts and refunds taken in one support session. */
export function ActionLog({ actions }: { actions: components["schemas"]["StaffActionLog"][] }) {
  if (actions.length === 0) return null;
  return (
    <ul>
      {actions.map((a) => (
        <li key={a.id}>
          {KIND[a.kind]} <span className="muted">· {new Date(a.at).toLocaleString()}</span>
          <br />"{a.reason}"
          {a.kind === "refund_event" && !a.finished_at && (
            <p className="muted">Not finished. Take it again to finish the refunds.</p>
          )}
          {a.result && <RefundResult result={a.result} />}
        </li>
      ))}
    </ul>
  );
}

export function ActionDialog({
  kind,
  organizer,
  event,
  busy,
  error,
  onClose,
  onConfirm,
}: {
  kind: Kind;
  organizer: string;
  event?: { id: string; name: string };
  busy: boolean;
  error: string;
  onClose: () => void;
  onConfirm: (reason: string) => void;
}) {
  const [reason, setReason] = useState("");
  const [typed, setTyped] = useState("");
  return (
    <FormDialog
      title={
        kind === "suspend" ? "Suspend" : kind === "lift" ? "Lift suspension" : "Refund every order"
      }
      dirty={reason !== "" || typed !== ""}
      onClose={onClose}
      busy={busy || !canConfirm(kind, reason, typed, event?.name ?? "")}
      submitLabel="Touch your passkey to confirm"
      onSubmit={() => onConfirm(reason.trim())}
    >
      <p>{actionText(kind, organizer, event?.name)}</p>
      {kind === "refund_event" && (
        <Field
          id="confirm-event-name"
          label="Type the event's name to confirm"
          value={typed}
          autoComplete="off"
          onChange={(e) => setTyped(e.target.value)}
        />
      )}
      <div className="field">
        <label htmlFor="action-reason">Reason</label>
        <textarea
          id="action-reason"
          value={reason}
          maxLength={500}
          onChange={(e) => setReason(e.target.value)}
        />
        <p className="hint">10 to 500 characters. Their owners will read this.</p>
      </div>
      <ErrorText>{error}</ErrorText>
    </FormDialog>
  );
}

/** Staff mode's takedown controls: suspend or lift, and once suspended, refund an event. */
export function StaffActions({ slug, name }: { slug: string; name: string }) {
  const org = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}", { params: { path: { organizer: slug } } })),
  );
  const suspended = org.data?.suspended === true;
  // Always fetched: reload() runs the last render's loader, which would still say "not suspended"
  // right after a suspend and leave the list empty.
  const events = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events", { params: { path: { organizer: slug } } }),
    ),
  );
  const [open, setOpen] = useState<{ kind: Kind; event?: { id: string; name: string } } | null>(
    null,
  );
  const [result, setResult] = useState<Result>();
  const action = useAction();

  const confirm = (reason: string) =>
    open &&
    action.run(async () => {
      const body = { kind: open.kind, event: open.event?.id };
      const { challenge_id, options } = must(await api.POST("/staff/actions/challenge", { body }));
      const credential = await touchPasskey(options);
      const done = must(
        await api.POST("/staff/actions", {
          body: {
            ...body,
            challenge_id,
            credential: credential as unknown as Record<string, unknown>,
            reason,
          },
        }),
      );
      setOpen(null);
      if (done.result) setResult(done.result);
      window.dispatchEvent(new Event(ORG_CHANGED));
      await org.reload();
      await events.reload();
    });

  return (
    <details className="staff-actions">
      <summary>Actions</summary>
      <ErrorText>{org.error?.message}</ErrorText>
      {org.data && (
        <button type="button" onClick={() => setOpen({ kind: suspended ? "lift" : "suspend" })}>
          {suspended ? "Lift suspension" : "Suspend organizer"}
        </button>
      )}
      {suspended && (
        <ul>
          {(events.data ?? []).map((e) => (
            <li key={e.id}>
              {e.name}{" "}
              <button type="button" onClick={() => setOpen({ kind: "refund_event", event: e })}>
                Refund every order
              </button>
            </li>
          ))}
        </ul>
      )}
      {result && <RefundResult result={result} />}
      {open && (
        <ActionDialog
          kind={open.kind}
          organizer={name}
          event={open.event}
          busy={action.busy}
          error={action.error}
          onClose={() => {
            setOpen(null);
            action.clear();
          }}
          onConfirm={confirm}
        />
      )}
    </details>
  );
}
