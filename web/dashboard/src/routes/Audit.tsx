import type { components } from "@ticketslinger/api-client";
import { api, must } from "../api";
import { ErrorText, type Me, useLoad } from "../ui";
import type { Org } from "./Organizer";
import { SessionList, staffAuditLine } from "./Staff";

const THING: Record<string, string> = {
  organizer: "Organizer",
  event: "Event",
  ticket_type: "Ticket type",
  quota: "Capacity",
  promo_code: "Promo code",
  question: "Question",
  api_key: "API key",
  order: "Order",
  refund: "Refund",
  member: "Team member",
};
const DONE: Record<string, string> = {
  created: "created",
  updated: "changed",
  deleted: "deleted",
  revoked: "revoked",
  added: "added",
  removed: "removed",
  reordered: "reordered",
  refunded: "refunded",
  retried: "sent again",
  email_resent: "ticket email sent again",
  failed: "failed",
  cancelled: "cancelled",
  succeeded_late: "went through after all",
  fee_stuck: "stuck on our fee",
  cancelled_late: "cancelled (refund queued)",
  stripe_connected: "connected to Stripe",
  stripe_linked: "linked a Stripe account",
  stripe_disconnected: "disconnected from Stripe",
};

/** Rows that need a whole sentence rather than thing + verb. */
const SENTENCE: Record<string, string> = {
  "event.refunded_by_support": "Every order of an event refunded in full, our fee too",
};

/** "ticket_type.updated" → "Ticket type changed"; anything unfamiliar shows as it is. */
export function describeAction(action: string): string {
  if (SENTENCE[action]) return SENTENCE[action];
  const [thing = "", done = ""] = action.split(".");
  return THING[thing] && DONE[done] ? `${THING[thing]} ${DONE[done]}` : action;
}

const when = (iso: string) =>
  new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(
    new Date(iso),
  );

type Actor = Pick<
  components["schemas"]["AuditEntry"],
  "actor_type" | "actor_user_id" | "actor_api_key_id" | "actor_name" | "actor_email"
> & { action?: string };

/**
 * The Who cell: a key by name, you as "You", a member by name with their
 * email beneath, support by name, and "Automatic" for what TicketSlinger
 * did by itself (but a refund support asked for is support's).
 */
export function who(e: Actor, meId: string) {
  if (e.actor_type === "staff")
    return e.actor_name ? `TicketSlinger support (${e.actor_name})` : "TicketSlinger support";
  if (e.actor_type === "system")
    return e.action === "event.refunded_by_support" ? "TicketSlinger support" : "Automatic";
  if (e.actor_api_key_id) return e.actor_name ? `API key: ${e.actor_name}` : "An API key";
  if (e.actor_user_id === meId) return "You";
  return e.actor_name ?? "Someone on your team";
}

/** What support did, for rows the Who cell already says were support's. */
const STAFF_DID: Record<string, string> = {
  "staff.session_started": "Opened your organizer in support mode",
  "staff.suspended": "Suspended your organizer, so your events went off sale",
  "staff.lifted": "Lifted your organizer's suspension",
  "staff.refund_requested": "Started refunding every order of an event",
};

/** What a support row says; a start's reason lives on the matching staff session. */
export function staffRow(e: { action: string; actor_name?: string | null }, reason?: string) {
  const name = e.actor_name ?? "someone";
  if (e.action === "staff.session_started" && reason) return staffAuditLine(name, reason);
  if (e.action === "staff.session_ended")
    return `TicketSlinger support (${name}) ended support mode`;
  return STAFF_DID[e.action] ?? null;
}

export function Audit({ org, me }: { org: Org; me: Me }) {
  const path = { organizer: org.slug };
  const { data, error } = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/audit", { params: { path } })),
  );

  // Owners and admins only, which is who can see this page.
  const access = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/staff-access", { params: { path } })),
  );
  const reasons = new Map(access.data?.map((a) => [a.id, a.reason]));

  return (
    <>
      <h1 tabIndex={-1}>Audit log</h1>
      <p className="lede">The latest changes to {org.name}, newest first. We show up to 100.</p>
      <ErrorText>{error?.message}</ErrorText>
      <p>
        <a href="#support-access">See every support request</a>
      </p>
      {!data && !error && <p className="muted">Loading…</p>}
      {data?.length === 0 && <p>Nothing has changed yet.</p>}
      {data && data.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">When</th>
                <th scope="col">What</th>
                <th scope="col">Who</th>
              </tr>
            </thead>
            <tbody>
              {data.map((e) => (
                <tr key={e.id}>
                  <td>{when(e.at)}</td>
                  <td>
                    {(e.actor_type === "staff" && staffRow(e, reasons.get(e.entity_id))) ||
                      describeAction(e.action)}
                  </td>
                  <td>
                    {who(e, me.id)}
                    {e.actor_email && (
                      <>
                        <br />
                        <span className="muted">{e.actor_email}</span>
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <h2 id="support-access">TicketSlinger support access</h2>
      <p className="lede">
        Every time our support team has opened {org.name}, and what they looked at.
      </p>
      <ErrorText>{access.error?.message}</ErrorText>
      {access.data && <SessionList sessions={access.data} showOrganizer={false} />}
    </>
  );
}
