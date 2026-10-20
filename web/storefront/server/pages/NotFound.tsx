/** @jsxImportSource preact */
import { UNAVAILABLE } from "../../islands/checkout";
import { Layout } from "./Layout";

export function NotFound({ what }: { what: "event" | "page" | "checkout" }) {
  return (
    <Layout title="Not found">
      <h1>We can't find that {what}.</h1>
      <p>Check the link, or ask whoever sent it for a fresh one.</p>
    </Layout>
  );
}

// A suspended organizer's event: the same page for every reason, so it says nothing about why.
export function Unavailable() {
  return (
    <Layout title="Not available">
      <h1>{UNAVAILABLE}</h1>
      <p>You can't buy tickets for it here. Already have an order? Your order link still works.</p>
    </Layout>
  );
}

export function ErrorPage({
  message,
  title = "Something went wrong",
}: {
  message: string;
  title?: string;
}) {
  return (
    <Layout title={title}>
      <h1>{message}</h1>
    </Layout>
  );
}
