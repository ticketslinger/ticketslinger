import { useEffect, useState } from "react";
import { ApiFailure, api, must } from "./api";
import { FormDialog } from "./forms";
import { type Me, navigate, useAction } from "./ui";

type StaffSession = NonNullable<Me["staff_session"]>;

/** Whole minutes left, rounded down, never below 0. */
export const minutesLeft = (expiresAt: string, now = Date.now()) =>
  Math.max(0, Math.floor((new Date(expiresAt).getTime() - now) / 60_000));

export const bannerText = (s: Pick<StaffSession, "organizer_name">, n: number) =>
  `Support mode: viewing ${s.organizer_name}, read-only. ${n} min left.`;

/** Full width, not dismissible, on every page while support mode runs. */
export function StaffBanner({
  session,
  onChanged,
}: {
  session: StaffSession;
  onChanged: () => void;
}) {
  const [now, setNow] = useState(Date.now());
  const [ending, setEnding] = useState(false);
  const [note, setNote] = useState("");
  const end = useAction();
  const left = minutesLeft(session.expires_at, now);

  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(t);
  }, []);
  // The hour ran out here: ask the server. The app goes back to /staff once /me says so,
  // so a fast clock on this device can't bounce someone out of a session that still runs.
  useEffect(() => {
    if (new Date(session.expires_at).getTime() <= now) onChanged();
  }, [session.expires_at, now, onChanged]);

  return (
    <>
      <div className="staff-banner" role="status">
        <p>{bannerText(session, left)}</p>
        <button type="button" onClick={() => setEnding(true)}>
          End
        </button>
      </div>
      {ending && (
        <FormDialog
          title="End support mode"
          dirty={false}
          onClose={() => setEnding(false)}
          busy={end.busy}
          submitLabel="End support mode"
          onSubmit={() =>
            end.run(async () => {
              try {
                must(
                  await api.DELETE("/staff/sessions/current", {
                    body: note.trim() ? { note: note.trim() } : {},
                  }),
                );
              } catch (e) {
                // 404: it already ended (expired, or ended in another tab). Same destination.
                if (!(e instanceof ApiFailure && e.status === 404)) throw e;
              }
              setEnding(false);
              onChanged();
              navigate("/staff", false, true);
            })
          }
        >
          <div className="field">
            <label htmlFor="end-note">Note (optional)</label>
            <textarea
              id="end-note"
              maxLength={2000}
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
          </div>
          <p className="error" role="alert">
            {end.error}
          </p>
        </FormDialog>
      )}
    </>
  );
}
