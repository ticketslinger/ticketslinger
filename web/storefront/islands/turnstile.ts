// The only script of ours on the event and order-link pages. Turnstile's api.js is about 28 KB
// gzipped, so it waits until the buyer starts on the form instead of riding on the first load (spec §3).
// The token still travels in cf-turnstile-response, and the API checks it as before.

export type Turnstile = {
  render(
    el: Element,
    o: {
      sitekey: string;
      callback(token: string): void;
      "error-callback"?(code: string): void;
    },
  ): unknown;
  reset(el: Element): void;
};

const API = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";

/** Wires the ticket form: the check starts on first touch; a submit without a token waits for one. */
export function lazyCheck(form: HTMLFormElement, load: () => Promise<Turnstile>) {
  const box = form.querySelector<HTMLElement>(".cf-turnstile");
  if (!box) return;
  // Blocked, offline or erroring: let the form go, and the server's "finish the quick check" answers.
  let failed = false;
  const tokenField = () => {
    let field = form.querySelector<HTMLInputElement>('input[name="cf-turnstile-response"]');
    if (!field) {
      field = Object.assign(form.ownerDocument.createElement("input"), {
        type: "hidden",
        name: "cf-turnstile-response",
      });
      form.append(field);
    }
    return field;
  };
  // The held submit, sent by the NEXT token: a token that expired and came back counts, an old one doesn't.
  let held: (() => void) | undefined;
  const send = () => {
    const go = held;
    held = undefined;
    go?.();
  };
  const fail = () => {
    failed = true;
    send();
  };
  let started = false;
  let widget: Turnstile | undefined;
  const start = () => {
    if (started) return;
    started = true;
    load()
      .then((t) => {
        widget = t;
        t.render(box, {
          sitekey: box.dataset.sitekey ?? "",
          // The token goes in the form's field before the held submit reads it.
          callback: (token) => {
            tokenField().value = token;
            send();
          },
          "error-callback": fail,
        });
      })
      // A throw from render (no turnstile global, a bad site key) fails the same way.
      .catch(fail);
  };
  // Back from the next page, out of the back/forward cache: the button works again, and the
  // token it sent is spent, so it goes and the widget fetches a fresh one.
  form.ownerDocument.defaultView?.addEventListener("pageshow", (e) => {
    if (!e.persisted) return;
    for (const b of form.querySelectorAll("button")) b.disabled = false;
    const token = form.querySelector<HTMLInputElement>('input[name="cf-turnstile-response"]');
    if (token) token.value = "";
    widget?.reset(box);
  });
  for (const type of ["focusin", "change", "pointerdown"]) {
    form.addEventListener(type, start, { once: true });
  }
  form.addEventListener("submit", (e) => {
    const submitter = (e as SubmitEvent).submitter as HTMLButtonElement | null;
    const token = form.querySelector<HTMLInputElement>('input[name="cf-turnstile-response"]');
    if (failed || token?.value) {
      // The form is going: a second tap would send a second order with the same token.
      if (submitter) submitter.disabled = true;
      return;
    }
    e.preventDefault();
    // One held submit at a time; another tap just waits with it.
    if (held) return;
    held = () => form.requestSubmit(submitter);
    start();
  });
}

function load(): Promise<Turnstile> {
  return new Promise((resolve, reject) => {
    const s = document.createElement("script");
    s.src = API;
    s.async = true;
    s.onload = () => resolve((window as unknown as { turnstile: Turnstile }).turnstile);
    s.onerror = reject;
    document.head.append(s);
  });
}

if (typeof document !== "undefined") {
  // The form holding the check: the ticket form, or the order-link form.
  const form = document.querySelector(".cf-turnstile")?.closest("form");
  if (form) lazyCheck(form, load);
}
