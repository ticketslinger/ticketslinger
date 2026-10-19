import type { components } from "@ticketslinger/api-client";
import { api, must } from "../api";
import { ErrorText, type Me, useLoad } from "../ui";
import type { Org } from "./Organizer";

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
};
const DONE: Record<string, string> = {
  created: "created",
  updated: "changed",
  deleted: "deleted",
  revoked: "revoked",
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

/** "ticket_type.updated" → "Ticket type changed"; anything unfamiliar shows as it is. */
export function describeAction(action: string): string {
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
>;

/**
 * The Who cell: a key by name, you as "You", a member by name with their
 * email beneath, and "Automatic" for what TicketSlinger did by itself.
 */
export function who(e: Actor, meId: string) {
  if (e.actor_type === "system") return "Automatic";
  if (e.actor_api_key_id) return e.actor_name ? `API key: ${e.actor_name}` : "An API key";
  if (e.actor_user_id === meId) return "You";
  return e.actor_name ?? "Someone on your team";
}

export function Audit({ org, me }: { org: Org; me: Me }) {
  const path = { organizer: org.slug };
  const { data, error } = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/audit", { params: { path } })),
  );

  return (
    <>
      <h1 tabIndex={-1}>Audit log</h1>
      <p className="lede">The latest changes to {org.name}, newest first. We show up to 100.</p>
      <ErrorText>{error?.message}</ErrorText>
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
                  <td>{describeAction(e.action)}</td>
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
    </>
  );
}
