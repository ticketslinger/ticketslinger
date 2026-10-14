import type { components } from "@ticketslinger/api-client";
import { useEffect, useState } from "react";
import { ApiFailure, api, must, session } from "../api";
import { ErrorText, Link, type Me, useAction } from "../ui";

type Organizer = components["schemas"]["Organizer"];

const ROLE = { owner: "Owner", admin: "Admin", checkin: "Check-in" } as const;

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
      {!me.mfa &&
        orgs?.some((o) => o.role !== "checkin") &&
        (me.totp_enabled ? (
          <div className="notice">
            <p>You've set up a second step. Sign in again with it to manage this organizer.</p>
            <button type="button" disabled={signOut.busy} onClick={signOutNow}>
              Sign in again
            </button>
          </div>
        ) : (
          <p className="notice">
            Add a passkey or an authenticator app to manage this organizer.{" "}
            <Link href="/security">Set one up</Link>
          </p>
        ))}
      <ErrorText>{loadError}</ErrorText>
      {orgs === null && !loadError && <p className="muted">Loading…</p>}
      {orgs?.length === 0 && <p>You're not part of an organizer yet. Create one to get going.</p>}
      {orgs && orgs.length > 0 && (
        <ul className="orgs">
          {orgs.map((o) => (
            <li key={o.id}>
              <strong>{o.name}</strong> <span className="muted">· {ROLE[o.role]}</span>
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
      </p>
    </main>
  );
}
