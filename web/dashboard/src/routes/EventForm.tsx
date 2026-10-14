import type { components } from "@ticketslinger/api-client";
import { useState } from "react";
import { api, must } from "../api";
import { browserZone, fromLocalInput, timeZones, toLocalInput } from "../time";
import { ErrorText, Field, navigate, useAction } from "../ui";
import { slugify } from "./NewOrganizer";
import { canEdit, type Org } from "./Organizer";

export type Event = components["schemas"]["Event"];

/** 825 → "8.25". Integer maths, so no float ever touches the rate. */
export function bpToPercent(bp: number): string {
  const frac = String(bp % 100)
    .padStart(2, "0")
    .replace(/0+$/, "");
  return frac ? `${Math.trunc(bp / 100)}.${frac}` : String(Math.trunc(bp / 100));
}

/** "8.25" → 825, read as text so 0.29 can't turn into 28.999…; null if it isn't a 0–100 percent. */
export function percentToBp(s: string): number | null {
  const m = /^(\d{0,3})(?:\.(\d{1,2}))?$/.exec(s.trim());
  if (!m) return null;
  const bp = Number(m[1] || "0") * 100 + Number((m[2] ?? "").padEnd(2, "0"));
  return bp <= 10000 ? bp : null;
}

let zones: string[] | undefined;

type Draft = {
  name: string;
  slug: string;
  description: string;
  venue_name: string;
  venue_address: string;
  timezone: string;
  starts: string;
  ends: string;
  fee_mode: Event["fee_mode"];
  tax: string;
  prices_include_tax: boolean;
};

const draftOf = (e?: Event): Draft =>
  e
    ? {
        ...e,
        starts: toLocalInput(e.starts_at, e.timezone),
        ends: toLocalInput(e.ends_at, e.timezone),
        tax: bpToPercent(e.tax_rate_bp),
      }
    : {
        name: "",
        slug: "",
        description: "",
        venue_name: "",
        venue_address: "",
        timezone: browserZone(),
        starts: "",
        ends: "",
        fee_mode: "pass",
        tax: "0",
        prices_include_tax: false,
      };

/** Creates an event, or edits one when `event` is given. Check-in staff see it read-only. */
export function EventForm({
  org,
  event,
  onSaved,
}: {
  org: Org;
  event?: Event;
  onSaved?: (e: Event) => void;
}) {
  const [d, setD] = useState(() => draftOf(event));
  const [slugEdited, setSlugEdited] = useState(Boolean(event));
  const [saved, setSaved] = useState(false);
  const { busy, error, setError, run } = useAction();
  const set = (patch: Partial<Draft>) => {
    setSaved(false);
    setD((old) => ({ ...old, ...patch }));
  };
  zones ??= timeZones();
  const zoneList = zones.includes(d.timezone) ? zones : [d.timezone, ...zones];
  const slugLocked = event !== undefined && event.status !== "draft";

  function save() {
    const tax_rate_bp = percentToBp(d.tax);
    if (tax_rate_bp === null) {
      setError("Use a tax rate from 0 to 100, with up to two decimals, like 8.25.");
      return;
    }
    const body = {
      name: d.name,
      description: d.description,
      venue_name: d.venue_name,
      venue_address: d.venue_address,
      timezone: d.timezone,
      starts_at: fromLocalInput(d.starts, d.timezone),
      ends_at: fromLocalInput(d.ends, d.timezone),
      fee_mode: d.fee_mode,
      tax_rate_bp,
      prices_include_tax: d.prices_include_tax,
    };
    run(async () => {
      const path = { organizer: org.slug };
      if (!event) {
        const e = must(
          await api.POST("/organizers/{organizer}/events", {
            params: { path },
            body: { ...body, slug: d.slug },
          }),
        );
        navigate(`/o/${org.slug}/events/${e.id}`);
        return;
      }
      const e = must(
        await api.PATCH("/organizers/{organizer}/events/{event}", {
          params: { path: { ...path, event: event.id } },
          body: slugLocked ? body : { ...body, slug: d.slug },
        }),
      );
      setSaved(true);
      onSaved?.(e);
    });
  }

  return (
    <form
      className="event-form"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <fieldset className="plain" disabled={!canEdit(org)}>
        <Field
          id="event-name"
          label="Event name"
          required
          maxLength={160}
          value={d.name}
          onChange={(e) =>
            set({
              name: e.target.value,
              ...(slugEdited ? {} : { slug: slugify(e.target.value, 60) }),
            })
          }
        />
        <Field
          id="event-slug"
          label="Web address"
          hint={
            slugLocked
              ? "This is fixed while the event is on sale. Move it back to a draft to change it."
              : "Lowercase letters, numbers and single hyphens. We fill it in from the name, and you can change it while the event is a draft."
          }
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          required
          minLength={3}
          maxLength={60}
          pattern="[a-z0-9](-?[a-z0-9])*"
          readOnly={slugLocked}
          value={d.slug}
          onChange={(e) => {
            set({ slug: e.target.value });
            setSlugEdited(e.target.value !== "");
          }}
        />

        <div className="field">
          <label htmlFor="event-timezone">Time zone</label>
          <select
            id="event-timezone"
            aria-describedby="event-timezone-hint"
            value={d.timezone}
            onChange={(e) => set({ timezone: e.target.value })}
          >
            {zoneList.map((z) => (
              <option key={z} value={z}>
                {z.replaceAll("_", " ")}
              </option>
            ))}
          </select>
          <p className="hint" id="event-timezone-hint">
            Where the event happens. The times below are in this zone, wherever you are.
          </p>
        </div>
        <div className="pair">
          <Field
            id="event-starts"
            label="Starts"
            type="datetime-local"
            required
            value={d.starts}
            onChange={(e) => set({ starts: e.target.value })}
          />
          <Field
            id="event-ends"
            label="Ends"
            type="datetime-local"
            required
            value={d.ends}
            onChange={(e) => set({ ends: e.target.value })}
          />
        </div>

        <Field
          id="venue-name"
          label="Venue"
          maxLength={160}
          value={d.venue_name}
          onChange={(e) => set({ venue_name: e.target.value })}
        />
        <div className="field">
          <label htmlFor="venue-address">Venue address</label>
          <textarea
            id="venue-address"
            rows={3}
            maxLength={500}
            value={d.venue_address}
            onChange={(e) => set({ venue_address: e.target.value })}
          />
        </div>
        <div className="field">
          <label htmlFor="event-description">Description</label>
          <textarea
            id="event-description"
            rows={6}
            maxLength={20000}
            value={d.description}
            onChange={(e) => set({ description: e.target.value })}
          />
        </div>

        <fieldset className="choices">
          <legend>Service fee</legend>
          <label className="check">
            <input
              type="radio"
              name="fee_mode"
              checked={d.fee_mode === "pass"}
              onChange={() => set({ fee_mode: "pass" })}
            />
            <span>
              Buyers pay the service fee
              <span className="hint">It goes on top of your ticket price.</span>
            </span>
          </label>
          <label className="check">
            <input
              type="radio"
              name="fee_mode"
              checked={d.fee_mode === "absorb"}
              onChange={() => set({ fee_mode: "absorb" })}
            />
            <span>
              We absorb the service fee
              <span className="hint">It comes out of your ticket price instead.</span>
            </span>
          </label>
        </fieldset>

        <div className="pair">
          <Field
            id="event-tax"
            label="Default tax rate (%)"
            hint="Used for every ticket type that doesn't set its own. Leave it at 0 if you don't charge tax."
            inputMode="decimal"
            required
            value={d.tax}
            onChange={(e) => set({ tax: e.target.value })}
          />
        </div>
        <label className="check">
          <input
            type="checkbox"
            checked={d.prices_include_tax}
            onChange={(e) => set({ prices_include_tax: e.target.checked })}
          />
          <span>Ticket prices already include tax</span>
        </label>

        <ErrorText>{error}</ErrorText>
        <p className="success" role="status">
          {saved ? "Saved." : ""}
        </p>
        {canEdit(org) && (
          <button type="submit" className="inline" disabled={busy}>
            {event ? "Save changes" : "Create event"}
          </button>
        )}
      </fieldset>
    </form>
  );
}
