import { useState } from "react";
import { api, must } from "../api";
import { ErrorText, focusOnMount, useAction, useLoad } from "../ui";
import { canEdit, type Org } from "./Organizer";

/** Back from Stripe's onboarding page, which adds ?return=1 (finished) or ?refresh=1 (link expired). */
export const comingBack = (search: string) => {
  const q = new URLSearchParams(search);
  return q.has("return") || q.has("refresh");
};

export function Payments({ org }: { org: Org }) {
  const path = { organizer: org.slug };
  const { data, setData, error } = useLoad(async () => {
    const status = must(await api.GET("/organizers/{organizer}/payments", { params: { path } }));
    // Stripe is the one who knows if onboarding finished, so ask it.
    if (comingBack(location.search) && canEdit(org) && status.connected) {
      return must(await api.POST("/organizers/{organizer}/payments/refresh", { params: { path } }));
    }
    return status;
  });
  const action = useAction();
  // null until asked; false after "Keep it connected", so focus can go back.
  const [confirming, setConfirming] = useState<boolean | null>(null);
  const goTo = (op: "/connect" | "/link") =>
    action.run(async () => {
      const { url } = must(
        op === "/connect"
          ? await api.POST("/organizers/{organizer}/payments/connect", { params: { path } })
          : await api.POST("/organizers/{organizer}/payments/link", { params: { path } }),
      );
      window.location.assign(url);
    });
  const refresh = () =>
    action.run(async () => {
      setData(
        must(await api.POST("/organizers/{organizer}/payments/refresh", { params: { path } })),
      );
    });
  const disconnect = () =>
    action.run(async () => {
      setData(
        must(await api.POST("/organizers/{organizer}/payments/disconnect", { params: { path } })),
      );
      setConfirming(null);
    });
  const editor = canEdit(org);
  const reconnect = data?.disconnected && data.link_method === "created";
  // The server sells through its own Stripe account: nothing to connect.
  const direct = data?.mode === "direct";

  return (
    <>
      <h1 tabIndex={-1}>Payments</h1>
      <ErrorText>{error?.message}</ErrorText>
      {!data && !error && <p className="muted">Loading…</p>}
      {data?.provider === "none" && <p>Payments aren't set up on this server yet.</p>}
      {direct && data.provider !== "none" && (
        <p className="lede">
          This server takes card payments through its own Stripe account. There's nothing to
          connect, and paid tickets can go on sale straight away.
        </p>
      )}
      {data && data.provider !== "none" && !direct && !data.connected && (
        <>
          {data.disconnected ? (
            <p className="lede">
              Payments are off for now. Paid tickets stay off sale until you connect Stripe again.
              Orders you've already taken stay as they are.
            </p>
          ) : (
            <p className="lede">
              Connect Stripe to sell paid tickets. Buyers pay you directly through your own Stripe
              account, and the money lands in your bank on Stripe's usual schedule. Free tickets
              work without it.
            </p>
          )}
          {editor && (
            <>
              <div className="row">
                <button type="button" disabled={action.busy} onClick={() => goTo("/connect")}>
                  {reconnect ? "Reconnect your Stripe account" : "Create a Stripe account"}
                </button>
                {data.can_link_existing && (
                  <button
                    type="button"
                    className="secondary"
                    disabled={action.busy}
                    onClick={() => goTo("/link")}
                  >
                    Link an existing Stripe account
                  </button>
                )}
              </div>
              {data.can_link_existing && (
                <p className="muted">
                  Already sell with Stripe? Link that account and the money goes where it always
                  does.
                </p>
              )}
            </>
          )}
          {!editor && <p className="muted">Ask an owner or admin to connect it.</p>}
        </>
      )}
      {data?.connected && data.charges_enabled && (
        <p className="success">You're ready to take payments.</p>
      )}
      {data?.connected && !data.charges_enabled && (
        <>
          <p className="lede">
            Stripe still needs a few details from you.
            {data.link_method === "linked" &&
              " Finish them in your Stripe dashboard, then check again."}
          </p>
          {editor && (
            <div className="row">
              {data.link_method === "linked" ? (
                <button type="button" disabled={action.busy} onClick={refresh}>
                  Check again
                </button>
              ) : (
                <button type="button" disabled={action.busy} onClick={() => goTo("/connect")}>
                  Finish setting up Stripe
                </button>
              )}
            </div>
          )}
        </>
      )}
      {data?.connected && editor && (
        <section className="danger">
          {confirming ? (
            <>
              <p>
                Buyers won't be able to pay for tickets until you connect again. Orders you've
                already taken stay as they are.
                {data.link_method === "created" &&
                  " Your Stripe account stays yours; we just stop selling through it."}
              </p>
              <div className="row">
                <button
                  type="button"
                  className="inline"
                  ref={focusOnMount}
                  disabled={action.busy}
                  onClick={disconnect}
                >
                  Disconnect Stripe
                </button>
                <button
                  type="button"
                  className="inline secondary"
                  onClick={() => setConfirming(false)}
                >
                  Keep it connected
                </button>
              </div>
            </>
          ) : (
            <button
              type="button"
              className="inline secondary"
              ref={confirming === false ? focusOnMount : undefined}
              onClick={() => setConfirming(true)}
            >
              Disconnect Stripe
            </button>
          )}
        </section>
      )}
      <ErrorText>{action.error}</ErrorText>
    </>
  );
}
