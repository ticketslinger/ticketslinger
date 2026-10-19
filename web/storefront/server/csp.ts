const base = [
  "default-src 'self'",
  "frame-ancestors 'none'",
  "base-uri 'self'",
  "object-src 'none'",
  "form-action 'self'",
];
const extra = {
  plain: [],
  // The buyer's order page shows its QR codes as data: PNGs, from the API.
  order: ["img-src 'self' data:"],
  turnstile: [
    "script-src 'self' https://challenges.cloudflare.com",
    "frame-src https://challenges.cloudflare.com",
    "connect-src 'self' https://challenges.cloudflare.com",
  ],
  // Stripe's own list for Stripe.js and Link (docs.stripe.com/security/guide, read 2026-10-14).
  // Link is on unless the organizer's own Stripe account turns it off, so it's always allowed.
  // No Google Maps: we don't use the Address Element.
  stripe: [
    "script-src 'self' https://js.stripe.com https://*.js.stripe.com",
    "frame-src https://js.stripe.com https://*.js.stripe.com https://hooks.stripe.com https://link.com https://*.link.com",
    "connect-src 'self' https://api.stripe.com https://link.com https://*.link.com",
    "img-src 'self' https://*.link.com",
  ],
} as const;

export const csp = (page: keyof typeof extra) => [...base, ...extra[page]].join("; ");
