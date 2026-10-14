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
};
const DONE: Record<string, string> = {
  created: "created",
  updated: "changed",
  deleted: "deleted",
  revoked: "revoked",
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

export function Audit({ org, me }: { org: Org; me: Me }) {
  const path = { organizer: org.slug };
  const { data, error } = useLoad(async () => {
    const [log, keys] = await Promise.all([
      api.GET("/organizers/{organizer}/audit", { params: { path } }),
      api.GET("/organizers/{organizer}/api-keys", { params: { path } }),
    ]);
    return { log: must(log), keys: new Map(must(keys).map((k) => [k.id, k.name])) };
  });

  const who = (e: { actor_user_id?: string; actor_api_key_id?: string }) => {
    if (e.actor_api_key_id) {
      const name = data?.keys.get(e.actor_api_key_id);
      return name ? `API key “${name}”` : "An API key";
    }
    return e.actor_user_id === me.id ? "You" : "Someone on your team";
  };

  return (
    <>
      <h1 tabIndex={-1}>Audit log</h1>
      <p className="lede">The latest changes to {org.name}, newest first. We show up to 100.</p>
      <ErrorText>{error?.message}</ErrorText>
      {!data && !error && <p className="muted">Loading…</p>}
      {data?.log.length === 0 && <p>Nothing has changed yet.</p>}
      {data && data.log.length > 0 && (
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
              {data.log.map((e) => (
                <tr key={e.id}>
                  <td>{when(e.at)}</td>
                  <td>{describeAction(e.action)}</td>
                  <td>{who(e)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
