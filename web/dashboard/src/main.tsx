import { StrictMode, useCallback, useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { api } from "./api";
import { Home } from "./routes/Home";
import { NewOrganizer } from "./routes/NewOrganizer";
import { Security } from "./routes/Security";
import { SignIn } from "./routes/SignIn";
import { SignUp } from "./routes/SignUp";
import { VerifyEmail } from "./routes/VerifyEmail";
import { type Me, navigate, safeNext, usePath } from "./ui";
import "./styles.css";

const PUBLIC = new Set(["/signin", "/signup"]);

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
    navigate("/signin");
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
    default:
      return (
        <main>
          <h1 tabIndex={-1}>There's nothing here</h1>
          <p>
            <a href="/">Go to your dashboard</a>
          </p>
        </main>
      );
  }
}

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
