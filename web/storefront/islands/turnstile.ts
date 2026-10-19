// The event page's only script of ours. Turnstile's api.js is about 28 KB gzipped, so it waits
// until the buyer starts on the ticket form instead of riding on the first load (spec §3).
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
};

const API = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";

/** Wires the ticket form: the check starts on first touch; a submit without a token waits for one. */
export function lazyCheck(form: HTMLFormElement, load: () => Promise<Turnstile>) {
  const box = form.querySelector<HTMLElement>(".cf-turnstile");
  if (!box) return;
  // Blocked, offline or erroring: let the form go, and the server's "finish the quick check" answers.
  let failed = false;
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
  const start = () => {
    if (started) return;
    started = true;
    load().then((t) => {
      t.render(box, {
        sitekey: box.dataset.sitekey ?? "",
        // The token goes in the form's field before the held submit reads it.
        callback: (token) => {
          const field = form.querySelector<HTMLInputElement>('input[name="cf-turnstile-response"]');
          if (field) field.value = token;
          send();
        },
        "error-callback": fail,
      });
    }, fail);
  };
  for (const type of ["focusin", "change", "pointerdown"]) {
    form.addEventListener(type, start, { once: true });
  }
  form.addEventListener("submit", (e) => {
    const token = form.querySelector<HTMLInputElement>('input[name="cf-turnstile-response"]');
    if (failed || token?.value) return;
    e.preventDefault();
    // One held submit at a time; another tap just waits with it.
    if (held) return;
    const submitter = (e as SubmitEvent).submitter;
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
  const form = document.querySelector<HTMLFormElement>("form.buy");
  if (form) lazyCheck(form, load);
}
