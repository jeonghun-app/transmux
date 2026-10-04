const { test, expect } = require("@playwright/test");
const AxeBuilder = require("@axe-core/playwright").default;
const fs = require("node:fs/promises");
const path = require("node:path");
let cameraToClean = null;

test.afterEach(async ({ page }) => {
  if (!cameraToClean) return;
  const id = cameraToClean;
  cameraToClean = null;
  await page.evaluate(async cameraID => {
    if (!state.token) return;
    const { data, etag } = await api("/v1/admin/cameras");
    const camera = data.cameras.find(item => item.center_id === "demo-center" && item.camera_id === cameraID);
    if (camera?.enabled) await api(`/v1/admin/cameras/demo-center/${cameraID}`, {
      method: "DELETE", headers: { "If-Match": etag },
    });
  }, id);
});

async function login(page, username = "admin") {
  await page.goto("/");
  await page.getByLabel("아이디", { exact: true }).fill(username);
  await page.getByLabel("비밀번호", { exact: true }).fill(
    process.env[username === "admin" ? "TRANSMUX_ADMIN_PASSWORD" : "TRANSMUX_VIEWER_PASSWORD"]
  );
  await page.getByRole("button", { name: "로그인", exact: true }).click();
  await expect(page.locator("#app")).toBeVisible();
  await expect(page.locator("#camera-list .camera-option").first()).toBeVisible();
}

async function decoded(video) {
  await expect.poll(() => video.evaluate(element =>
    element.videoWidth > 0 && element.readyState >= 2 &&
    (element.getVideoPlaybackQuality?.().totalVideoFrames || 0) > 2
  ), { timeout: 60000, intervals: [500, 1000] }).toBe(true);
}

async function accessible(page) {
  const result = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
  expect(result.violations.map(({ id, nodes }) => ({ id, targets: nodes.map(node => node.target) }))).toEqual([]);
}

test("login preserves errors, prevents duplicate submission, and supports keyboard", async ({ page }) => {
  await page.goto("/");
  await accessible(page);
  await page.getByLabel("아이디", { exact: true }).fill("admin");
  await page.getByLabel("비밀번호", { exact: true }).fill("incorrect-password");
  await page.getByRole("button", { name: "로그인", exact: true }).click();
  await expect(page.locator("#login-status")).toContainText("아이디 또는 비밀번호");
  await expect(page.getByLabel("아이디", { exact: true })).toHaveValue("admin");
  let count = 0;
  await page.route("**/v1/login", async route => {
    count++;
    await new Promise(resolve => setTimeout(resolve, 650));
    await route.continue();
  });
  await page.getByLabel("비밀번호", { exact: true }).fill(process.env.TRANSMUX_ADMIN_PASSWORD);
  const button = page.locator("#login-button");
  const before = await button.boundingBox();
  await button.click({ force: true });
  await expect(button).toHaveAttribute("aria-busy", "true");
  await page.locator("#password").press("Enter");
  await button.click({ force: true });
  const pending = await button.boundingBox();
  expect(pending.width).toBe(before.width);
  await expect(page.locator("#app")).toBeVisible();
  expect(count).toBe(1);
  await expect(page.locator("#password")).toHaveValue("");
});

test("two real H.264 cameras decode MPEG-TS and fMP4 in the live grid", async ({ page }) => {
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await login(page);
  await page.getByRole("checkbox", { name: /정문.*선택/ }).check();
  await page.getByRole("checkbox", { name: /창고.*선택/ }).check();
  await page.getByRole("button", { name: "선택 영상 보기", exact: true }).click();
  const videos = page.locator("#video-grid video");
  await expect(videos).toHaveCount(2);
  await Promise.all([decoded(videos.nth(0)), decoded(videos.nth(1))]);
  await expect(page.locator("#video-grid .status.success")).toHaveCount(2);
  await accessible(page);
  await fs.mkdir(path.join(__dirname, "artifacts"), { recursive: true });
  await page.screenshot({ path: path.join(__dirname, "artifacts/live-console.png"), fullPage: true });
  await page.getByRole("button", { name: "전체 닫기", exact: true }).click();
  await expect(videos).toHaveCount(0);
  await expect(page.locator("#watch-selected")).toBeDisabled();
  expect(errors).toEqual([]);
});

test("native HLS fallback starts loading a newly opened video", async ({ page }) => {
  await login(page);
  const supported = await page.evaluate(() =>
    Boolean(document.createElement("video").canPlayType("application/vnd.apple.mpegurl")));
  test.skip(!supported, "This browser has no native HLS decoder");
  await page.evaluate(() => { window.Hls = undefined; });
  await page.getByRole("checkbox", { name: /정문.*선택/ }).check();
  await page.locator("#watch-selected").click();
  await decoded(page.locator("#video-grid video"));
  await expect(page.locator("#video-grid .status.success")).toHaveCount(1);
});

test("recording playback and completed MP4 download work without a Blob", async ({ page }) => {
  await login(page);
  await page.getByRole("link", { name: "녹화 조회", exact: true }).click();
  await page.locator("#recording-camera").selectOption("demo-center/entrance");
  await page.locator("#recording-search").click();
  await expect(page.locator("#recording-periods .period").first()).toBeVisible();
  await page.locator("#play-recording").click();
  await decoded(page.locator("#archive-video"));
  await expect(page.locator("#archive-range")).not.toContainText("조회할");
  await page.locator("#export-recording").click();
  await expect(page.locator("#export-status")).toContainText("작업이 시작");
  const item = page.locator(".export-item").filter({ hasText: "demo-center / entrance" }).first();
  const link = item.getByRole("link", { name: "MP4 다운로드" });
  await expect(link).toBeVisible({ timeout: 45000 });
  expect(await link.getAttribute("href")).not.toMatch(/^blob:/);
  const pendingDownload = page.waitForEvent("download");
  await link.click();
  const download = await pendingDownload;
  expect(download.suggestedFilename()).toMatch(/\.mp4$/);
  const saved = await download.path();
  expect((await fs.stat(saved)).size).toBeGreaterThan(10000);
  await accessible(page);
  await page.screenshot({ path: path.join(__dirname, "artifacts/recording-console.png"), fullPage: true });
});

test("camera create starts real ingestion and disable preserves the catalog entry", async ({ page }) => {
  await login(page);
  await page.getByRole("link", { name: "카메라 관리", exact: true }).click();
  await expect(page.locator("#managed-list tr").first()).toBeVisible();
  const cameraID = `browser-${Date.now()}`;
  cameraToClean = cameraID;
  const name = `브라우저 검증 ${cameraID}`;
  await page.locator("#add-camera").click();
  await expect(page.getByRole("dialog", { name: "카메라 추가", exact: true })).toBeVisible();
  await page.locator("#edit-center").fill("demo-center");
  await page.locator("#edit-id").fill(cameraID);
  await page.locator("#edit-name").fill(name);
  await page.locator("#edit-url").fill("rtsp://mediamtx:8554/entrance");
  await page.locator("#edit-codec").selectOption("hevc");
  await expect(page.locator("#edit-format")).toHaveValue("fmp4");
  await expect(page.locator('#edit-format option[value="mpegts"]')).toBeDisabled();
  await page.locator("#edit-codec").selectOption("h264");
  await page.locator("#edit-format").selectOption("mpegts");
  let writes = 0;
  await page.route("**/v1/admin/cameras", async route => {
    if (route.request().method() === "POST") {
      writes++;
      await new Promise(resolve => setTimeout(resolve, 600));
    }
    await route.continue();
  });
  await page.locator("#save-camera").click();
  await page.locator("#edit-name").press("Enter");
  await expect(page.locator("#camera-dialog")).not.toBeVisible();
  expect(writes).toBe(1);
  const row = page.locator("#managed-list tr").filter({ hasText: name });
  await expect(row).toBeVisible();
  await row.getByRole("button", { name: `${name} 편집`, exact: true }).click();
  await expect(page.locator("#edit-url")).toHaveValue("");
  await page.keyboard.press("Escape");
  await expect(row.getByRole("button", { name: `${name} 편집`, exact: true })).toBeFocused();
  await page.getByRole("link", { name: "라이브 관제", exact: true }).click();
  await page.getByRole("checkbox", { name: `demo-center ${name} 선택`, exact: true }).check();
  await page.locator("#watch-selected").click();
  // A new source needs a roster poll and its first complete GOP. Exercise
  // the operator's retry action if playback is requested before publication.
  await expect.poll(async () => {
    const video = page.locator("#video-grid video");
    const retry = page.getByRole("button", { name: "다시 연결", exact: true });
    if (await retry.isVisible() && !(await retry.getAttribute("aria-busy"))) await retry.click();
    return video.evaluate(element => element.videoWidth > 0 && element.readyState >= 2);
  }, { timeout: 60000, intervals: [1000, 2000, 3000] }).toBe(true);
  await decoded(page.locator("#video-grid video"));
  await page.getByRole("link", { name: "카메라 관리", exact: true }).click();
  await expect(row).toBeVisible();
  await row.getByRole("button", { name: `${name} 수집 중지`, exact: true }).click();
  await expect(page.locator("#cancel-disable")).toBeFocused();
  await page.locator("#confirm-disable").click();
  await expect(page.locator("#disable-dialog")).not.toBeVisible();
  await expect(row.locator(".state-badge")).toHaveText("수집 중지");
  await expect(row.getByRole("button", { name: `${name} 편집`, exact: true })).toBeVisible();
});

test("viewer permissions and narrow layouts retain controls and readable feedback", async ({ page }) => {
  await login(page);
  await page.getByRole("link", { name: "녹화 조회", exact: true }).click();
  await page.locator("#recording-camera").selectOption("demo-center/hevc");
  await page.locator("#recording-search").click();
  await expect(page.locator("#recording-periods .period").first()).toBeVisible();
  await page.getByRole("link", { name: "카메라 관리", exact: true }).click();
  await page.locator("#managed-list").getByRole("button", { name: / 편집$/ }).first().click();
  await page.locator("#edit-url").fill("rtsp://operator:private-test-password@camera.invalid/stream");
  await page.locator("#cancel-camera-dialog").click();
  await page.locator("#logout").click();
  await expect(page.locator("#recording-periods .period")).toHaveCount(0);
  await expect(page.locator("#export-list")).toBeEmpty();
  await expect(page.locator("#edit-url")).toHaveValue("");
  await page.setViewportSize({ width: 390, height: 844 });
  await login(page, "viewer");
  await expect(page.locator("#manage-nav")).not.toBeVisible();
  await expect(page.locator("#camera-list .camera-option")).toHaveCount(2);
  await accessible(page);
  for (const width of [320, 390, 768]) {
    await page.setViewportSize({ width, height: 900 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await expect(page.locator("#logout")).toBeVisible();
  }
  await page.getByRole("link", { name: "녹화 조회", exact: true }).click();
  await page.locator("#recording-search").click();
  await expect(page.locator("#recording-periods .period").first()).toBeVisible();
  await expect(page.locator("#export-recording")).toBeDisabled();
  await accessible(page);
});

test("refresh failures preserve the catalog and a stale recording response is ignored", async ({ page }) => {
  await login(page);
  const count = await page.locator("#camera-list .camera-option").count();
  await page.route("**/v1/cameras", route => route.fulfill({
    status: 503, contentType: "application/json",
    body: JSON.stringify({ error: { code: "temporarily_unavailable", message: "Unavailable" } }),
  }));
  await page.locator("#refresh-catalog").click();
  await expect(page.locator("#catalog-status")).toContainText("서비스에 연결하지 못했습니다");
  await expect(page.locator("#camera-list .camera-option")).toHaveCount(count);
  await page.getByRole("link", { name: "녹화 조회", exact: true }).click();
  await page.route("**/v1/recordings?**", async route => {
    await new Promise(resolve => setTimeout(resolve, 650));
    await route.continue();
  });
  await page.locator("#recording-camera").selectOption("demo-center/entrance");
  await page.locator("#recording-search").click();
  await page.locator("#recording-camera").selectOption("demo-center/warehouse");
  await expect(page.locator("#recording-search")).not.toHaveAttribute("aria-busy", "true");
  await expect(page.locator("#play-recording")).toBeDisabled();
  await expect(page.locator("#recording-periods .period")).toHaveCount(0);
});
