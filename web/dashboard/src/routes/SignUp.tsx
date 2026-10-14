import { useState } from "react";
import { checkPending, session } from "../api";
import { useTurnstile } from "../turnstile";
import { ErrorText, Field, Link, useAction } from "../ui";

const MIN = 12;

export function SignUp({ onSignedIn }: { onSignedIn: () => void }) {
  const [password, setPassword] = useState("");
  const [sentTo, setSentTo] = useState("");
  const { busy, error, run } = useAction();
  const check = useTurnstile(true);

  if (sentTo) {
    return (
      <main>
        <h1 tabIndex={-1}>Check your inbox</h1>
        <p>
          We've sent a link to <strong>{sentTo}</strong>. Open it to confirm your email, and you can
          set up your organizer straight after.
        </p>
        <p className="muted">Nothing there? Give it a minute, and check your spam folder.</p>
        <button type="button" className="secondary" onClick={onSignedIn}>
          Go to your dashboard
        </button>
      </main>
    );
  }

  const left = MIN - password.length;
  return (
    <main>
      <h1 tabIndex={-1}>Create your account</h1>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          const f = new FormData(e.currentTarget);
          const email = String(f.get("email"));
          run(async () => {
            if (check.shown && !check.token) throw checkPending();
            try {
              await session.signup(String(f.get("name")), email, password, check.token);
            } catch (err) {
              check.reset(); // the token is spent either way
              throw err;
            }
            setSentTo(email);
          });
        }}
      >
        <Field id="name" label="Name" autoComplete="name" required maxLength={120} />
        <Field id="email" label="Email" type="email" autoComplete="email" required />
        <Field
          id="password"
          label="Password"
          type="password"
          autoComplete="new-password"
          required
          minLength={MIN}
          maxLength={256}
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          hint={left > 0 ? `At least ${MIN} characters. ${left} to go.` : "That's long enough."}
        />
        {check.shown && (
          <div className="field">
            <p className="hint">One quick check that you're a person.</p>
            <div ref={check.ref} />
          </div>
        )}
        <ErrorText>{error}</ErrorText>
        <button type="submit" disabled={busy}>
          Create account
        </button>
      </form>
      <p className="muted">
        Already have an account? <Link href="/signin">Sign in</Link>
      </p>
    </main>
  );
}
