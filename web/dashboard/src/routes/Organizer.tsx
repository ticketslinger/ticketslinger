import type { components } from "@ticketslinger/api-client";
import { type ReactNode, useEffect } from "react";
import { api, must } from "../api";
import { SuspendedBanner } from "../SuspendedBanner";
import { ErrorText, Link, type Me, NotFound, useLoad } from "../ui";
import { ROLE, SecondStepNotice } from "./Home";

export type Org = components["schemas"]["Organizer"];
export type Section = "events" | "payments" | "team" | "api-keys" | "audit";

const isEditor = (org: Org) => org.role === "owner" || org.role === "admin";

/** Owners and admins change things, unless the organizer is suspended; check-in staff only look. */
export const canEdit = (org: Org) => isEditor(org) && !org.suspended;

/** Support staff read what owners and admins read, and change nothing. */
export const canRead = (org: Org) => isEditor(org) || org.role === "staff";

const NAV: { section: Section; label: string; path: string; editorsOnly?: boolean }[] = [
  { section: "events", label: "Events", path: "" },
  { section: "payments", label: "Payments", path: "/payments" },
  { section: "team", label: "Team", path: "/team", editorsOnly: true },
  { section: "api-keys", label: "API keys", path: "/api-keys", editorsOnly: true },
  { section: "audit", label: "Audit log", path: "/audit", editorsOnly: true },
];

// Team, API keys and Audit log show write controls unconditionally, so staff don't get them.
export const navFor = (org: Org) => NAV.filter((n) => !n.editorsOnly || canEdit(org));

/** The frame around every /o/:slug page: which organizer, and where to go in it. */
export function Organizer({
  slug,
  section,
  me,
  onSignedOut,
  children,
}: {
  slug: string;
  section: Section;
  me: Me;
  onSignedOut: () => void;
  children: (org: Org) => ReactNode;
}) {
  const { data: org, error } = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}", { params: { path: { organizer: slug } } })),
  );

  // The page's heading arrives with the organizer, after the router's focus move.
  useEffect(() => {
    if (org && document.activeElement === document.body) {
      document.querySelector<HTMLElement>("h1")?.focus();
    }
  }, [org]);

  if (error?.status === 404) return <NotFound />;

  return (
    <main className="wide">
      <p className="crumbs">
        <Link href="/">Your organizers</Link>
      </p>
      {error?.code === "mfa_required" ? (
        <>
          <h1 tabIndex={-1}>One more step</h1>
          <SecondStepNotice me={me} onSignedOut={onSignedOut} />
        </>
      ) : error ? (
        <>
          <h1 tabIndex={-1}>We couldn't open this organizer</h1>
          <ErrorText>{error.message}</ErrorText>
        </>
      ) : (
        org && (
          <>
            <p className="org-name">
              {org.name} <span className="muted">· {ROLE[org.role]}</span>
            </p>
            {org.suspended && <SuspendedBanner organizer={org.name} />}
            <nav className="tabs" aria-label={org.name}>
              {navFor(org).map((n) => (
                <Link
                  key={n.section}
                  href={`/o/${org.slug}${n.path}`}
                  aria-current={n.section === section ? "page" : undefined}
                >
                  {n.label}
                </Link>
              ))}
            </nav>
            {children(org)}
          </>
        )
      )}
    </main>
  );
}
