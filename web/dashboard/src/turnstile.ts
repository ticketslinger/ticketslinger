import { useEffect, useRef, useState } from "react";

// Explicit rendering: the script draws nothing until we call render().
const SRC = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";

type Turnstile = {
  render(
    el: HTMLElement,
    opts: {
      sitekey: string;
      callback: (token: string) => void;
      "expired-callback": () => void;
      "error-callback": () => void;
    },
  ): string;
  remove(id: string): void;
};

let script: Promise<Turnstile> | undefined;

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

/**
 * Renders the widget into el. onToken gets each token as the check passes,
 * including the fresh one the widget fetches when an old one expires, and ""
 * while there's no live token. Resolves with a function that removes it.
 */
export async function loadTurnstile(
  siteKey: string,
  el: HTMLElement,
  onToken: (token: string) => void,
): Promise<() => void> {
  const t = await load();
  const id = t.render(el, {
    sitekey: siteKey,
    callback: onToken,
    "expired-callback": () => onToken(""),
    "error-callback": () => onToken(""),
  });
  return () => t.remove(id);
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
 * Shows the widget in `ref` while `wanted` and Turnstile is on, and removes
 * it on unmount. `token` is the live token, or "" until the check passes.
 * Each token works once, so call `reset` after sending one.
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
    let remove: (() => void) | undefined;
    setToken("");
    loadTurnstile(key, el, (t) => live && setToken(t)).then(
      (r) => {
        if (live) remove = r;
        else r();
      },
      () => {},
    );
    return () => {
      live = false;
      remove?.();
    };
  }, [wanted, key, round]);
  return { ref, token, shown: wanted && key !== "", reset: () => setRound((r) => r + 1) };
}
