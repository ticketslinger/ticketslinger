import type { components } from "@ticketslinger/api-client";

export type CheckinResult = components["schemas"]["CheckinResult"];

export type View = {
  tone: "green" | "amber" | "red" | "grey";
  heading: string;
  body: string;
  action: string | null;
  autoDismissMs: number | null;
};

const NEXT = "Scan the next ticket";

/** A time of day as "7:42 PM" in the event's zone. */
export function clock(iso: string, zone: string): string {
  return new Intl.DateTimeFormat("en-US", { timeZone: zone, hour: "numeric", minute: "2-digit" })
    .format(new Date(iso))
    .replace(/\s/g, " ");
}

/** What the door sees after a scan, or after one that never reached us. */
export function viewFor(r: CheckinResult | "network", zone: string): View {
  if (r === "network") {
    return {
      tone: "grey",
      heading: "That scan didn't reach us",
      body: "Check your signal, then try again.",
      action: "Try again",
      autoDismissMs: null,
    };
  }
  const t = r.ticket;
  switch (r.result) {
    case "accepted":
      return {
        tone: "green",
        heading: "Welcome in",
        body: `${t?.attendee_name || "Guest"} · ${t?.ticket_type_name ?? ""}`,
        action: null,
        autoDismissMs: 2500,
      };
    case "duplicate":
      return {
        tone: "amber",
        heading: "Already checked in",
        body: `This ticket came in at ${t?.checked_in_at ? clock(t.checked_in_at, zone) : "an earlier scan"}. Make sure it's the same person before you let them through.`,
        action: NEXT,
        autoDismissMs: null,
      };
    case "void":
      return {
        tone: "red",
        heading: "This ticket was cancelled",
        body: "It was refunded or its order was cancelled, so it can't get anyone in.",
        action: NEXT,
        autoDismissMs: null,
      };
    default:
      return {
        tone: "red",
        heading: "Not a ticket for this event",
        body: "It may be for another event, or not one of ours. Ask to see the ticket email or PDF.",
        action: NEXT,
        autoDismissMs: null,
      };
  }
}

/** False while the same value is still inside its 3-second quiet spell. */
export function shouldSend(
  value: string,
  last: { value: string; at: number } | null,
  now: number,
): boolean {
  return !last || last.value !== value || now - last.at >= 3_000;
}
