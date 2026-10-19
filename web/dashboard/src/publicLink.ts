/** Where buyers find an event: <storefront origin>/<organizer slug>/<event slug>. "" when no origin is set. */
export function publicEventUrl(origin: string, organizer: string, event: string): string {
  const base = origin.replace(/\/+$/, "");
  return base ? `${base}/${encodeURIComponent(organizer)}/${encodeURIComponent(event)}` : "";
}

/** What the event page shows: the link (live), a note that it's coming (draft), or nothing. */
export function publicLinkState(status: string, url: string): "link" | "draft" | "none" {
  if (status === "draft") return "draft";
  return status === "live" && url ? "link" : "none";
}

let origin: Promise<string> | undefined;

/** The storefront origin from /config.json, fetched once; "" when it isn't configured. */
export function storefrontUrl(): Promise<string> {
  origin ??= fetch("/config.json")
    .then((r) => r.json() as Promise<{ storefrontUrl?: string }>)
    .then(
      (c) => c.storefrontUrl ?? "",
      () => "",
    );
  return origin;
}
