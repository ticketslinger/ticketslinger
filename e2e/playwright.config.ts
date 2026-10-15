import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: ".",
  forbidOnly: !!process.env.CI,
  // pay-latest pays whichever checkout is newest, so two specs buying at once would pay each other's.
  workers: 1,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: { baseURL: "http://localhost:3002", ...devices["Pixel 7"], trace: "retain-on-failure" },
  webServer: {
    command: "bun start.ts",
    url: "http://localhost:3002/",
    reuseExistingServer: !process.env.CI,
    // Builds the API and the dashboard before serving.
    timeout: 180_000,
    stdout: "pipe",
  },
});
