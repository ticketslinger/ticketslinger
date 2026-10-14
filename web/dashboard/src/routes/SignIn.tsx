import { useState } from "react";
import { ApiFailure, checkPending, session } from "../api";
import { useTurnstile } from "../turnstile";
import { ErrorText, Field, Link, useAction } from "../ui";

export function SignIn({ onSignedIn }: { onSignedIn: () => void }) {
  const [needCode, setNeedCode] = useState(false);
  const [useRecovery, setUseRecovery] = useState(false);
  // After a few wrong passwords the API asks for a Turnstile check.
  const [needCheck, setNeedCheck] = useState(false);
  const check = useTurnstile(needCheck);
  const { busy, error, run } = useAction();

  const withPasskey = () =>
    run(async () => {
      await session.passkeyLogin();
      onSignedIn();
    });

  return (
    <main>
      <h1 tabIndex={-1}>Sign in</h1>
      <button type="button" onClick={withPasskey} disabled={busy}>
        Sign in with a passkey
      </button>
      <p className="divider">or use your password</p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          const f = new FormData(e.currentTarget);
          run(async () => {
            if (check.shown && !check.token) throw checkPending();
            try {
              await session.login(
                String(f.get("email")),
                String(f.get("password")),
                needCode && !useRecovery ? String(f.get("code")) : undefined,
                useRecovery ? String(f.get("recovery")) : undefined,
                check.shown ? check.token : undefined,
              );
            } catch (err) {
              if (check.shown) check.reset(); // the token is spent
              if (err instanceof ApiFailure && err.code === "turnstile_required")
                setNeedCheck(true);
              if (err instanceof ApiFailure && err.code === "totp_required" && !needCode) {
                setNeedCode(true);
                return;
              }
              throw err;
            }
            onSignedIn();
          });
        }}
      >
        <Field id="email" label="Email" type="email" autoComplete="username webauthn" required />
        <Field
          id="password"
          label="Password"
          type="password"
          autoComplete="current-password"
          required
        />
        {useRecovery && (
          <Field
            id="recovery"
            label="Recovery code"
            hint="Each recovery code works once. Type one of the codes you saved, like K7QM-4XWD-9TBR."
            autoComplete="off"
            autoCapitalize="characters"
            spellCheck={false}
            maxLength={32}
            required
            autoFocus
          />
        )}
        {needCode && !useRecovery && (
          <Field
            id="code"
            label="6-digit code"
            hint="Open your authenticator app and type the code it shows for TicketSlinger."
            autoComplete="one-time-code"
            inputMode="numeric"
            pattern="[0-9]{6}"
            maxLength={6}
            required
            autoFocus
          />
        )}
        {check.shown && (
          <div className="field">
            <p className="hint">One quick check that you're a person.</p>
            <div ref={check.ref} />
          </div>
        )}
        <ErrorText>{error}</ErrorText>
        <button type="submit" disabled={busy}>
          Sign in
        </button>
        <button type="button" className="secondary" onClick={() => setUseRecovery(!useRecovery)}>
          {useRecovery
            ? needCode
              ? "Use my authenticator app instead"
              : "I don't need a recovery code"
            : "Use a recovery code instead"}
        </button>
      </form>
      <p className="muted">
        New here? <Link href="/signup">Create an account</Link>
      </p>
    </main>
  );
}
