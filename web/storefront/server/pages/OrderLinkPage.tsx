/** @jsxImportSource preact */
import { Layout } from "./Layout";

export type OrderLinkPageProps = {
  eventName: string;
  /** Where the form posts: this page's own path. */
  action: string;
  siteKey: string;
  /** What the buyer typed, kept after a refusal. */
  email?: string;
  /** Shown by the email field. */
  emailMessage?: string;
  /** Shown above the button. */
  message?: string;
  /** The request went through. */
  done?: boolean;
};

export function OrderLinkPage(p: OrderLinkPageProps) {
  return (
    <Layout title={`Get your ticket link again · ${p.eventName}`}>
      <p class="organizer">{p.eventName}</p>
      <h1>Get your ticket link again</h1>
      {p.done ? (
        <p role="status">
          Done. If that address bought tickets for {p.eventName}, the link is on its way. It can
          take a minute or two.
        </p>
      ) : (
        <>
          <p>
            Tell us the email you bought with. If it has tickets for this event, we'll send a fresh
            link there, and the old one will stop working.
          </p>
          <form class="details" method="post" action={p.action}>
            <div class="field">
              <label for="email">Email</label>
              <input
                id="email"
                name="email"
                type="email"
                autocomplete="email"
                maxLength={254}
                required
                value={p.email ?? ""}
                {...(p.emailMessage
                  ? { "aria-invalid": true, "aria-describedby": "email-error" }
                  : {})}
              />
              {p.emailMessage && (
                <p class="notice" id="email-error" role="alert">
                  {p.emailMessage}
                </p>
              )}
            </div>
            {p.siteKey && (
              <>
                <div class="cf-turnstile" data-sitekey={p.siteKey} />
                <script src="https://challenges.cloudflare.com/turnstile/v0/api.js" async defer />
              </>
            )}
            {p.message && (
              <p class="notice" role="alert">
                {p.message}
              </p>
            )}
            <button type="submit">Send my link</button>
          </form>
        </>
      )}
    </Layout>
  );
}
