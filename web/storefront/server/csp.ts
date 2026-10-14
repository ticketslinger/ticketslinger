const base = [
  "default-src 'self'",
  "frame-ancestors 'none'",
  "base-uri 'self'",
  "object-src 'none'",
  "form-action 'self'",
];
const extra = {
  plain: [],
  turnstile: [
    "script-src 'self' https://challenges.cloudflare.com",
    "frame-src https://challenges.cloudflare.com",
    "connect-src 'self' https://challenges.cloudflare.com",
  ],
  stripe: [
    "script-src 'self' https://js.stripe.com",
    "frame-src https://js.stripe.com https://hooks.stripe.com",
    "connect-src 'self' https://api.stripe.com",
  ],
} as const;

export const csp = (page: keyof typeof extra) => [...base, ...extra[page]].join("; ");
