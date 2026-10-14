import type { components } from "@ticketslinger/api-client";
import {
  type AnchorHTMLAttributes,
  type ReactNode,
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { ApiFailure } from "./api";

export type Me = components["schemas"]["Me"];

// Routing: pathname + history and a tiny matcher is all we need.
const subscribe = (cb: () => void) => {
  addEventListener("popstate", cb);
  return () => removeEventListener("popstate", cb);
};
export const usePath = () => useSyncExternalStore(subscribe, () => location.pathname);

/** Matches a path like /o/:slug against a pattern; returns the named segments, or null. */
export function match(pattern: string, path: string): Record<string, string> | null {
  const want = pattern.split("/");
  const got = path.split("/");
  if (want.length !== got.length) return null;
  const params: Record<string, string> = {};
  for (const [i, w] of want.entries()) {
    const g = got[i] ?? "";
    if (!w.startsWith(":")) {
      if (w !== g) return null;
      continue;
    }
    if (!g) return null;
    try {
      params[w.slice(1)] = decodeURIComponent(g);
    } catch {
      return null;
    }
  }
  return params;
}

/** Where to go after sign-in: a same-site path or "/". Never an off-site URL. */
export function safeNext(raw: string | null): string {
  if (!raw?.startsWith("/") || raw[1] === "/" || raw[1] === "\\") return "/";
  // The URL parser drops tabs and newlines, so "/\t/evil" really means "//evil".
  return new URL(raw, "http://x").origin === "http://x" ? raw : "/";
}

export function navigate(to: string, replace = false) {
  history[replace ? "replaceState" : "pushState"](null, "", to);
  dispatchEvent(new PopStateEvent("popstate"));
}

export function Link({
  href,
  ...rest
}: AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) {
  return (
    <a
      href={href}
      {...rest}
      onClick={(e) => {
        if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
        e.preventDefault();
        navigate(href);
      }}
    />
  );
}

const OFFLINE = "We couldn't reach TicketSlinger. Check your connection and try again.";
const asFailure = (e: unknown) =>
  e instanceof ApiFailure ? e : new ApiFailure("offline", OFFLINE, 0);

/** Loads data once on mount; reload() fetches it again. */
export function useLoad<T>(load: () => Promise<T>) {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<ApiFailure>();
  const latest = useRef(load);
  latest.current = load;
  const reload = useCallback(
    () =>
      latest.current().then(
        (d) => {
          setData(d);
          setError(undefined);
        },
        (e) => setError(asFailure(e)),
      ),
    [],
  );
  useEffect(() => {
    reload();
  }, [reload]);
  return { data, setData, error, reload };
}

/** Runs a form action, tracking busy state and a human-readable error. */
export function useAction() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function run(fn: () => Promise<void>) {
    setBusy(true);
    setError("");
    try {
      await fn();
    } catch (e) {
      setError(asFailure(e).message);
    } finally {
      setBusy(false);
    }
  }
  return { busy, error, setError, run };
}

/** Errors live in an always-present alert region so screen readers announce them. */
export function ErrorText({ children }: { children: ReactNode }) {
  return (
    <p className="error" role="alert">
      {children}
    </p>
  );
}

export function Field({
  id,
  label,
  hint,
  ...input
}: { id: string; label: string; hint?: ReactNode } & React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      <input id={id} name={id} aria-describedby={hint ? `${id}-hint` : undefined} {...input} />
      {hint && (
        <p className="hint" id={`${id}-hint`}>
          {hint}
        </p>
      )}
    </div>
  );
}

export function NotFound() {
  return (
    <main>
      <h1 tabIndex={-1}>There's nothing here</h1>
      <p>
        <Link href="/">Go to your dashboard</Link>
      </p>
    </main>
  );
}
