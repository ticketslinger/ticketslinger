import { useEffect, useRef, useState } from "react";
import { ApiFailure, api, must } from "../api";
import { Link } from "../ui";

type State = "checking" | "done" | { error: string };

export function VerifyEmail({ onVerified }: { onVerified: () => void }) {
  const [state, setState] = useState<State>("checking");
  const started = useRef(false);

  useEffect(() => {
    // Tokens are single-use, so make sure StrictMode's double effect doesn't spend it twice.
    if (started.current) return;
    started.current = true;
    const token = new URLSearchParams(location.search).get("token") ?? "";
    if (!token) {
      setState({ error: "This link is missing its code. Open the newest email we sent you." });
      return;
    }
    api
      .POST("/auth/verify-email", { body: { token } })
      .then((r) => {
        must(r);
        setState("done");
        onVerified();
      })
      .catch((e) =>
        setState({
          error:
            e instanceof ApiFailure
              ? e.message
              : "We couldn't reach TicketSlinger. Check your connection and reload this page.",
        }),
      );
  }, [onVerified]);

  if (state === "checking") {
    return (
      <main>
        <h1 tabIndex={-1}>Checking your link…</h1>
      </main>
    );
  }
  if (state === "done") {
    return (
      <main>
        <h1 tabIndex={-1}>Your email is verified</h1>
        <p>Thanks. Next up: tell us who's putting on the events.</p>
        <Link className="button" href="/organizers/new">
          Create your organizer
        </Link>
      </main>
    );
  }
  return (
    <main>
      <h1 tabIndex={-1}>That link didn't work</h1>
      <p role="alert">{state.error}</p>
      <Link href="/">Go to your dashboard</Link>
    </main>
  );
}
