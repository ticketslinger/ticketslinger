import { startRegistration } from "@simplewebauthn/browser";
import qrcode from "qrcode-generator";
import { useEffect, useState } from "react";
import { ApiFailure, api, must, session } from "../api";
import { ErrorText, Field, Link, type Me, useAction, useLoad } from "../ui";

const GENERIC_PASSKEY_FAILURE =
  "Your device didn't save the passkey. Give it another go when you're ready.";

/** What to tell them when the browser refuses to make a passkey. InvalidStateError
 * means excludeCredentials matched: this device already holds one for the account.
 * Anything else (NotAllowedError is a cancel or a timeout) gets the generic line. */
export const passkeyFailureMessage = (e: unknown) =>
  e instanceof Error && e.name === "InvalidStateError"
    ? "This device already has a passkey for your account."
    : GENERIC_PASSKEY_FAILURE;

const day = (iso: string) =>
  new Intl.DateTimeFormat(undefined, { dateStyle: "medium" }).format(new Date(iso));

/** QR code as SVG elements: the CSP allows neither data: images nor injected markup. */
export function QrCode({ text, label }: { text: string; label: string }) {
  const qr = qrcode(0, "M");
  qr.addData(text);
  qr.make();
  const n = qr.getModuleCount();
  let d = "";
  for (let r = 0; r < n; r++)
    for (let c = 0; c < n; c++) if (qr.isDark(r, c)) d += `M${c} ${r}h1v1h-1z`;
  return (
    <svg className="qr" viewBox={`-4 -4 ${n + 8} ${n + 8}`} role="img" aria-labelledby="qr-title">
      <title id="qr-title">{label}</title>
      <rect x={-4} y={-4} width={n + 8} height={n + 8} fill="#fff" />
      <path d={d} fill="#000" shapeRendering="crispEdges" />
    </svg>
  );
}

export function Security({
  me,
  onChanged,
  onSignedIn,
  onSignedOut,
}: {
  me: Me;
  onChanged: () => void;
  onSignedIn: () => void;
  onSignedOut: () => void;
}) {
  const [password, setPassword] = useState("");
  const [totp, setTotp] = useState<{ secret: string; otpauth_url: string } | null>(null);
  const [totpOn, setTotpOn] = useState(false);
  const [passkeyAdded, setPasskeyAdded] = useState(false);
  const totpAction = useAction();
  const passkeyAction = useAction();
  const passkeys = useLoad(async () => must(await api.GET("/auth/passkeys")));
  const recoveryAction = useAction();
  const [recovery, setRecovery] = useState<string[] | null>(null);
  const [copied, setCopied] = useState(false);
  const signInAgain = useAction();
  // A session that already passed a second factor needs no password. Once the
  // account has one, a password session is refused and the API says why.
  const needPassword = !me.mfa && !me.has_second_factor;
  const needSecondStep = !me.mfa && me.has_second_factor;

  const startTotp = () =>
    totpAction.run(async () => {
      setTotp(must(await api.POST("/auth/totp", { body: needPassword ? { password } : {} })));
    });

  const confirmTotp = (code: string) =>
    totpAction.run(async () => {
      try {
        must(await api.POST("/auth/totp/confirm", { body: { code } }));
      } catch (e) {
        // An expired setup: put the start button back so they can begin again.
        if (e instanceof ApiFailure && e.code === "totp_not_started") setTotp(null);
        throw e;
      }
      setTotp(null);
      setTotpOn(true);
      onChanged();
    });

  const addPasskey = () =>
    passkeyAction.run(async () => {
      setPasskeyAdded(false);
      const begin = must(
        await api.POST("/auth/passkeys/register", {
          body: needPassword ? { password } : {},
        }),
      );
      const optionsJSON = (
        begin.options as {
          publicKey: Parameters<typeof startRegistration>[0]["optionsJSON"];
        }
      ).publicKey;
      let credential: Awaited<ReturnType<typeof startRegistration>>;
      try {
        credential = await startRegistration({ optionsJSON });
      } catch (e) {
        throw new ApiFailure("cancelled", passkeyFailureMessage(e), 0);
      }
      must(
        await api.POST("/auth/passkeys/register/{challengeId}", {
          params: { path: { challengeId: begin.challenge_id } },
          body: credential as unknown as Record<string, unknown>,
        }),
      );
      setPasskeyAdded(true);
      passkeys.reload();
    });

  const createRecovery = () =>
    recoveryAction.run(async () => {
      setCopied(false);
      setRecovery(must(await api.POST("/auth/recovery-codes")).codes);
    });

  const copyRecovery = () =>
    recoveryAction.run(async () => {
      await navigator.clipboard.writeText((recovery ?? []).join("\n"));
      setCopied(true);
    });

  // The browser downloads this blob URL without a data: URL; revoke it when the codes change or go.
  const [recoveryFile, setRecoveryFile] = useState<string | null>(null);
  useEffect(() => {
    if (!recovery) return;
    const url = URL.createObjectURL(new Blob([`${recovery.join("\n")}\n`], { type: "text/plain" }));
    setRecoveryFile(url);
    return () => {
      URL.revokeObjectURL(url);
      setRecoveryFile(null);
    };
  }, [recovery]);
  // From the account or from what happened on this page: a factor added a moment ago counts.
  const hasFactor = me.has_second_factor || me.totp_enabled || totpOn || passkeyAdded;

  return (
    <main>
      <p>
        <Link href="/">Back to your dashboard</Link>
      </p>
      <h1 tabIndex={-1}>Sign-in and security</h1>
      <p>
        A passkey or an authenticator app means a stolen password alone won't get anyone into your
        account.
      </p>

      {needSecondStep && (
        <p className="notice">
          Sign in with your passkey or authenticator code first, then you can add another.
        </p>
      )}
      {needPassword && (
        <Field
          id="password"
          label="Password"
          type="password"
          autoComplete="current-password"
          hint="We ask for it again before changing how you sign in."
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
      )}

      <section aria-labelledby="passkey-h">
        <h2 id="passkey-h">Passkeys</h2>
        <p>Sign in with your fingerprint, face or device PIN. No password to type.</p>
        {passkeys.data && passkeys.data.length > 0 && (
          <ul aria-label="Your passkeys">
            {passkeys.data.map((p) => (
              <li key={p.id}>
                <strong>{p.name}</strong>{" "}
                <span className="muted">
                  · added {day(p.created_at)}
                  {p.last_used_at
                    ? `, last used ${day(p.last_used_at)}`
                    : ", not used to sign in yet"}
                </span>
              </li>
            ))}
          </ul>
        )}
        <ErrorText>{passkeyAction.error}</ErrorText>
        <p className="success" role="status">
          {passkeyAdded &&
            (me.mfa
              ? "Passkey added. Next time, you can sign in with it."
              : "Passkey added. Sign in with it now and you can manage your organizers straight away.")}
        </p>
        {passkeyAdded && !me.mfa && (
          <button
            type="button"
            disabled={passkeyAction.busy}
            onClick={() =>
              passkeyAction.run(async () => {
                await session.passkeyLogin();
                onSignedIn();
              })
            }
          >
            Use it now
          </button>
        )}
        <button
          type="button"
          className={passkeyAdded && !me.mfa ? "secondary" : undefined}
          onClick={addPasskey}
          disabled={passkeyAction.busy || needSecondStep}
        >
          Add a passkey
        </button>
      </section>

      <section aria-labelledby="totp-h">
        <h2 id="totp-h">Authenticator app</h2>
        <p className="success" role="status">
          {(me.totp_enabled || totpOn) &&
            (me.mfa
              ? "Your authenticator app is set up. We'll ask for a code when you sign in with your password."
              : "Your authenticator app is on. It kicks in from your next sign-in, so sign in again with a code to manage your organizers.")}
        </p>
        {(me.totp_enabled || totpOn) && !me.mfa && (
          <>
            <ErrorText>{signInAgain.error}</ErrorText>
            <button
              type="button"
              disabled={signInAgain.busy}
              onClick={() =>
                signInAgain.run(async () => {
                  await session.logout();
                  onSignedOut();
                })
              }
            >
              Sign in again
            </button>
          </>
        )}
        {!me.totp_enabled && !totpOn && !totp && (
          <>
            <p>Use an app like 1Password, Google Authenticator or Authy for a 6-digit code.</p>
            <ErrorText>{totpAction.error}</ErrorText>
            <button type="button" onClick={startTotp} disabled={totpAction.busy || needSecondStep}>
              Set up an authenticator app
            </button>
          </>
        )}
        {totp && (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              confirmTotp(String(new FormData(e.currentTarget).get("code")));
            }}
          >
            <p>Scan this with your authenticator app.</p>
            <QrCode text={totp.otpauth_url} label="QR code for your authenticator app" />
            <p className="muted">
              Can't scan it? Type this key in instead: <code className="secret">{totp.secret}</code>
            </p>
            <Field
              id="code"
              label="6-digit code"
              hint="Type the code your app shows to finish."
              autoComplete="one-time-code"
              inputMode="numeric"
              pattern="[0-9]{6}"
              maxLength={6}
              required
            />
            <ErrorText>{totpAction.error}</ErrorText>
            <button type="submit" disabled={totpAction.busy}>
              Turn it on
            </button>
          </form>
        )}
      </section>

      {hasFactor && (
        <section aria-labelledby="recovery-h">
          <h2 id="recovery-h">Recovery codes</h2>
          {recovery ? (
            <>
              <p className="notice">
                Save these now. Each code works once, and we won't show them again. Making new codes
                switches these off.
              </p>
              <ul className="codes" aria-label="Your recovery codes">
                {recovery.map((c) => (
                  <li key={c}>
                    <code className="secret">{c}</code>
                  </li>
                ))}
              </ul>
              <ErrorText>{recoveryAction.error}</ErrorText>
              <p className="success" role="status">
                {copied && "Copied."}
              </p>
              <button type="button" onClick={copyRecovery}>
                Copy all
              </button>{" "}
              <a
                className="button secondary"
                href={recoveryFile ?? undefined}
                download="ticketslinger-recovery-codes.txt"
              >
                Download as a text file
              </a>
            </>
          ) : (
            <>
              <p>
                Lost your phone? A recovery code gets you in without your authenticator app or
                passkey. You get ten, and each one works once.
              </p>
              {!me.mfa && (
                <p className="notice">
                  Sign in with your passkey or authenticator code first, then you can create them.
                </p>
              )}
              <ErrorText>{recoveryAction.error}</ErrorText>
              <button
                type="button"
                onClick={createRecovery}
                disabled={recoveryAction.busy || !me.mfa}
              >
                Create recovery codes
              </button>
            </>
          )}
        </section>
      )}
    </main>
  );
}
