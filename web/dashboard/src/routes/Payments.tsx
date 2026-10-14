import { api, must } from "../api";
import { ErrorText, useAction, useLoad } from "../ui";
import { canEdit, type Org } from "./Organizer";

/** Back from Stripe's onboarding page, which adds ?return=1 (finished) or ?refresh=1 (link expired). */
export const comingBack = (search: string) => {
  const q = new URLSearchParams(search);
  return q.has("return") || q.has("refresh");
};

export function Payments({ org }: { org: Org }) {
  const path = { organizer: org.slug };
  const { data, error } = useLoad(async () => {
    const status = must(await api.GET("/organizers/{organizer}/payments", { params: { path } }));
    // Stripe is the one who knows if onboarding finished, so ask it.
    if (comingBack(location.search) && canEdit(org) && status.connected) {
      return must(await api.POST("/organizers/{organizer}/payments/refresh", { params: { path } }));
    }
    return status;
  });
  const connect = useAction();
  const go = () =>
    connect.run(async () => {
      const { url } = must(
        await api.POST("/organizers/{organizer}/payments/connect", { params: { path } }),
      );
      window.location.assign(url);
    });
  const button = (label: string) =>
    canEdit(org) && (
      <button type="button" disabled={connect.busy} onClick={go}>
        {label}
      </button>
    );

  return (
    <>
      <h1 tabIndex={-1}>Payments</h1>
      <ErrorText>{error?.message}</ErrorText>
      {!data && !error && <p className="muted">Loading…</p>}
      {data?.provider === "none" && <p>Payments aren't set up on this server yet.</p>}
      {data && data.provider !== "none" && !data.connected && (
        <>
          <p className="lede">
            Connect Stripe to sell paid tickets. Buyers pay you directly through your own Stripe
            account, and the money lands in your bank on Stripe's usual schedule. Free tickets work
            without it.
          </p>
          <div className="row">{button("Connect Stripe")}</div>
        </>
      )}
      {data?.connected && data.charges_enabled && (
        <p className="success">You're ready to take payments.</p>
      )}
      {data?.connected && !data.charges_enabled && (
        <>
          <p className="lede">Stripe still needs a few details from you.</p>
          <div className="row">{button("Finish setting up Stripe")}</div>
        </>
      )}
      {data && data.provider !== "none" && !canEdit(org) && !data.connected && (
        <p className="muted">Ask an owner or admin to connect it.</p>
      )}
      <ErrorText>{connect.error}</ErrorText>
    </>
  );
}
