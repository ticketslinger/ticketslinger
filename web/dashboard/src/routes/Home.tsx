import type { components } from "@ticketslinger/api-client";
import { useEffect, useState } from "react";
import { ApiFailure, api, must, session } from "../api";
import { ErrorText, Link, type Me, useAction } from "../ui";

type Organizer = components["schemas"]["Organizer"];

export const ROLE = {
  owner: "Owner",
  admin: "Admin",
  checkin: "Check-in",
  "key:read": "API key, read only",
  "key:write": "API key",
  staff: "TicketSlinger support",
} as const;

/** The account links beyond the basics: Support is for staff only. */
export const menuItems = (me: Pick<Me, "staff">) => (me.staff ? ["Support"] : []);

export function Home({ me, onSignedOut }: { me: Me; onSignedOut: () => void }) {
  const [orgs, setOrgs] = useState<Organizer[] | null>(null);
  const [loadError, setLoadError] = useState("");
  const signOut = useAction();
  const signOutNow = () =>
    signOut.run(async () => {
      await session.logout();
      onSignedOut();
    });

  useEffect(() => {
    api
      .GET("/organizers")
      .then((r) => setOrgs(must(r)))
      .catch((e) =>
        setLoadError(
          e instanceof ApiFailure ? e.message : "We couldn't load your organizers. Try reloading.",
        ),
      );
  }, []);

  return (
    <main>
      <header className="top">
        <p className="muted">Signed in as {me.email}</p>
        <button type="button" className="secondary" disabled={signOut.busy} onClick={signOutNow}>
          Sign out
        </button>
      </header>
      <ErrorText>{signOut.error}</ErrorText>

      <h1 tabIndex={-1}>Hi, {me.name.trim().split(/\s+/)[0]}</h1>

      {!me.email_verified && (
        <p className="notice">
          Confirm your email to create an organizer. We sent the link to {me.email}.
        </p>
      )}

      <h2>Your organizers</h2>
      {!me.mfa && orgs?.some((o) => o.role === "owner" || o.role === "admin") && (
        <SecondStepNotice me={me} onSignedOut={onSignedOut} />
      )}
      <ErrorText>{loadError}</ErrorText>
      {orgs === null && !loadError && <p className="muted">Loading…</p>}
      {orgs?.length === 0 && <p>You're not part of an organizer yet. Create one to get going.</p>}
      {orgs && orgs.length > 0 && (
        <ul className="orgs">
          {orgs.map((o) => (
            <li key={o.id}>
              <Link href={`/o/${o.slug}`}>
                <strong>{o.name}</strong>
              </Link>{" "}
              <span className="muted">· {ROLE[o.role]}</span>
            </li>
          ))}
        </ul>
      )}

      <p className="actions">
        <Link className="button" href="/organizers/new">
          Create an organizer
        </Link>
        <Link className="button secondary" href="/security">
          Sign-in and security
        </Link>
        {menuItems(me).includes("Support") && (
          <Link className="button secondary" href="/staff">
            Support
          </Link>
        )}
      </p>
    </main>
  );
}

/** Owners and admins need a second step this session before they can manage an organizer. */
export function SecondStepNotice({ me, onSignedOut }: { me: Me; onSignedOut: () => void }) {
  const signOut = useAction();
  if (!me.has_second_factor) {
    return (
      <p className="notice">
        Add a passkey or an authenticator app to manage this organizer.{" "}
        <Link href="/security">Set one up</Link>
      </p>
    );
  }
  return (
    <div className="notice">
      <p>You've set up a second step. Sign in again with it to manage this organizer.</p>
      <button
        type="button"
        disabled={signOut.busy}
        onClick={() =>
          signOut.run(async () => {
            await session.logout();
            onSignedOut();
          })
        }
      >
        Sign in again
      </button>
      <ErrorText>{signOut.error}</ErrorText>
    </div>
  );
}
