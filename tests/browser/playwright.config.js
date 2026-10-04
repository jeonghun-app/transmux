const { defineConfig } = require("@playwright/test");
const fs = require("node:fs");
const path = require("node:path");

const filename = process.env.TRANSMUX_TEST_ENV || path.resolve(__dirname, "../../.env.solution");
const env = Object.fromEntries(fs.readFileSync(filename, "utf8").split("\n")
  .filter(line => line && !line.startsWith("#") && line.includes("="))
  .map(line => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)]));
for (const [key, value] of Object.entries(env)) process.env[key] ||= value;

module.exports = defineConfig({
  testDir: ".",
  testMatch: "**/*.spec.js",
  fullyParallel: false,
  workers: 1,
  timeout: 90000,
  expect: { timeout: 15000 },
  reporter: [["list"], ["html", { open: "never" }]],
  use: {
    baseURL: process.env.TRANSMUX_TEST_URL || env.TRANSMUX_PUBLIC_URL || "http://localhost:8090",
    viewport: { width: 1440, height: 1000 },
    locale: "ko-KR",
    timezoneId: "Asia/Seoul",
    reducedMotion: "reduce",
    screenshot: "only-on-failure",
    // Traces can contain login bodies and capability URLs.
    trace: "off",
  },
});
