import { useState } from "react";
import { api, must } from "../api";
import { ErrorText, Field, Link, type Me, navigate, useAction } from "../ui";

/** Name → slug matching the API's rule: ^[a-z0-9](-?[a-z0-9])*$, at most `max` characters. */
export function slugify(name: string, max = 40): string {
  return name
    .normalize("NFKD")
    .replace(/[̀-ͯ]/g, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+/, "")
    .slice(0, max)
    .replace(/-+$/, "");
}

export function NewOrganizer({ me }: { me: Me }) {
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [slugEdited, setSlugEdited] = useState(false);
  const { busy, error, run } = useAction();

  return (
    <main>
      <p>
        <Link href="/">Back to your dashboard</Link>
      </p>
      <h1 tabIndex={-1}>Create your organizer</h1>
      <p>An organizer is whoever puts on the events: you, your venue, your promotion company.</p>
      {!me.email_verified && (
        <p className="notice">
          Confirm your email first. The link is in your inbox, and this page will work once you've
          clicked it.
        </p>
      )}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          run(async () => {
            must(await api.POST("/organizers", { body: { name, slug } }));
            navigate("/");
          });
        }}
      >
        <Field
          id="org-name"
          label="Organizer name"
          autoComplete="organization"
          required
          maxLength={120}
          value={name}
          onChange={(e) => {
            setName(e.target.value);
            if (!slugEdited) setSlug(slugify(e.target.value));
          }}
        />
        <Field
          id="slug"
          label="Short link name"
          hint="Lowercase letters, numbers and single hyphens, 3 to 40 characters. We fill it in from the name; change it if you like."
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          required
          minLength={3}
          maxLength={40}
          pattern="[a-z0-9](-?[a-z0-9])*"
          value={slug}
          onChange={(e) => {
            setSlug(e.target.value);
            setSlugEdited(e.target.value !== "");
          }}
        />
        <ErrorText>{error}</ErrorText>
        <button type="submit" disabled={busy}>
          Create organizer
        </button>
      </form>
    </main>
  );
}
