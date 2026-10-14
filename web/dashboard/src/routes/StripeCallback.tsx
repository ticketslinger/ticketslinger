import { useEffect, useRef, useState } from "react";
import { ApiFailure, api, must } from "../api";
import { Link, navigate } from "../ui";

export type Outcome =
  | { kind: "complete"; state: string; code: string }
  | { kind: "declined" }
  | { kind: "bad" };

/** What Stripe's OAuth page sent back: a code to finish with, a "no thanks", or nothing usable. */
export function callbackOutcome(search: string): Outcome {
  const q = new URLSearchParams(search);
  if (q.get("error") === "access_denied") return { kind: "declined" };
  const state = q.get("state") ?? "";
  const code = q.get("code") ?? "";
  if (q.has("error") || !state || !code) return { kind: "bad" };
  return { kind: "complete", state, code };
}

const expired = "That link has expired. Start again from your organizer's Payments page.";

export function StripeCallback() {
  const [outcome] = useState(() => callbackOutcome(location.search));
  const [error, setError] = useState("");
  const started = useRef(false);

  useEffect(() => {
    // The code and state are single-use: keep them out of history, and make
    // sure StrictMode's double effect doesn't spend them twice.
    if (started.current) return;
    started.current = true;
    history.replaceState(null, "", location.pathname);
    if (outcome.kind !== "complete") return;
    api
      .POST("/stripe/link/complete", { body: { state: outcome.state, code: outcome.code } })
      .then((r) => {
        const { organizer } = must(r);
        navigate(`/o/${encodeURIComponent(organizer)}/payments?return=1`, true);
      })
      .catch((e) =>
        setError(
          e instanceof ApiFailure
            ? e.code === "invalid_state"
              ? expired
              : e.message
            : "We couldn't reach TicketSlinger. Check your connection, then start again from your organizer's Payments page.",
        ),
      );
  }, [outcome]);

  if (outcome.kind === "complete" && !error) {
    return (
      <main>
        <h1 tabIndex={-1}>Linking your Stripe account…</h1>
      </main>
    );
  }
  return (
    <main>
      <h1 tabIndex={-1}>
        {outcome.kind === "declined" ? "No account linked" : "That didn't link"}
      </h1>
      <p role="alert">
        {outcome.kind === "declined"
          ? "No account was linked, and nothing changed."
          : outcome.kind === "bad"
            ? expired
            : error}
      </p>
      <Link href="/">Go to your dashboard</Link>
    </main>
  );
}
