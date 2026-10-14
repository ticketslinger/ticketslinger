/** @jsxImportSource preact */
import type { ComponentChildren } from "preact";

export function Layout({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <html lang="en">
      <head>
        <meta charset="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <title>{title}</title>
        <link rel="stylesheet" href="/assets/storefront.css" />
      </head>
      <body>
        <main>{children}</main>
      </body>
    </html>
  );
}
