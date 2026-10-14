import { useEffect, useRef, useState } from "react";

// Explicit rendering: the script draws nothing until we call render().
const SRC = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";

type Turnstile = {
  render(
    el: HTMLElement,
    opts: { sitekey: string; callback: (token: string) => void; "error-callback": () => void },
  ): string;
  remove(id: string): void;
};

let script: Promise<Turnstile> | undefined;
const widgets = new WeakMap<HTMLElement, string>();

/** Adds Cloudflare's script once, as a script element (the CSP allows no inline code). */
function load(): Promise<Turnstile> {
  script ??= new Promise((resolve, reject) => {
    const s = document.createElement("script");
    s.src = SRC;
    s.async = true;
    s.onload = () => {
      const t = (window as { turnstile?: Turnstile }).turnstile;
      if (t) resolve(t);
      else reject(new Error("turnstile missing"));
    };
    s.onerror = () => {
      script = undefined; // let the next try load it again
      s.remove();
      reject(new Error("turnstile unreachable"));
    };
    document.head.append(s);
  });
  return script;
}

/** Renders the widget into el, replacing any earlier one there, and resolves with its token. */
export async function loadTurnstile(siteKey: string, el: HTMLElement): Promise<string> {
  const t = await load();
  const old = widgets.get(el);
  if (old) t.remove(old);
  return new Promise((resolve, reject) => {
    widgets.set(
      el,
      t.render(el, {
        sitekey: siteKey,
        callback: resolve,
        "error-callback": () => reject(new Error("turnstile error")),
      }),
    );
  });
}

let siteKey: Promise<string> | undefined;

/** The site key from /config.json, fetched once; "" when Turnstile is off. */
export function turnstileSiteKey(): Promise<string> {
  siteKey ??= fetch("/config.json")
    .then((r) => r.json() as Promise<{ turnstileSiteKey?: string }>)
    .then(
      (c) => c.turnstileSiteKey ?? "",
      () => "",
    );
  return siteKey;
}

/**
 * Shows the widget in `ref` while `wanted` and Turnstile is on. `token` stays
 * "" until the check passes. Each token works once, so call `reset` after
 * sending one that didn't sign you in.
 */
export function useTurnstile(wanted: boolean) {
  const [key, setKey] = useState("");
  const [token, setToken] = useState("");
  const [round, setRound] = useState(0);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    turnstileSiteKey().then(setKey);
  }, []);
  // biome-ignore lint/correctness/useExhaustiveDependencies: round only asks for a fresh widget
  useEffect(() => {
    const el = ref.current;
    if (!wanted || !key || !el) return;
    let live = true;
    setToken("");
    loadTurnstile(key, el).then(
      (t) => live && setToken(t),
      () => {},
    );
    return () => {
      live = false;
    };
  }, [wanted, key, round]);
  return { ref, token, shown: wanted && key !== "", reset: () => setRound((r) => r + 1) };
}
