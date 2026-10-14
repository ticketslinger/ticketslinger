import type { components } from "@ticketslinger/api-client";
import { useState } from "react";
import { api, must } from "../api";
import { ErrorText, Field, focusOnMount, useAction, useLoad } from "../ui";
import type { Org } from "./Organizer";

type Key = components["schemas"]["ApiKey"];

const ACCESS = { read: "Read only", write: "Read and write" } as const;
const when = (iso: string) =>
  new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(
    new Date(iso),
  );

export function ApiKeys({ org }: { org: Org }) {
  const path = { organizer: org.slug };
  const {
    data: keys,
    error,
    reload,
  } = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/api-keys", { params: { path } })),
  );
  const [name, setName] = useState("");
  const [scope, setScope] = useState<Key["scope"]>("read");
  const [token, setToken] = useState("");
  const [copied, setCopied] = useState("");
  const create = useAction();
  const active = keys?.filter((k) => !k.revoked_at);

  return (
    <>
      <h1 tabIndex={-1}>API keys</h1>
      <p className="lede">
        A key lets your own software work with {org.name} through the API. Treat each one like a
        password, and give every tool its own so you can switch one off without touching the rest.
      </p>

      {token && (
        // Focus moves here so the one-time key isn't missed; keyed so a second key does it again.
        <div className="notice" key={token} tabIndex={-1} ref={focusOnMount}>
          <p>
            <strong>Here's your new key.</strong> Copy it now: we only keep a scrambled version, so
            we can't show it to you again.
          </p>
          <label htmlFor="new-key">Your new key</label>
          <input
            id="new-key"
            className="secret"
            readOnly
            value={token}
            onFocus={(e) => e.target.select()}
          />
          <div className="row">
            <button
              type="button"
              className="inline"
              onClick={() =>
                (navigator.clipboard?.writeText(token) ?? Promise.reject()).then(
                  () => setCopied("Copied."),
                  () => setCopied("Copying didn't work here. Select the key and copy it yourself."),
                )
              }
            >
              Copy
            </button>
            <button
              type="button"
              className="inline secondary"
              onClick={() => {
                setToken("");
                setCopied("");
              }}
            >
              I've saved it
            </button>
          </div>
          <p className="success" role="status">
            {copied}
          </p>
        </div>
      )}

      <ErrorText>{error?.message}</ErrorText>
      {!keys && !error && <p className="muted">Loading…</p>}
      {active?.length === 0 && <p>No keys yet. Create one below when you need it.</p>}
      {active && active.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Starts with</th>
                <th scope="col">Access</th>
                <th scope="col">Last used</th>
                <th scope="col">
                  <span className="visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {active.map((k) => (
                <KeyRow key={k.id} k={k} org={org} onRevoked={reload} />
              ))}
            </tbody>
          </table>
        </div>
      )}

      <section>
        <h2>Create a key</h2>
        <form
          className="narrow"
          onSubmit={(e) => {
            e.preventDefault();
            create.run(async () => {
              const r = must(
                await api.POST("/organizers/{organizer}/api-keys", {
                  params: { path },
                  body: { name, scope },
                }),
              );
              setToken(r.token);
              setCopied("");
              setName("");
              await reload();
            });
          }}
        >
          <Field
            id="key-name"
            label="Key name"
            hint="Something that tells you later what uses it, like “Box office laptop”."
            required
            maxLength={80}
            autoComplete="off"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <fieldset className="choices">
            <legend>Access</legend>
            {(["read", "write"] as const).map((s) => (
              <label className="check" key={s}>
                <input
                  type="radio"
                  name="scope"
                  checked={scope === s}
                  onChange={() => setScope(s)}
                />
                <span>
                  {ACCESS[s]}
                  <span className="hint">
                    {s === "read"
                      ? "It can look at your events and everything set up for them."
                      : "It can also create, change and delete them."}
                  </span>
                </span>
              </label>
            ))}
          </fieldset>
          <ErrorText>{create.error}</ErrorText>
          <button type="submit" className="inline" disabled={create.busy}>
            Create key
          </button>
        </form>
      </section>
    </>
  );
}

function KeyRow({ k, org, onRevoked }: { k: Key; org: Org; onRevoked: () => void }) {
  // null until asked; false after "Keep it", so focus can go back to the first button.
  const [confirming, setConfirming] = useState<boolean | null>(null);
  const revoke = useAction();
  return (
    <tr>
      <td>{k.name}</td>
      <td>
        <code>tsk_{k.prefix}…</code>
      </td>
      <td>{ACCESS[k.scope]}</td>
      <td>{k.last_used_at ? when(k.last_used_at) : "Never"}</td>
      <td className="actions-cell">
        {confirming ? (
          <>
            <p>Anything using this key stops working straight away.</p>
            <div className="row">
              <button
                type="button"
                className="inline"
                ref={focusOnMount}
                disabled={revoke.busy}
                onClick={() =>
                  revoke.run(async () => {
                    must(
                      await api.DELETE("/organizers/{organizer}/api-keys/{apiKey}", {
                        params: { path: { organizer: org.slug, apiKey: k.id } },
                      }),
                    );
                    onRevoked();
                  })
                }
              >
                Revoke {k.name}
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
            aria-label={`Revoke ${k.name}`}
            ref={confirming === false ? focusOnMount : undefined}
            onClick={() => setConfirming(true)}
          >
            Revoke
          </button>
        )}
        <ErrorText>{revoke.error}</ErrorText>
      </td>
    </tr>
  );
}
