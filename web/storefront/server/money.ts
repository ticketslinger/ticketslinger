/** Whole cents to dollars for display, with integer arithmetic only: 123456 is "$1,234.56". */
export function formatCents(cents: number): string {
  const c = cents % 100;
  const dollars = (cents - c) / 100;
  return `$${dollars.toLocaleString("en-US")}.${String(c).padStart(2, "0")}`;
}
