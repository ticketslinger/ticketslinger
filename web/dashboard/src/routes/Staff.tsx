import type { components } from "@ticketslinger/api-client";
import { useState } from "react";
import { api, must, touchPasskey } from "../api";
import { ErrorText, Field, Link, navigate, useAction, useLoad } from "../ui";

type Log = components["schemas"]["StaffSessionLog"];

export const REASON_MIN = 10;
export const REASON_MAX = 500;
export const reasonOk = (reason: string) => {
  const n = reason.trim().length;
  return n >= REASON_MIN && n <= REASON_MAX;
};

/** The spec's sentence for an organizer's audit page. */
export const staffAuditLine = (name: string, reason: string) =>
  `TicketSlinger support (${name}) viewed your organizer: "${reason}"`;

const ENDED: Record<string, string> = {
  staff: "ended by support",
  logout: "ended by sign-out",
  revoked: "ended when access was taken away",
  expired: "ran out of time",
};

const when = (iso: string) =>
  new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(
    new Date(iso),
  );

/** Staff sessions with the requests made in each. */
export function SessionList({
  sessions,
  showOrganizer,
}: {
  sessions: Log[];
  showOrganizer: boolean;
}) {
  if (sessions.length === 0) return <p>No support sessions yet.</p>;
  return (
    <ul className="orgs">
      {sessions.map((s) => (
        <li key={s.id}>
          <strong>{s.staff_name}</strong>
          {showOrganizer && <> on {s.organizer_name}</>}{" "}
          <span className="muted">
            · {when(s.started_at)} · {s.ended_how ? ENDED[s.ended_how] : "still running"}
          </span>
          <br />
          <span>"{s.reason}"</span>
          {s.end_note && <p className="muted">Note: {s.end_note}</p>}
          <details>
            <summary>
              {s.requests.length} {s.requests.length === 1 ? "request" : "requests"}
            </summary>
            <ul>
              {s.requests.map((r) => (
                <li key={`${r.at}-${r.method}-${r.route}`}>
                  {when(r.at)} {r.method} {r.route}
                </li>
              ))}
            </ul>
          </details>
        </li>
      ))}
    </ul>
  );
}

export function Staff({ onStarted }: { onStarted: () => Promise<void> }) {
  const [q, setQ] = useState("");
  const [found, setFound] = useState<components["schemas"]["StaffOrganizer"][]>([]);
  const [picked, setPicked] = useState("");
  const [reason, setReason] = useState("");
  const search = useAction();
  const start = useAction();
  const { data: sessions, error } = useLoad(async () => must(await api.GET("/staff/sessions")));

  return (
    <main className="wide">
      <p className="crumbs">
        <Link href="/">Your organizers</Link>
      </p>
      <h1 tabIndex={-1}>Support</h1>
      <p className="lede">
        Look at one organizer, read-only, for up to an hour. The reason you give goes to their
        owners and to us.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          search.run(async () => {
            setPicked("");
            setFound(
              must(await api.GET("/staff/organizers", { params: { query: { q: q.trim() } } })),
            );
          });
        }}
      >
        <Field
          id="staff-q"
          label="Find an organizer"
          hint="Part of the name or the web address."
          value={q}
          onChange={(e) => setQ(e.target.value)}
          minLength={2}
          maxLength={120}
          required
        />
        <button type="submit" className="secondary" disabled={search.busy}>
          Search
        </button>
        <ErrorText>{search.error}</ErrorText>
      </form>
      {found.length > 0 && (
        <fieldset>
          <legend>Pick one</legend>
          {found.map((o) => (
            <label key={o.slug} className="check">
              <input
                type="radio"
                name="staff-org"
                checked={picked === o.slug}
                onChange={() => setPicked(o.slug)}
              />
              {o.name} <span className="muted">· {o.slug}</span>
            </label>
          ))}
        </fieldset>
      )}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          start.run(async () => {
            const { challenge_id, options } = must(await api.POST("/staff/sessions/challenge"));
            const credential = await touchPasskey(options);
            must(
              await api.POST("/staff/sessions", {
                body: {
                  challenge_id,
                  credential: credential as unknown as Record<string, unknown>,
                  organizer: picked,
                  reason: reason.trim(),
                },
              }),
            );
            await onStarted(); // /me shows the new session, so the banner is up on arrival
            navigate(`/o/${encodeURIComponent(picked)}`, false, true);
          });
        }}
      >
        <div className="field">
          <label htmlFor="staff-reason">Why do you need to look?</label>
          <textarea
            id="staff-reason"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            maxLength={REASON_MAX}
            aria-describedby="staff-reason-hint"
          />
          <p className="hint" id="staff-reason-hint">
            {REASON_MIN} to {REASON_MAX} characters. Their owners will read this.
          </p>
        </div>
        <ErrorText>{start.error}</ErrorText>
        <button type="submit" disabled={start.busy || !picked || !reasonOk(reason)}>
          Touch your passkey to start
        </button>
      </form>

      <h2>Support sessions</h2>
      <ErrorText>{error?.message}</ErrorText>
      {!sessions && !error && <p className="muted">Loading…</p>}
      {sessions && <SessionList sessions={sessions} showOrganizer />}
    </main>
  );
}
