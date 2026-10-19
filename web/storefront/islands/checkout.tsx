/** @jsxImportSource preact */
import type { Stripe, StripeElements } from "@stripe/stripe-js";
// The pure entry loads Stripe.js from js.stripe.com only when we call loadStripe, so test-mode
// and free checkouts never fetch it.
import { loadStripe } from "@stripe/stripe-js/pure";
import type { components } from "@ticketslinger/api-client";
import { render } from "preact";
import { useEffect, useRef, useState } from "preact/hooks";
import { formatCents } from "../server/money";
import { eventPath, orderPath } from "./links";

type Schemas = components["schemas"];
type Question = Schemas["StorefrontQuestion"];
type Item = Schemas["CheckoutItem"];
type ApiError = Partial<Schemas["Error"]["error"]>;
export type PayRequest = Schemas["PayCheckout"];

/** What the server puts in #checkout-data. */
export type CheckoutData = {
  /** The checkout's id: names what this page keeps in sessionStorage, where the token never goes. */
  id: string;
  token: string;
  /** The checkout's status when the page was drawn. */
  status: Schemas["Checkout"]["status"];
  expires_at: string;
  quote: Schemas["Quote"];
  items: Item[];
  ticket_names: Record<string, string>;
  questions: Question[];
  event_name: string;
  timezone: string;
  /** Slugs of the event page, for starting again. */
  organizer: string;
  event_slug: string;
};

/** One answer the form asks for. name is the API's own field path, so its 422s map straight back. */
export type Field = { name: string; question: Question; ticket_index: number };

const ticketsOf = (items: Item[]) =>
  items.flatMap((it) => Array.from({ length: it.quantity }, () => it.ticket_type_id));

const asks = (q: Question, type: string) => q.applies_to_all || q.applies_to.includes(type);

/** One field per order question the cart is asked, and one per ticket a ticket question asks. */
export function answersFor(questions: Question[], items: Item[]): Field[] {
  const tickets = ticketsOf(items);
  const field = (question: Question, ticket_index: number) => ({
    name: `answers.${question.id}.${ticket_index}`,
    question,
    ticket_index,
  });
  const out: Field[] = [];
  for (const q of questions) {
    if (q.scope === "order") {
      if (tickets.some((t) => asks(q, t))) out.push(field(q, -1));
    } else {
      tickets.forEach((t, i) => {
        if (asks(q, t)) out.push(field(q, i));
      });
    }
  }
  return out;
}

/** The form as payCheckout takes it. A checkbox left unticked is "false". */
export function payload(form: FormData, fields: Field[]): PayRequest {
  const text = (k: string) => {
    const v = form.get(k);
    return typeof v === "string" ? v : "";
  };
  return {
    email: text("email").trim(),
    attendees: form.getAll("attendee").map((v) => (typeof v === "string" ? v.trim() : "")),
    answers: fields.map((f) => ({
      question_id: f.question.id,
      ticket_index: f.ticket_index,
      value: f.question.kind === "checkbox" ? String(text(f.name) === "true") : text(f.name),
    })),
  };
}

// Our words, by error code. The API's own message never reaches the page.
const FIELD_COPY: Record<string, string> = {
  bad_email: "That email doesn't look right. It should look like ada@example.com.",
  missing_answer: "This one needs an answer.",
  bad_answer: "That answer doesn't fit this question. Check it and try again.",
  bad_attendee: "Keep each name to 120 characters.",
};

/** A 422 that names a field, as {field: message}. */
export function fieldErrors(e: ApiError): Record<string, string> {
  const m = FIELD_COPY[e.code ?? ""];
  return m && e.field ? { [e.field]: m } : {};
}

const GENERIC = "Something went wrong on our side. Try again.";
const BUSY = "Busy for a moment. Try again.";
const NOT_READY = "This event can't take card payments just yet.";
const EXPIRED =
  "Your 15 minutes ran out and the tickets went back on sale. Start again and we'll hold them for you.";
const PRICE_CHANGED =
  "The price changed while you were checking out. Start again to see the new total.";
export const MESSAGES: Record<string, string> = {
  payments_not_ready: NOT_READY,
  payments_not_configured: NOT_READY,
  below_minimum_charge: "Card payments start at $0.50, and this order comes to less.",
  rate_limited: "That's a lot of tries. Wait a minute and try again.",
  try_again: BUSY,
  payments_unavailable: BUSY,
  idempotency_in_progress: BUSY,
  payments_refused:
    "Card payments for this event are switched off right now. Try again later, or contact the organizer.",
};
/** Once a card may have been charged. Never "back on sale", never "start again". */
export const PENDING =
  "Your payment may still be going through. We'll email you the moment it does. Check your inbox before you try again.";
/** The payment was stopped unfinished, an hour after the hold ran out. */
export const CANCELLED =
  "Your payment didn't go through, so you haven't been charged. The tickets you were holding have gone back on sale.";
/** Paid, but the buyer has since asked for a new link: it goes by email only. */
export const LINK_BY_EMAIL =
  "You've got these tickets. To see them, open the newest link we emailed you.";
/** The organizer cancelled the event; the API says the same in its 409. */
export const EVENT_CANCELLED =
  "This event has been cancelled, so it isn't selling tickets any more. If you'd already paid, we're refunding you in full.";
// Answers that end this checkout: the buyer has to start again from the event.
// checkout_expired isn't one: a payment may already exist, so we ask the checkout first.
const OVER: Record<string, string> = {
  price_changed: PRICE_CHANGED,
  event_cancelled: EVENT_CANCELLED,
};

/**
 * What one read of the checkout means while we wait on a payment. started: this page
 * handed a card to Stripe (or came back from the bank), so a charge may exist.
 * The API keeps a checkout with a payment "paying" past its hold; "expired" means none.
 */
export function waitStep(
  c: { status: string; expires_at: string },
  started: boolean,
  now = Date.now(),
): "done" | "over" | "late" | "wait" {
  if (c.status === "completed") return "done";
  if (c.status === "cancelled") return "over";
  if (c.status === "expired") return started ? "late" : "over";
  if (c.status === "paying" && Date.parse(c.expires_at) <= now) return "late";
  return "wait";
}

const DECLINED = "Your bank didn't approve that payment. Try again, or use another card.";
const UNFINISHED =
  "You didn't finish your bank's check, so nothing was charged. Try again when you're ready.";
/** A decline after the hold ran out: nothing was charged, and there's nothing left to pay for here. */
export const DECLINED_LATE =
  "Your bank didn't approve that payment, so you haven't been charged. Your hold ran out in the meantime, so head back to the event to try again.";

/**
 * Where the page starts. Back from the bank: a decline is a decline (never PENDING), and a
 * pass waits for the webhook. A payment out there past its hold waits, with the pending words.
 */
export function firstPhase(
  back: URLSearchParams,
  data: Pick<CheckoutData, "status" | "expires_at">,
  now = Date.now(),
): Phase {
  const lapsed = Date.parse(data.expires_at) <= now;
  if (back.get("redirect_status") === "failed") {
    return lapsed ? { kind: "over", message: DECLINED_LATE } : { kind: "details" };
  }
  if (back.has("payment_intent")) return { kind: "waiting", test: false, started: true };
  if (data.status === "paying" && lapsed) {
    return { kind: "waiting", test: false, started: true, late: true };
  }
  return { kind: "details" };
}

/** Where the page starts after the bank, from Stripe's own word on the payment. */
export function afterReturn(status: string | undefined, lapsed: boolean): Phase {
  switch (status) {
    case "succeeded":
    case "processing":
      return { kind: "waiting", test: false, started: true };
    case "requires_payment_method":
    case "requires_action":
      return lapsed ? { kind: "over", message: DECLINED_LATE } : { kind: "details" };
    case "canceled":
      return { kind: "over", message: CANCELLED };
    default:
      return { kind: "waiting", test: false, started: true };
  }
}

// What reading the payment back after the bank needs: the publishable key and the account,
// never the client secret (that comes back in the URL). Storage may be blocked: then we go
// by the redirect's own word.
type PayKeys = { publishable_key: string; account?: string };
const keysName = (id: string) => `ts-pay:${id}`;
function saveKeys(id: string, k: PayKeys) {
  try {
    sessionStorage.setItem(keysName(id), JSON.stringify(k));
  } catch {}
}
function savedKeys(id: string): PayKeys | undefined {
  try {
    const k = JSON.parse(sessionStorage.getItem(keysName(id)) ?? "null");
    return typeof k?.publishable_key === "string" ? k : undefined;
  } catch {
    return undefined;
  }
}

/** The way back to the event, when both slugs check out. */
function BackLink({ data }: { data: CheckoutData }) {
  const to = eventPath(data);
  return to ? (
    <a class="button" href={to}>
      Back to {data.event_name}
    </a>
  ) : null;
}

/** A card may have been charged: say so, and still leave a way out. */
export function Pending({ data }: { data: CheckoutData }) {
  return (
    <div class="pay">
      <p class="status-line" role="status">
        {PENDING}
      </p>
      <BackLink data={data} />
    </div>
  );
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** To the order, by a path we rebuilt ourselves; false when the link doesn't check out. */
const go = (url: string | undefined, code: string | undefined) => {
  const path = url ? orderPath(url, code) : undefined;
  if (path) location.assign(path);
  return path !== undefined;
};

type Answer = { status: number; body: { error?: ApiError } & Schemas["PaymentStart"] };

// One key for the whole submission. Pay is idempotent (same order, same payment), so a
// retry with the key is safe; the API keeps no copy, so in_progress means "ask again soon".
async function pay(token: string, body: PayRequest, key: string): Promise<Answer> {
  let busy = 0;
  for (let waits = 0; ; waits++) {
    let res: Response | undefined;
    try {
      res = await fetch(`/api/checkouts/${token}/pay`, {
        method: "POST",
        headers: { "content-type": "application/json", "idempotency-key": key },
        body: JSON.stringify(body),
      });
    } catch {}
    const json = res ? await res.json().catch(() => ({})) : {};
    const code = json?.error?.code;
    const after = Number(res?.headers.get("retry-after")) * 1000 || 1000;
    if (res?.status === 409 && code === "idempotency_in_progress" && waits < 10) {
      await sleep(after);
      continue;
    }
    const down =
      !res || res.status === 502 || code === "try_again" || code === "payments_unavailable";
    if (down && busy < 3) {
      await sleep(Math.max(after, 1000 * 2 ** busy++));
      continue;
    }
    return { status: res?.status ?? 0, body: json };
  }
}

export type Phase =
  | { kind: "details" }
  | { kind: "card"; amount: number }
  // late: the hold ran out, or two minutes passed, while a card may be charged.
  | { kind: "waiting"; test: boolean; started: boolean; late?: boolean }
  | { kind: "over"; message: string };

// In the event's own time zone, named, so "7:42 PM MDT" means one moment wherever the buyer is.
function clock(iso: string, timeZone: string) {
  const o = { hour: "numeric", minute: "2-digit", timeZoneName: "short" } as const;
  let f: Intl.DateTimeFormat;
  try {
    f = new Intl.DateTimeFormat("en-US", { ...o, timeZone });
  } catch {
    f = new Intl.DateTimeFormat("en-US", o);
  }
  return f.format(new Date(iso)).replace(/\s/g, " ");
}

function Checkout({ data }: { data: CheckoutData }) {
  const fields = answersFor(data.questions, data.items);
  const tickets = ticketsOf(data.items);
  // Back from a bank's check (3D Secure): Stripe adds these to return_url.
  // Read once, then dropped from the address bar (see below): the client secret shouldn't
  // linger there, and the next return_url shouldn't carry the last one's params.
  const [back] = useState(() => new URLSearchParams(location.search));
  const failed = back.get("redirect_status") === "failed";
  const [phase, setPhase] = useState<Phase>(() => firstPhase(back, data));
  // The last word on this page's payment was a decline: nothing was charged.
  const declined = useRef(failed);
  const [sending, setSending] = useState(false);
  const [message, setMessage] = useState<string | undefined>(failed ? DECLINED : undefined);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [cardError, setCardError] = useState<string>();
  const [now, setNow] = useState(Date.now());
  const card = useRef<HTMLDivElement>(null);
  const stripe = useRef<{ stripe: Stripe; elements: StripeElements }>(undefined);
  const start = useRef<Schemas["PaymentStart"]>(undefined);

  const expires = Date.parse(data.expires_at);
  // After the bank, ask Stripe what became of the payment rather than trusting the redirect.
  useEffect(() => {
    if (!back.has("payment_intent")) return;
    const drop = () => history.replaceState(null, "", location.pathname);
    const secret = back.get("payment_intent_client_secret");
    const keys = savedKeys(data.id);
    if (!secret || !keys) return drop();
    (async () => {
      const s = await loadStripe(keys.publishable_key, { stripeAccount: keys.account }).catch(
        () => null,
      );
      const r = await s?.retrievePaymentIntent(secret).catch(() => undefined);
      drop();
      const status = r?.paymentIntent?.status;
      if (!status) return; // No answer: the redirect's word stands.
      const next = afterReturn(status, Date.parse(data.expires_at) <= Date.now());
      declined.current = next.kind !== "waiting";
      setMessage(
        status === "requires_action"
          ? UNFINISHED
          : status === "requires_payment_method"
            ? DECLINED
            : undefined,
      );
      setPhase(next);
    })();
  }, [back, data.id, data.expires_at]);
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(t);
  }, []);
  // Only before paying: paying extends the hold, and from then on the API says when it's over.
  // Even here we ask first: another tab may have started a payment on this checkout.
  useEffect(() => {
    if (phase.kind === "details" && now >= expires) {
      setPhase(
        declined.current
          ? { kind: "over", message: DECLINED_LATE }
          : { kind: "waiting", test: false, started: false },
      );
    }
  }, [now, phase.kind, expires]);

  const started = phase.kind === "waiting" && phase.started;
  useEffect(() => {
    if (phase.kind !== "waiting") return;
    let live = true;
    (async () => {
      const until = Date.now() + 120_000;
      while (live && Date.now() < until) {
        try {
          // The cap is two minutes, so no one read may hang past it.
          const r = await fetch(`/api/checkouts/${data.token}`, {
            signal: AbortSignal.timeout(10_000),
          });
          if (r.status === 409) {
            const e = await r.json().catch(() => ({}));
            if (e?.error?.code === "event_cancelled") {
              return setPhase({ kind: "over", message: EVENT_CANCELLED });
            }
          }
          if (r.ok) {
            const c = await r.json();
            const step = waitStep(c, started);
            if (step === "done") {
              if (!go(c.order_url, c.order_code)) {
                setPhase({ kind: "over", message: c.order_url ? PENDING : LINK_BY_EMAIL });
              }
              return;
            }
            if (step === "over") {
              return setPhase({
                kind: "over",
                message: c.status === "cancelled" ? CANCELLED : EXPIRED,
              });
            }
            // Keep polling: the webhook may still confirm it.
            if (step === "late") setPhase((p) => (p.kind === "waiting" ? { ...p, late: true } : p));
          }
        } catch {}
        await sleep(1500);
      }
      if (live) setPhase((p) => (p.kind === "waiting" ? { ...p, late: true } : p));
    })();
    return () => {
      live = false;
    };
  }, [phase.kind, started, data.token]);

  useEffect(() => {
    const s = start.current;
    if (phase.kind !== "card" || !s?.publishable_key || !s.client_secret || !card.current) return;
    const el = card.current;
    (async () => {
      const loaded = await loadStripe(s.publishable_key ?? "", { stripeAccount: s.account }).catch(
        () => null,
      );
      if (!loaded)
        return setCardError("We couldn't load the card form. Reload the page to try again.");
      const elements = loaded.elements({ clientSecret: s.client_secret ?? "" });
      elements.create("payment").mount(el);
      stripe.current = { stripe: loaded, elements };
    })();
  }, [phase.kind]);

  async function submit(e: Event) {
    e.preventDefault();
    if (sending) return;
    const body = payload(new FormData(e.currentTarget as HTMLFormElement), fields);
    setSending(true);
    setMessage(undefined);
    setErrors({});
    const { status, body: res } = await pay(data.token, body, crypto.randomUUID());
    setSending(false);
    const err = res.error ?? {};
    if (status === 200) {
      if (res.kind === "free") {
        if (!go(res.order_url, res.order_code)) setPhase({ kind: "over", message: GENERIC });
        return;
      }
      start.current = res;
      return setPhase(
        res.kind === "stripe"
          ? { kind: "card", amount: res.amount_cents }
          : { kind: "waiting", test: true, started: true },
      );
    }
    // Already paid: the checkout knows its order.
    if (err.code === "checkout_completed") {
      return setPhase({ kind: "waiting", test: false, started: true });
    }
    // The checkout says whether a payment is still out there.
    if (err.code === "checkout_expired") {
      return setPhase({ kind: "waiting", test: false, started: false });
    }
    const over = OVER[err.code ?? ""];
    if (over) return setPhase({ kind: "over", message: over });
    const byField = fieldErrors(err);
    const { attendees, ...rest } = byField;
    setErrors(rest);
    setMessage(
      attendees ??
        (Object.keys(rest).length ? "Check the answers marked below." : MESSAGES[err.code ?? ""]) ??
        GENERIC,
    );
    const first = Object.keys(rest)[0];
    if (first) document.getElementById(first)?.focus();
  }

  async function confirm() {
    const s = stripe.current;
    if (!s || sending) return;
    setSending(true);
    setCardError(undefined);
    const keys = start.current;
    if (keys?.publishable_key) {
      saveKeys(data.id, { publishable_key: keys.publishable_key, account: keys.account });
    }
    const r = await s.stripe.confirmPayment({
      elements: s.elements,
      confirmParams: { return_url: location.href },
      redirect: "if_required",
    });
    setSending(false);
    // Stripe's own words for the card: they say what the bank said.
    if (r.error) {
      declined.current = true;
      return setCardError(r.error.message ?? GENERIC);
    }
    declined.current = false;
    setPhase({ kind: "waiting", test: false, started: true });
  }

  if (phase.kind === "over") {
    return (
      <div class="pay">
        <p class="notice" role="alert">
          {phase.message}
        </p>
        <BackLink data={data} />
      </div>
    );
  }
  if (phase.kind === "waiting" && phase.late) return <Pending data={data} />;
  if (phase.kind === "waiting") {
    return (
      <p class="status-line" role="status">
        {phase.test
          ? "Test mode: no card needed. Waiting for the payment to go through…"
          : "Waiting for the payment to go through…"}
      </p>
    );
  }
  if (phase.kind === "card") {
    return (
      <div class="pay">
        <div ref={card} class="card" />
        <button type="button" onClick={confirm} disabled={sending}>
          Pay {formatCents(phase.amount)}
        </button>
        {cardError && (
          <p class="notice" role="alert">
            {cardError}
          </p>
        )}
      </div>
    );
  }

  const err = (name: string) =>
    errors[name] ? { "aria-invalid": true, "aria-describedby": `${name}-error` } : {};
  const note = (name: string) =>
    errors[name] && (
      <p class="notice" id={`${name}-error`} role="alert">
        {errors[name]}
      </p>
    );
  const input = (f: Field, label: string) => {
    const q = f.question;
    const id = f.name;
    if (q.kind === "checkbox") {
      return (
        <div class="field check" key={id}>
          <input
            type="checkbox"
            id={id}
            name={id}
            value="true"
            required={q.required}
            {...err(id)}
          />
          <label for={id}>{label}</label>
          {note(id)}
        </div>
      );
    }
    return (
      <div class="field" key={id}>
        <label for={id}>{label}</label>
        {q.kind === "select" ? (
          <select id={id} name={id} required={q.required} {...err(id)}>
            <option value="">Choose one</option>
            {q.options.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </select>
        ) : (
          <input id={id} name={id} maxLength={1000} required={q.required} {...err(id)} />
        )}
        {note(id)}
      </div>
    );
  };

  return (
    <form class="details" onSubmit={submit}>
      <p class="hold">We're holding your tickets until {clock(data.expires_at, data.timezone)}.</p>
      <div class="field">
        <label for="email">Email</label>
        <input
          id="email"
          name="email"
          type="email"
          autocomplete="email"
          maxLength={254}
          required
          {...err("email")}
        />
        <p class="hint">Your tickets go here.</p>
        {note("email")}
      </div>
      {tickets.map((type, i) => (
        <fieldset class="ticket-details" key={`${type}-${i}`}>
          <legend>
            Ticket {i + 1}: {data.ticket_names[type] ?? "Ticket"}
          </legend>
          <div class="field">
            <label for={`attendee-${i}`}>Name on ticket {i + 1}</label>
            <input id={`attendee-${i}`} name="attendee" maxLength={120} autocomplete="name" />
          </div>
          {fields
            .filter((f) => f.ticket_index === i)
            .map((f) => input(f, `${f.question.label} (ticket ${i + 1})`))}
        </fieldset>
      ))}
      {fields.filter((f) => f.ticket_index === -1).map((f) => input(f, f.question.label))}
      {message && (
        <p class="notice" role="alert">
          {message}
        </p>
      )}
      <button type="submit" disabled={sending}>
        {data.quote.total_cents > 0
          ? "Continue to payment"
          : tickets.length > 1
            ? "Get my free tickets"
            : "Get my free ticket"}
      </button>
    </form>
  );
}

export function mount(root: HTMLElement, data: CheckoutData): void {
  root.textContent = "";
  render(<Checkout data={data} />, root);
}

if (typeof document !== "undefined") {
  const root = document.getElementById("checkout");
  const json = document.getElementById("checkout-data")?.textContent;
  if (root && json) mount(root, JSON.parse(json));
}
