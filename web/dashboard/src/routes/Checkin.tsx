import type { components } from "@ticketslinger/api-client";
import { useEffect, useRef, useState } from "react";
import { ApiFailure, api, must } from "../api";
import { withKey } from "../idempotency";
import { createDetector, type Detect } from "../scan/detector";
import { clock, shouldSend, type View, viewFor } from "../scan/result";
import { ErrorText, Link, useLoad } from "../ui";
import { eventTabs } from "./Events";
import type { Org } from "./Organizer";

type Ticket = components["schemas"]["CheckinTicket"];
type Body = components["schemas"]["CreateCheckin"];

const REFUSED =
  "We can't use your camera. Allow it for this site in your browser settings, or find guests by name below.";
const NO_CAMERA = "This device has no camera we can use. Find guests by name below.";
const OFFLINE = "We couldn't reach TicketSlinger. Check your connection and try again.";

const ICON: Record<View["tone"], string> = { green: "✓", amber: "!", red: "✕", grey: "↻" };
// [Hz, ms] per beep.
const TONES: Partial<Record<View["tone"], [number, number][]>> = {
  green: [[880, 120]],
  amber: [
    [440, 90],
    [440, 90],
  ],
  red: [[220, 400]],
};
const BUZZ: Partial<Record<View["tone"], number | number[]>> = {
  green: 100,
  amber: [100, 80, 100],
  red: 400,
};

const count = (n: number) => n.toLocaleString("en-US");

export function Checkin({ org, id }: { org: Org; id: string }) {
  const path = { organizer: org.slug, event: id };
  const { data: event, error: loadError } = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/events/{event}", { params: { path } })),
  );
  const zone = event?.timezone ?? "UTC";

  const [counts, setCounts] = useState<{ checked_in: number; sold: number }>();
  const [scanning, setScanning] = useState(false);
  const [starting, setStarting] = useState(false);
  const [cameraError, setCameraError] = useState("");
  const [error, setError] = useState("");
  const [wait, setWait] = useState("");
  const [view, setView] = useState<View | null>(null);

  const video = useRef<HTMLVideoElement>(null);
  const stream = useRef<MediaStream | null>(null);
  const audio = useRef<AudioContext | null>(null);
  const resultButton = useRef<HTMLButtonElement>(null);
  const returnFocus = useRef<HTMLElement | null>(null);
  // The scan loop reads these between renders.
  const busy = useRef(false);
  const showing = useRef(false);
  const last = useRef<{ value: string; at: number } | null>(null);
  const waitUntil = useRef(0);
  const key = useRef<string | null>(null);
  const pending = useRef<Body | null>(null);
  const zoneRef = useRef(zone);
  zoneRef.current = zone;

  // The counter refreshes on its own too, for the other phones at the door.
  // biome-ignore lint/correctness/useExhaustiveDependencies: path is rebuilt from props each render
  useEffect(() => {
    const load = () =>
      api
        .GET("/organizers/{organizer}/events/{event}/checkins/summary", { params: { path } })
        .then((r) => r.data && setCounts(r.data))
        .catch(() => {});
    load();
    const t = setInterval(load, 15_000);
    return () => clearInterval(t);
  }, [org.slug, id]);

  // Leaving the page turns the camera and the sound off.
  useEffect(
    () => () => {
      for (const t of stream.current?.getTracks() ?? []) t.stop();
      audio.current?.close().catch(() => {});
    },
    [],
  );

  // Green clears itself; anything that needs a decision takes focus.
  // biome-ignore lint/correctness/useExhaustiveDependencies: dismiss reads only refs and a state setter
  useEffect(() => {
    showing.current = view !== null;
    if (!view) return;
    if (view.autoDismissMs === null) {
      resultButton.current?.focus();
      return;
    }
    const t = setTimeout(dismiss, view.autoDismissMs);
    return () => clearTimeout(t);
  }, [view]);

  function dismiss() {
    if (last.current) last.current = { ...last.current, at: Date.now() };
    setView(null);
    if (returnFocus.current?.isConnected) returnFocus.current.focus();
  }

  // Sound needs a tap first on phones, so the context starts from one.
  function wakeAudio() {
    try {
      audio.current ??= new AudioContext();
      audio.current.resume().catch(() => {});
    } catch {
      // No Web Audio: the screen and the buzz still say it.
    }
  }

  function show(v: View) {
    returnFocus.current =
      document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setView(v);
    navigator.vibrate?.(BUZZ[v.tone] ?? 0);
    const ctx = audio.current;
    if (!ctx) return;
    let at = ctx.currentTime;
    for (const [hz, ms] of TONES[v.tone] ?? []) {
      const osc = ctx.createOscillator();
      const gain = ctx.createGain();
      osc.frequency.value = hz;
      gain.gain.value = 0.2;
      osc.connect(gain).connect(ctx.destination);
      osc.start(at);
      osc.stop(at + ms / 1000);
      at += ms / 1000 + 0.06;
    }
  }

  /** Sends one check-in. A fresh scan gets a fresh key; "Try again" reuses it. */
  async function send(body: Body, fresh: boolean) {
    if (fresh) key.current = null;
    pending.current = body;
    busy.current = true;
    setError("");
    let retryAfter = 0;
    try {
      const r = await withKey(key, async (k) => {
        const res = await api.POST("/organizers/{organizer}/events/{event}/checkins", {
          params: { path, header: { "Idempotency-Key": k } },
          body,
        });
        retryAfter = Number(res.response.headers.get("Retry-After")) || 0;
        return must(res);
      });
      setCounts({ checked_in: r.checked_in, sold: r.sold });
      show(viewFor(r, zoneRef.current));
    } catch (e) {
      if (e instanceof ApiFailure && e.status === 429) {
        // Too many scans: a short pause, not a red screen.
        const ms = Math.min(retryAfter || 5, 60) * 1000;
        waitUntil.current = Date.now() + ms;
        setWait(e.message);
        setTimeout(() => setWait(""), ms);
      } else if (!(e instanceof ApiFailure) || e.status === 0 || e.status >= 500) {
        show(viewFor("network", zoneRef.current));
      } else {
        setError(e.message);
      }
    } finally {
      busy.current = false;
      if (body.qr && last.current?.value === body.qr) last.current.at = Date.now();
    }
  }

  async function start() {
    if (starting) return;
    setStarting(true);
    wakeAudio();
    setCameraError("");
    let s: MediaStream;
    try {
      if (!navigator.mediaDevices?.getUserMedia) throw new DOMException("", "NotFoundError");
      s = await navigator.mediaDevices.getUserMedia({ video: { facingMode: "environment" } });
    } catch (e) {
      setCameraError(
        e instanceof DOMException && e.name === "NotAllowedError" ? REFUSED : NO_CAMERA,
      );
      setStarting(false);
      return;
    }
    let detect: Detect | null = null;
    try {
      detect = await createDetector();
    } catch {
      setError(OFFLINE);
    }
    const v = video.current;
    // The page may have closed while we waited for the camera or the decoder.
    if (!detect || !v) {
      for (const t of s.getTracks()) t.stop();
      if (v) setStarting(false);
      return;
    }
    stream.current = s;
    v.srcObject = s;
    await v.play().catch(() => {});
    setScanning(true);
    loop(v, detect);
  }

  function loop(v: HTMLVideoElement, detect: Detect) {
    const tick = async () => {
      if (!v.isConnected || stream.current?.active === false) return;
      if (
        !busy.current &&
        !showing.current &&
        Date.now() >= waitUntil.current &&
        v.readyState >= 2
      ) {
        busy.current = true;
        let values: string[] = [];
        try {
          values = await detect(v);
        } catch {
          // A frame that can't be read is just skipped.
        }
        busy.current = false;
        const value = values[0];
        const now = Date.now();
        if (value && !showing.current && shouldSend(value, last.current, now)) {
          last.current = { value, at: now };
          await send({ qr: value }, true);
        }
      }
      setTimeout(tick, 150);
    };
    tick();
  }

  return (
    <>
      <p className="crumbs">
        <Link href={`/o/${org.slug}`}>All events</Link>
      </p>
      <h1 tabIndex={-1}>Check guests in</h1>
      {loadError && <ErrorText>{loadError.message}</ErrorText>}
      {event && (
        <>
          <p className="muted">{event.name}</p>
          <nav className="tabs" aria-label="Event sections">
            {eventTabs(org).map((t) => (
              <Link
                key={t.label}
                href={`/o/${org.slug}/events/${id}${t.path}`}
                aria-current={t.path === "/checkin" ? "page" : undefined}
              >
                {t.label}
              </Link>
            ))}
          </nav>
        </>
      )}

      {counts && (
        <p className="scan-count">
          {count(counts.checked_in)} of {count(counts.sold)} checked in
        </p>
      )}

      {!scanning && (
        <button type="button" className="inline" disabled={starting} onClick={start}>
          Start scanning
        </button>
      )}
      {cameraError && (
        <p className="notice" role="alert">
          {cameraError}
        </p>
      )}
      <video ref={video} className="scan-video" muted playsInline hidden={!scanning} />
      <p role="status" className="muted">
        {wait}
      </p>
      <ErrorText>{error}</ErrorText>

      <div aria-live="assertive" aria-atomic="true">
        {view && (
          <section className={`scan-result ${view.tone}`} aria-label={view.heading}>
            <span className="scan-icon" aria-hidden="true">
              {ICON[view.tone]}
            </span>
            <h2 className="scan-heading">{view.heading}</h2>
            <p className="scan-body">{view.body}</p>
            {view.action && (
              <button
                type="button"
                className="scan-action"
                ref={resultButton}
                onClick={() => {
                  if (view.tone === "grey" && pending.current) {
                    setView(null);
                    send(pending.current, false);
                  } else dismiss();
                }}
              >
                {view.action}
              </button>
            )}
          </section>
        )}
      </div>

      <GuestSearch
        path={path}
        zone={zone}
        onCheckIn={async (ticketId) => {
          wakeAudio();
          return send({ ticket_id: ticketId }, true);
        }}
      />
    </>
  );
}

function GuestSearch({
  path,
  zone,
  onCheckIn,
}: {
  path: { organizer: string; event: string };
  zone: string;
  onCheckIn: (ticketId: string) => Promise<void>;
}) {
  const [q, setQ] = useState("");
  const [found, setFound] = useState<{ q: string; rows: Ticket[] } | null>(null);
  const [error, setError] = useState("");
  const [again, setAgain] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const query = q.trim();

  // biome-ignore lint/correctness/useExhaustiveDependencies: path is rebuilt from props each render; again re-runs the search
  useEffect(() => {
    if (query.length < 2) {
      setFound(null);
      return;
    }
    let live = true;
    const t = setTimeout(async () => {
      try {
        const rows = must(
          await api.GET("/organizers/{organizer}/events/{event}/checkins/search", {
            params: { path, query: { q } },
          }),
        );
        if (live) {
          setFound({ q: query, rows });
          setError("");
        }
      } catch (e) {
        if (live) setError(e instanceof ApiFailure ? e.message : OFFLINE);
      }
    }, 300);
    return () => {
      live = false;
      clearTimeout(t);
    };
  }, [q, again, path.organizer, path.event]);

  return (
    <section className="guest-search">
      <div className="field">
        <label htmlFor="guest-q">Find a guest</label>
        <input
          ref={input}
          id="guest-q"
          type="search"
          maxLength={120}
          autoComplete="off"
          aria-describedby="guest-q-hint"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <p className="hint" id="guest-q-hint">
          Name, email or order code
        </p>
      </div>
      <ErrorText>{error}</ErrorText>
      {found && found.rows.length === 0 && <p>No tickets match "{found.q}".</p>}
      {found && found.rows.length > 0 && (
        <ul className="guest-list">
          {found.rows.map((t) => {
            const name = t.attendee_name || "Guest";
            return (
              <li key={t.id} className="guest-row">
                <div id={`guest-${t.id}`}>
                  <strong>{name}</strong>
                  <span className="muted">
                    {" "}
                    · {t.ticket_type_name} · {t.order_code}
                  </span>
                  <br />
                  {t.status === "void"
                    ? "Cancelled"
                    : t.checked_in_at
                      ? `Checked in at ${clock(t.checked_in_at, zone)}`
                      : "Not checked in"}
                </div>
                {t.status === "valid" && !t.checked_in_at && (
                  <button
                    type="button"
                    className="inline"
                    aria-describedby={`guest-${t.id}`}
                    onClick={async () => {
                      // This button goes once the guest is in; the search box is where they'll go next.
                      input.current?.focus();
                      await onCheckIn(t.id);
                      setAgain((n) => n + 1);
                    }}
                  >
                    Check in {name}
                  </button>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}
