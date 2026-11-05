import type { components } from "@ticketslinger/api-client";
import { useRef, useState } from "react";
import { ApiFailure, api, must } from "../api";
import { withKey } from "../idempotency";
import { ErrorText, Field, focusOnMount, useAction, useLoad } from "../ui";
import { ROLE } from "./Home";
import { canEdit, type Org } from "./Organizer";

type Member = components["schemas"]["Member"];
type Role = components["schemas"]["AddMember"]["role"];

export const ROLE_CHOICES: Record<Role, string> = {
  checkin: "Check-in: scans tickets and finds guests",
  admin: "Admin: runs events, refunds and reports",
};

/** The team's own answers, word for word, whatever the server's wording. */
export const TEAM_ERRORS: Record<string, string> = {
  no_account:
    "There's no TicketSlinger account with that email yet. Ask them to sign up at app.ticketslinger.com, then add them here.",
  already_member: "They're already on your team.",
  cannot_remove: "Owners can't be removed here, and you can't remove yourself.",
};

export const removeWarning = (org: string) =>
  `They lose access to ${org} straight away, and any API keys they made here stop working.`;

export function explain(e: unknown): unknown {
  if (!(e instanceof ApiFailure)) return e;
  const copy = TEAM_ERRORS[e.code];
  return copy ? new ApiFailure(e.code, copy, e.status, e.field) : e;
}

export function Team({ org }: { org: Org }) {
  const path = { organizer: org.slug };
  const {
    data: members,
    error,
    reload,
  } = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/members", { params: { path } })),
  );
  const owner = org.role === "owner" && !org.suspended;
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<Role>("checkin");
  const add = useAction();

  return (
    <>
      <h1 tabIndex={-1}>Team</h1>
      <p className="lede">
        {owner
          ? `Bring in the people who run ${org.name} with you. They sign up for TicketSlinger first, then you add them by that email. We don't send them anything, so let them know.`
          : `Everyone who works on ${org.name}. Only an owner can add or remove people.`}
      </p>
      <ErrorText>{error?.message}</ErrorText>
      {!members && !error && <p className="muted">Loading…</p>}
      {members && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Email</th>
                <th scope="col">Role</th>
                {owner && (
                  <th scope="col">
                    <span className="visually-hidden">Actions</span>
                  </th>
                )}
              </tr>
            </thead>
            <tbody>
              {members.map((m) => (
                <MemberRow key={m.user_id} m={m} org={org} owner={owner} onRemoved={reload} />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {owner && (
        <section>
          <h2>Add someone</h2>
          <form
            className="narrow"
            onSubmit={(e) => {
              e.preventDefault();
              add.run(async () => {
                try {
                  must(
                    await api.POST("/organizers/{organizer}/members", {
                      params: { path },
                      body: { email, role },
                    }),
                  );
                } catch (err) {
                  throw explain(err);
                }
                setEmail("");
                await reload();
              });
            }}
          >
            <Field
              id="member-email"
              label="Email"
              type="email"
              required
              maxLength={254}
              autoComplete="off"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
            <div className="field">
              <label htmlFor="member-role">Role</label>
              <select
                id="member-role"
                value={role}
                onChange={(e) => setRole(e.target.value as Role)}
              >
                {(["checkin", "admin"] as const).map((r) => (
                  <option key={r} value={r}>
                    {ROLE_CHOICES[r]}
                  </option>
                ))}
              </select>
            </div>
            <ErrorText>{add.error}</ErrorText>
            <button type="submit" className="inline" disabled={add.busy}>
              Add to team
            </button>
          </form>
        </section>
      )}

      {(org.role === "owner" || org.role === "admin") && <Notifications org={org} />}
    </>
  );
}

/** Each owner or admin's own order emails for this organizer. */
function Notifications({ org }: { org: Org }) {
  const path = { organizer: org.slug };
  const { data, setData, error } = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/notifications", { params: { path } })),
  );
  const save = useAction();
  const key = useRef<string | null>(null);
  return (
    <section>
      <h2>Notifications</h2>
      <label className="check">
        <input
          type="checkbox"
          aria-describedby="order-alerts-hint"
          checked={data?.order_alerts ?? false}
          disabled={!data || save.busy || !canEdit(org)}
          onChange={(e) => {
            const on = e.target.checked;
            key.current = null; // a new choice is a new request
            save.run(async () => {
              setData(
                await withKey(key, async (k) =>
                  must(
                    await api.PUT("/organizers/{organizer}/notifications", {
                      params: { path, header: { "Idempotency-Key": k } },
                      body: { order_alerts: on },
                    }),
                  ),
                ),
              );
            });
          }}
        />
        <span>Email me about each new online order</span>
      </label>
      <p className="hint" id="order-alerts-hint">
        At most 50 a day. Comp and manual orders don't send one.
      </p>
      <ErrorText>{error?.message || save.error}</ErrorText>
    </section>
  );
}

function MemberRow({
  m,
  org,
  owner,
  onRemoved,
}: {
  m: Member;
  org: Org;
  owner: boolean;
  onRemoved: () => void;
}) {
  // null until asked; false after "Keep them", so focus can go back to the first button.
  const [confirming, setConfirming] = useState<boolean | null>(null);
  const remove = useAction();
  return (
    <tr>
      <td>{m.name}</td>
      <td>{m.email}</td>
      <td>{ROLE[m.role]}</td>
      {owner && (
        <td className="actions-cell">
          {m.role !== "owner" &&
            (confirming ? (
              <>
                <p>{removeWarning(org.name)}</p>
                <div className="row">
                  <button
                    type="button"
                    className="inline"
                    ref={focusOnMount}
                    disabled={remove.busy}
                    onClick={() =>
                      remove.run(async () => {
                        try {
                          must(
                            await api.DELETE("/organizers/{organizer}/members/{member}", {
                              params: { path: { organizer: org.slug, member: m.user_id } },
                            }),
                          );
                        } catch (err) {
                          throw explain(err);
                        }
                        onRemoved();
                      })
                    }
                  >
                    Remove {m.name}
                  </button>
                  <button
                    type="button"
                    className="inline secondary"
                    onClick={() => setConfirming(false)}
                  >
                    Keep them
                  </button>
                </div>
              </>
            ) : (
              <button
                type="button"
                className="inline secondary"
                aria-label={`Remove ${m.name}`}
                ref={confirming === false ? focusOnMount : undefined}
                onClick={() => setConfirming(true)}
              >
                Remove
              </button>
            ))}
          <ErrorText>{remove.error}</ErrorText>
        </td>
      )}
    </tr>
  );
}
