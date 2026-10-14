import { useState } from "react";
import { api, must } from "../api";
import { formatWhen } from "../time";
import { ErrorText, Link, navigate, useAction, useLoad } from "../ui";
import { type Event, EventForm } from "./EventForm";
import { canEdit, type Org } from "./Organizer";

const STATUS: Record<Event["status"], string> = {
  draft: "Draft",
  live: "On sale",
  ended: "Ended",
  cancelled: "Cancelled",
};

const Status = ({ status }: { status: Event["status"] }) => (
  <span className={`pill ${status}`}>{STATUS[status]}</span>
);

export function Events({ org }: { org: Org }) {
  const { data: events, error } = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events", {
        params: { path: { organizer: org.slug } },
      }),
    ),
  );

  return (
    <>
      <div className="heading-row">
        <h1 tabIndex={-1}>Events</h1>
        {canEdit(org) && (
          <Link className="button inline" href={`/o/${org.slug}/events/new`}>
            Create an event
          </Link>
        )}
      </div>
      <ErrorText>{error?.message}</ErrorText>
      {!events && !error && <p className="muted">Loading…</p>}
      {events?.length === 0 &&
        (canEdit(org) ? (
          <p>No events yet. A new one starts as a draft, so nobody sees it until you say so.</p>
        ) : (
          <p>No events yet. They'll show up here once someone on your team creates one.</p>
        ))}
      {events && events.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Event</th>
                <th scope="col">Starts</th>
                <th scope="col">Status</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e) => (
                <tr key={e.id}>
                  <td>
                    <Link href={`/o/${org.slug}/events/${e.id}`}>{e.name}</Link>
                  </td>
                  <td>
                    {formatWhen(e.starts_at, e.timezone)}{" "}
                    <span className="muted">{e.timezone.replaceAll("_", " ")}</span>
                  </td>
                  <td>
                    <Status status={e.status} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

export function NewEvent({ org }: { org: Org }) {
  return (
    <>
      <p className="crumbs">
        <Link href={`/o/${org.slug}`}>All events</Link>
      </p>
      <h1 tabIndex={-1}>Create an event</h1>
      <p className="muted">It starts as a draft, and nothing goes on sale until you say so.</p>
      <EventForm org={org} />
    </>
  );
}

// Tasks for tickets, promo codes and questions add their tabs here.
const TABS = [{ path: "", label: "Details" }];

export function EventPage({ org, id }: { org: Org; id: string }) {
  const path = { organizer: org.slug, event: id };
  const {
    data: event,
    setData: setEvent,
    error,
  } = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/events/{event}", { params: { path } })),
  );
  const status = useAction();
  const remove = useAction();
  const [confirming, setConfirming] = useState(false);

  const setStatus = (to: "draft" | "live") =>
    status.run(async () => {
      setEvent(
        must(
          await api.PATCH("/organizers/{organizer}/events/{event}", {
            params: { path },
            body: { status: to },
          }),
        ),
      );
    });

  return (
    <>
      <p className="crumbs">
        <Link href={`/o/${org.slug}`}>All events</Link>
      </p>
      <div className="heading-row">
        <h1 tabIndex={-1}>
          {error?.status === 404 ? "There's no such event" : (event?.name ?? "Event")}
        </h1>
        {event && <Status status={event.status} />}
      </div>
      {error?.status === 404 ? (
        <p>It may have been deleted while it was a draft.</p>
      ) : (
        <ErrorText>{error?.message}</ErrorText>
      )}
      {event && (
        <>
          <p className="muted">
            {formatWhen(event.starts_at, event.timezone)} · {event.timezone.replaceAll("_", " ")}
          </p>
          {canEdit(org) && (event.status === "draft" || event.status === "live") && (
            <div className="status-box">
              {event.status === "draft" ? (
                <>
                  <p>This event is a draft. Go live when its tickets are ready to sell.</p>
                  <button
                    type="button"
                    className="inline"
                    disabled={status.busy}
                    onClick={() => setStatus("live")}
                  >
                    Go live
                  </button>
                </>
              ) : (
                <>
                  <p>This event is on sale.</p>
                  <button
                    type="button"
                    className="inline secondary"
                    disabled={status.busy}
                    onClick={() => setStatus("draft")}
                  >
                    Back to draft
                  </button>
                </>
              )}
              <ErrorText>{status.error}</ErrorText>
            </div>
          )}

          <nav className="tabs" aria-label="Event sections">
            {TABS.map((t) => (
              <Link
                key={t.label}
                href={`/o/${org.slug}/events/${event.id}${t.path}`}
                aria-current="page"
              >
                {t.label}
              </Link>
            ))}
          </nav>
          <EventForm org={org} event={event} onSaved={setEvent} />

          {canEdit(org) && event.status === "draft" && (
            <section className="danger">
              {confirming ? (
                <>
                  <p>
                    This removes the event along with its tickets, promo codes and questions.
                    There's no undo.
                  </p>
                  <div className="row">
                    <button
                      type="button"
                      className="inline"
                      disabled={remove.busy}
                      onClick={() =>
                        remove.run(async () => {
                          must(
                            await api.DELETE("/organizers/{organizer}/events/{event}", {
                              params: { path },
                            }),
                          );
                          navigate(`/o/${org.slug}`);
                        })
                      }
                    >
                      Delete event
                    </button>
                    <button
                      type="button"
                      className="inline secondary"
                      onClick={() => setConfirming(false)}
                    >
                      Keep it
                    </button>
                  </div>
                </>
              ) : (
                <button
                  type="button"
                  className="inline secondary"
                  onClick={() => setConfirming(true)}
                >
                  Delete this draft
                </button>
              )}
              <ErrorText>{remove.error}</ErrorText>
            </section>
          )}
        </>
      )}
    </>
  );
}
