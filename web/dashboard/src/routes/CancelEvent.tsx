import { useRef, useState } from "react";
import { api, must } from "../api";
import { FormDialog } from "../forms";
import { withKey } from "../idempotency";
import { formatCents } from "../money";
import { ErrorText, Field, useAction } from "../ui";
import type { Event } from "./EventForm";
import type { Org } from "./Organizer";

/** The typed name has to match the event's, spaces around it aside. */
export const canCancel = (typed: string, name: string) =>
  typed.trim() !== "" && typed.trim() === name.trim();

/** The danger button on a live event's Details tab, its confirm dialog, and the result afterwards. */
export function CancelEvent({
  org,
  event,
  onCancelled,
}: {
  org: Org;
  event: Event;
  onCancelled: (e: Event) => void;
}) {
  const [open, setOpen] = useState(false);
  const [typed, setTyped] = useState("");
  const [done, setDone] = useState("");
  const action = useAction();
  // One key per confirmed press; a retry after a dropped connection reuses it.
  const key = useRef<string | null>(null);

  if (done)
    return (
      <section className="danger">
        <p role="status">{done}</p>
      </section>
    );
  if (event.status !== "live") return null;

  const submit = () =>
    action.run(async () => {
      const r = await withKey(key, async (k) =>
        must(
          await api.POST("/organizers/{organizer}/events/{event}/cancel", {
            params: {
              path: { organizer: org.slug, event: event.id },
              header: { "Idempotency-Key": k },
            },
            body: { confirm_name: typed },
          }),
        ),
      );
      setOpen(false);
      setDone(
        `Cancelled. We're refunding ${r.orders} order${r.orders === 1 ? "" : "s"}, ${formatCents(r.amount_cents)} in all.`,
      );
      onCancelled(r.event);
    });

  return (
    <section className="danger">
      <button type="button" className="inline danger-button" onClick={() => setOpen(true)}>
        Cancel this event
      </button>
      {open && (
        <FormDialog
          title={`Cancel ${event.name}?`}
          dirty={typed !== ""}
          onClose={() => {
            setOpen(false);
            setTyped("");
            key.current = null;
            action.clear();
          }}
          onSubmit={submit}
          busy={action.busy || !canCancel(typed, event.name)}
          submitLabel="Cancel the event and refund everyone"
        >
          <p>
            Everyone who bought a ticket gets their money back, our service fee included, and every
            ticket stops working. Nobody can buy from now on. This can't be undone.
          </p>
          <Field
            id="confirm-name"
            label="Type the event's name to confirm"
            value={typed}
            autoComplete="off"
            onChange={(e) => setTyped(e.target.value)}
          />
          <ErrorText>{action.error}</ErrorText>
        </FormDialog>
      )}
    </section>
  );
}
