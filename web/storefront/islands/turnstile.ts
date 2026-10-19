// The event page's only script of ours. Turnstile's api.js is about 28 KB gzipped, so it waits
// until the buyer starts on the ticket form instead of riding on the first load (spec §3).
// The token still travels in cf-turnstile-response, and the API checks it as before.

export type Turnstile = {
  render(el: Element, o: { sitekey: string; callback(token: string): void }): unknown;
};

const API = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";

/** Wires the ticket form: the check starts on first touch; a submit without a token waits for one. */
export function lazyCheck(form: HTMLFormElement, load: () => Promise<Turnstile>) {
  const box = form.querySelector<HTMLElement>(".cf-turnstile");
  if (!box) return;
  let failed = false;
  let checked: Promise<void> | undefined;
  const start = () => {
    checked ??= load()
      .then(
        (t) =>
          new Promise<void>((done) => {
            t.render(box, { sitekey: box.dataset.sitekey ?? "", callback: () => done() });
          }),
      )
      // Blocked or offline: let the form go, and the server's "finish the quick check" answers.
      .catch(() => {
        failed = true;
      });
    return checked;
  };
  for (const type of ["focusin", "change", "pointerdown"]) {
    form.addEventListener(type, start, { once: true });
  }
  form.addEventListener("submit", (e) => {
    const token = form.querySelector<HTMLInputElement>('input[name="cf-turnstile-response"]');
    if (failed || token?.value) return;
    e.preventDefault();
    const submitter = (e as SubmitEvent).submitter;
    void start().then(() => form.requestSubmit(submitter));
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
