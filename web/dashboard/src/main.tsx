import {
  Fragment,
  type ReactNode,
  StrictMode,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";
import { createRoot } from "react-dom/client";
import { api } from "./api";
import { ApiKeys } from "./routes/ApiKeys";
import { Audit } from "./routes/Audit";
import { EventPage, Events, NewEvent } from "./routes/Events";
import { Home } from "./routes/Home";
import { NewOrganizer } from "./routes/NewOrganizer";
import { type Org, Organizer, type Section } from "./routes/Organizer";
import { Security } from "./routes/Security";
import { SignIn } from "./routes/SignIn";
import { SignUp } from "./routes/SignUp";
import { VerifyEmail } from "./routes/VerifyEmail";
import { type Me, match, NotFound, navigate, safeNext, usePath } from "./ui";
import "./styles.css";

const PUBLIC = new Set(["/signin", "/signup"]);

// Pages inside an organizer, first match wins.
const ORGANIZER: [string, Section, (org: Org, me: Me, p: Record<string, string>) => ReactNode][] = [
  ["/o/:slug", "events", (org) => <Events org={org} />],
  ["/o/:slug/events/new", "events", (org) => <NewEvent org={org} />],
  ["/o/:slug/events/:id", "events", (org, _, p) => <EventPage org={org} id={p.id ?? ""} />],
  ["/o/:slug/api-keys", "api-keys", (org) => <ApiKeys org={org} />],
  ["/o/:slug/audit", "audit", (org, me) => <Audit org={org} me={me} />],
];

function App() {
  const path = usePath();
  // undefined: still asking; null: signed out; false: couldn't reach the API.
  const [me, setMe] = useState<Me | null | undefined | false>(undefined);

  const loadMe = useCallback(async () => {
    try {
      const r = await api.GET("/me");
      setMe(r.data ?? (r.response.status === 401 ? null : false));
    } catch {
      setMe(false);
    }
  }, []);
  useEffect(() => {
    loadMe();
  }, [loadMe]);

  const signedIn = useCallback(async () => {
    await loadMe();
    navigate(safeNext(new URLSearchParams(location.search).get("next")));
  }, [loadMe]);
  const signedOut = useCallback(() => {
    setMe(null);
    const here = location.pathname + location.search;
    navigate(here === "/" ? "/signin" : `/signin?next=${encodeURIComponent(here)}`);
  }, []);

  // Move focus to the new page's heading so screen readers announce the change.
  const first = useRef(true);
  // biome-ignore lint/correctness/useExhaustiveDependencies: path is the trigger, not a value used
  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    document.querySelector<HTMLElement>("h1")?.focus();
  }, [path]);

  // Redirects: signed-out visitors go to sign-in; signed-in ones skip it.
  const redirect =
    me === null && !PUBLIC.has(path) && path !== "/verify-email"
      ? path === "/"
        ? "/signin"
        : `/signin?next=${encodeURIComponent(path + location.search)}`
      : me && PUBLIC.has(path)
        ? safeNext(new URLSearchParams(location.search).get("next"))
        : null;
  useEffect(() => {
    if (redirect) navigate(redirect, true);
  }, [redirect]);

  if (path === "/verify-email") return <VerifyEmail onVerified={loadMe} />;
  if (me === undefined || redirect) return null;
  if (me === false) {
    return (
      <main>
        <h1 tabIndex={-1}>We can't reach TicketSlinger</h1>
        <p role="alert">Check your connection, then try again.</p>
        <button type="button" onClick={loadMe}>
          Try again
        </button>
      </main>
    );
  }
  if (me === null) {
    return path === "/signup" ? <SignUp onSignedIn={signedIn} /> : <SignIn onSignedIn={signedIn} />;
  }
  switch (path) {
    case "/security":
      return <Security me={me} onChanged={loadMe} onSignedIn={signedIn} onSignedOut={signedOut} />;
    case "/organizers/new":
      return <NewOrganizer me={me} />;
    case "/":
      return <Home me={me} onSignedOut={signedOut} />;
  }
  for (const [pattern, section, page] of ORGANIZER) {
    const p = match(pattern, path);
    if (!p?.slug) continue;
    return (
      // Keyed by organizer so the frame keeps its data while you move around inside it.
      <Organizer key={p.slug} slug={p.slug} section={section} me={me} onSignedOut={signedOut}>
        {/* Keyed by path: each page loads its own data fresh. */}
        {(org) => <Fragment key={path}>{page(org, me, p)}</Fragment>}
      </Organizer>
    );
  }
  return <NotFound />;
}

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
