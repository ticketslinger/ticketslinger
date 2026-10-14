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

/** Matches a path like /o/:slug (one trailing slash allowed) against a pattern; returns the named segments, or null. */
export function match(pattern: string, path: string): Record<string, string> | null {
  const want = pattern.split("/");
  const got = (path.length > 1 && path.endsWith("/") ? path.slice(0, -1) : path).split("/");
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

// Set while a form has unsaved changes: it decides whether a navigation goes ahead.
let blocker: ((go: () => void) => void) | null = null;

/**
 * Moves to another page. A form with unsaved changes gets to ask first,
 * unless `force` is set (the move is the result of saving) or it's a
 * redirect that replaces the current entry.
 */
export function navigate(to: string, replace = false, force = false) {
  const go = () => {
    history[replace ? "replaceState" : "pushState"](null, "", to);
    dispatchEvent(new PopStateEvent("popstate"));
  };
  if (blocker && !replace && !force) blocker(go);
  else go();
}

/**
 * While `dirty`, asks before leaving the page: an in-page dialog for links
 * and navigate(), the browser's own prompt for reloads and closing the tab.
 * Render the returned element somewhere in the form's component.
 */
export function useUnsavedChanges(dirty: boolean) {
  const [pending, setPending] = useState<(() => void) | null>(null);
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    addEventListener("beforeunload", warn);
    blocker = (go) => setPending(() => go);
    return () => {
      removeEventListener("beforeunload", warn);
      blocker = null;
    };
  }, [dirty]);
  useEffect(() => {
    if (pending && !dialog.current?.open) dialog.current?.showModal();
  }, [pending]);
  return (
    <dialog
      ref={dialog}
      className="confirm"
      aria-labelledby="unsaved-title"
      onClose={() => setPending(null)}
    >
      <p id="unsaved-title">You have unsaved changes. Leave without saving?</p>
      <div className="row">
        <button type="button" className="inline secondary" onClick={() => dialog.current?.close()}>
          Keep editing
        </button>
        <button
          type="button"
          className="inline"
          onClick={() => {
            dialog.current?.close();
            pending?.();
          }}
        >
          Leave without saving
        </button>
      </div>
    </dialog>
  );
}

/** Runs `go`, after the in-page "unsaved changes" question if a form has some. */
export const askToLeave = (go: () => void) => (blocker ? blocker(go) : go());

/** A ref that focuses its element when it appears, for confirm steps that replace a button. */
export const focusOnMount = (el: HTMLElement | null) => el?.focus();

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
  // The request property the last failure names, so a form can point at it.
  const [field, setField] = useState("");
  async function run(fn: () => Promise<void>) {
    setBusy(true);
    setError("");
    setField("");
    try {
      await fn();
    } catch (e) {
      const f = asFailure(e);
      setError(f.message);
      setField(f.field);
    } finally {
      setBusy(false);
    }
  }
  const clear = () => {
    setError("");
    setField("");
  };
  return { busy, error, field, setError, clear, run };
}

/** Errors live in an always-present alert region so screen readers announce them. */
export function ErrorText({ children, id }: { children: ReactNode; id?: string }) {
  return (
    <p className="error" role="alert" id={id}>
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
