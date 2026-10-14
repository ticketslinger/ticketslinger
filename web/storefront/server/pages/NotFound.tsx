/** @jsxImportSource preact */
import { Layout } from "./Layout";

export function NotFound({ what }: { what: "event" | "page" }) {
  return (
    <Layout title="Not found">
      <h1>We can't find that {what}.</h1>
      <p>Check the link, or ask whoever sent it for a fresh one.</p>
    </Layout>
  );
}

export function ErrorPage({ message }: { message: string }) {
  return (
    <Layout title="Something went wrong">
      <h1>{message}</h1>
    </Layout>
  );
}
