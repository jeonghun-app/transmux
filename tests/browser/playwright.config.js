const { defineConfig } = require("@playwright/test");
const fs = require("node:fs");
const path = require("node:path");

const filename = process.env.TRANSMUX_TEST_ENV || path.resolve(__dirname, "../../.env.solution");
// Supported .env syntax, as in scripts/verify-solution.py: KEY=value, an
// optional leading "export", spaces around "=", a UTF-8 BOM, comments and
// simple '...' or "..." quoting. Escapes, variable references and multi-line
// values are not supported; generated values are URL-safe and unaffected.
function parseEnv(text) {
  const values = {};
  for (const line of text.replace(/^\uFEFF/, "").split(/\r?\n/)) {
    const match = /^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=\s*(.*?)\s*$/.exec(line);
    if (!match) continue;
    const quoted = /^(['"])(.*?)\1(?:\s+#.*)?$/.exec(match[2]);
    values[match[1]] = quoted ? quoted[2] : match[2].replace(/\s+#.*$/, "");
  }
  return values;
}
const env = parseEnv(fs.readFileSync(filename, "utf8"));
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
