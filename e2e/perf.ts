// Page weight and Lighthouse scores for the storefront's event page (spec §3, D7).
import { gzipSync } from "node:zlib";
import { chromium, type Page, type Response } from "@playwright/test";
import lighthouse from "lighthouse";

/**
 * Gzip bytes of every script `url` loads. `topFrame` counts only scripts the top-level
 * document loads, first or third party; `all` adds the ones inside iframes (Turnstile's challenge).
 */
export async function scriptBytes(
  page: Page,
  url: string,
): Promise<{ topFrame: number; all: number }> {
  const scripts: Response[] = [];
  let last = Date.now();
  const collect = (r: Response) => {
    if (r.request().resourceType() !== "script") return;
    scripts.push(r);
    last = Date.now();
  };
  page.on("response", collect);
  // Not "networkidle": Turnstile's challenge iframe runs a blob: script whose request never
  // finishes, so the page never goes idle. Wait for load, then two quiet seconds without a script.
  await page.goto(url, { waitUntil: "load" });
  while (Date.now() - last < 2_000) await page.waitForTimeout(250);
  page.off("response", collect);
  let topFrame = 0;
  let all = 0;
  for (const r of scripts) {
    // A redirect has no body; the script it points at arrives as its own response.
    if (r.status() >= 300 && r.status() < 400) continue;
    const bytes = gzipSync(await r.body()).length;
    all += bytes;
    if (r.request().frame() === page.mainFrame()) topFrame += bytes;
  }
  return { topFrame, all };
}

const median = (xs: number[]) => [...xs].sort((a, b) => a - b)[Math.floor(xs.length / 2)];

/** Lighthouse `runs` times on its mobile defaults (simulated throttling); the median of each score. */
export async function lighthouseScores(
  url: string,
  runs: number,
): Promise<{ performance: number; accessibility: number; bestPractices: number }> {
  const browser = await chromium.launch({
    channel: "chromium",
    args: ["--remote-debugging-port=9223"],
  });
  const scores = {
    performance: [] as number[],
    accessibility: [] as number[],
    bestPractices: [] as number[],
  };
  try {
    for (let i = 0; i < runs; i++) {
      const result = await lighthouse(url, {
        port: 9223,
        output: "json",
        logLevel: "error",
        onlyCategories: ["performance", "accessibility", "best-practices"],
      });
      if (!result) throw new Error("lighthouse returned no result");
      const { categories, runtimeError } = result.lhr;
      if (runtimeError) throw new Error(`lighthouse: ${runtimeError.code} ${runtimeError.message}`);
      // Whatever costs points, by audit id, so a red run says why.
      for (const c of Object.values(categories)) {
        for (const ref of c.auditRefs) {
          const a = result.lhr.audits[ref.id];
          if (ref.weight > 0 && a.score !== null && a.score < 1) {
            const items = (a.details as { items?: { node?: { snippet?: string } }[] } | undefined)
              ?.items;
            const at = items?.[0]?.node?.snippet ?? "";
            console.log(`lighthouse ${c.id}: ${ref.id} ${a.score} ${a.displayValue ?? ""} ${at}`);
          }
        }
      }
      scores.performance.push(categories.performance.score ?? 0);
      scores.accessibility.push(categories.accessibility.score ?? 0);
      scores.bestPractices.push(categories["best-practices"].score ?? 0);
    }
  } finally {
    await browser.close();
  }
  return {
    performance: median(scores.performance),
    accessibility: median(scores.accessibility),
    bestPractices: median(scores.bestPractices),
  };
}
