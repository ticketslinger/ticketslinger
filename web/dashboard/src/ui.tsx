import type { components } from "@ticketslinger/api-client";
import { type AnchorHTMLAttributes, type ReactNode, useState, useSyncExternalStore } from "react";
import { ApiFailure } from "./api";

export type Me = components["schemas"]["Me"];

// Routing: the paths are fixed, so pathname + history is all we need.
const subscribe = (cb: () => void) => {
  addEventListener("popstate", cb);
  return () => removeEventListener("popstate", cb);
};
export const usePath = () => useSyncExternalStore(subscribe, () => location.pathname);

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
      setError(
        e instanceof ApiFailure
          ? e.message
          : "We couldn't reach TicketSlinger. Check your connection and try again.",
      );
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
