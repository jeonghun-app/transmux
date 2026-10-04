"use strict";

const $ = id => document.getElementById(id);
const state = {
  token: "", epoch: 0, abort: new AbortController(), me: null, cameras: [],
  selected: new Set(), players: new Map(), liveGeneration: 0, archivePlayer: null,
  catalogGeneration: 0, managedGeneration: 0, managed: [], etag: "",
  view: "live", exportsTimer: null, exportsGeneration: 0, recordingGeneration: 0,
  recordingQuery: null, disableCamera: null, dialogTrigger: null,
};
const keyOf = cam => `${cam.center_id}/${cam.camera_id}`;
const messages = {
  invalid_credentials: "아이디 또는 비밀번호를 확인하세요.",
  login_rate_limit: "로그인 요청이 많습니다. 1분 후 다시 시도하세요.",
  invalid_token: "로그인이 만료되었습니다. 다시 로그인하세요.",
  forbidden: "이 작업에 필요한 권한이 없습니다.",
  camera_offline: "최근 라이브 영상이 없습니다. 카메라 연결 상태를 확인한 뒤 다시 시도하세요.",
  camera_disabled: "영상 수집이 중지된 카메라입니다.",
  recording_not_found: "이 구간에 조회할 수 있는 녹화 영상이 없습니다.",
  range_too_large: "녹화 조각이 너무 많습니다. 조회 구간을 줄여주세요.",
  invalid_range: "시작과 종료 시각을 확인하세요. 조회는 최대 24시간, 재생은 최대 6시간입니다.",
  invalid_export_range: "다운로드할 구간이 너무 깁니다. 더 짧은 구간을 선택하세요.",
  recording_format_changed: "저장 형식이 변경된 구간입니다. 조회 결과에서 같은 형식의 구간을 선택하세요.",
  camera_list_changed: "다른 사용자가 카메라 목록을 수정했습니다. 목록을 새로고침한 뒤 다시 편집하세요.",
  camera_exists: "이미 등록된 카메라입니다. 기존 카메라를 편집하세요.",
  shard_capacity: "이 수집 서버의 카메라 한도에 도달했습니다.",
  export_capacity: "내보내기 작업 또는 보관 한도에 도달했습니다. 기존 작업을 정리한 뒤 다시 시도하세요.",
  export_storage_full: "저장 공간이 부족합니다. 다른 내보내기가 끝나거나 기존 파일을 정리한 뒤 다시 시도하세요.",
  export_too_large: "파일 크기 한도를 초과했습니다. 더 짧은 구간을 선택하세요.",
  export_failed: "파일을 만들지 못했습니다. 녹화가 있는 짧고 연속된 구간으로 다시 시도하세요.",
  export_timeout: "파일 준비 시간이 초과되었습니다. 더 짧은 구간을 선택하세요.",
  query_capacity: "조회 요청이 많습니다. 잠시 후 다시 시도하세요.",
  session_capacity: "녹화 재생 세션이 많습니다. 잠시 후 다시 시도하세요.",
  temporarily_unavailable: "서비스에 연결하지 못했습니다. 잠시 후 다시 시도하세요.",
  external_camera_provider: "카메라 정보는 연결된 외부 시스템에서 관리합니다.",
};

function text(node, value) { node.textContent = value; return node; }
function el(tag, className, content) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (content !== undefined) node.textContent = content;
  return node;
}
function status(id, message = "", kind = "") {
  const node = typeof id === "string" ? $(id) : id;
  node.textContent = message;
  node.classList.toggle("error", kind === "error");
  node.classList.toggle("success", kind === "success");
}
function report(id, error) {
  if (!error.stale) status(id, error.message || "요청에 실패했습니다. 다시 시도하세요.", "error");
}
function busy(button, label) {
  if (button.dataset.busy) return null;
  button.dataset.busy = "true";
  const original = button.textContent;
  button.style.setProperty("--busy-width", `${button.getBoundingClientRect().width}px`);
  button.setAttribute("aria-busy", "true");
  button.setAttribute("aria-disabled", "true");
  const mark = el("span", "busy-mark");
  mark.setAttribute("aria-hidden", "true");
  button.replaceChildren(mark, document.createTextNode(label));
  return () => {
    button.textContent = original;
    delete button.dataset.busy;
    button.removeAttribute("aria-busy");
    button.removeAttribute("aria-disabled");
    button.style.removeProperty("--busy-width");
  };
}
async function api(path, options = {}) {
  const epoch = state.epoch;
  const controller = new AbortController();
  const cancel = () => controller.abort();
  const parent = state.abort.signal;
  parent.addEventListener("abort", cancel, { once: true });
  const timer = setTimeout(cancel, 25000);
  const headers = { Accept: "application/json", ...options.headers };
  if (state.token) headers.Authorization = `Bearer ${state.token}`;
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  try {
    const response = await fetch(path, {
      ...options, headers, signal: controller.signal,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      credentials: "omit", cache: "no-store", referrerPolicy: "no-referrer",
    });
    const data = await response.json();
    if (epoch !== state.epoch) throw Object.assign(new Error(), { stale: true });
    if (!response.ok) {
      const code = data.error?.code;
      const error = Object.assign(new Error(messages[code] || data.error?.message || "요청에 실패했습니다."), { code, httpStatus: response.status });
      if (response.status === 401 && path !== "/v1/login") logout(messages.invalid_token);
      throw error;
    }
    return { data, etag: response.headers.get("ETag") };
  } catch (error) {
    if (epoch !== state.epoch) throw Object.assign(error, { stale: true });
    if (error.name === "AbortError" || error instanceof TypeError) {
      throw Object.assign(new Error("응답을 확인하지 못했습니다. 연결을 확인한 뒤 상태를 새로고침하세요."), { uncertain: true });
    }
    throw error;
  } finally {
    clearTimeout(timer);
    parent.removeEventListener("abort", cancel);
  }
}
function formatTime(value) {
  return new Intl.DateTimeFormat("ko-KR", { dateStyle: "short", timeStyle: "medium", hour12: false }).format(new Date(value));
}
function localInput(date) {
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000);
  return local.toISOString().slice(0, 19);
}
function readableSize(bytes) {
  if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(2)} GB`;
  return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
}

$("show-password").addEventListener("click", () => {
  const shown = $("password").type === "password";
  $("password").type = shown ? "text" : "password";
  $("show-password").setAttribute("aria-pressed", String(shown));
  $("show-password").textContent = shown ? "숨김" : "표시";
});
$("login-form").addEventListener("submit", async event => {
  event.preventDefault();
  const done = busy($("login-button"), "로그인 중");
  if (!done) return;
  status("login-status", "계정을 확인하고 있습니다.");
  try {
    const { data } = await api("/v1/login", { method: "POST", body: {
      username: $("username").value.trim(), password: $("password").value,
    } });
    state.token = data.access_token;
    state.me = (await api("/v1/me")).data;
    $("password").value = "";
    $("login-view").hidden = true;
    $("app").hidden = false;
    $("skip-link").hidden = false;
    $("account-name").textContent = state.me.username;
    $("manage-nav").hidden = !state.me.grants.some(grant => grant.permissions.includes("manage"));
    status("login-status");
    await refreshCatalog();
    showView(location.hash.slice(1) || "live");
    $("main").focus();
  } catch (error) {
    state.token = "";
    report("login-status", error);
  } finally { done(); }
});
function logout(message = "") {
  state.epoch++;
  state.abort.abort();
  state.abort = new AbortController();
  state.token = "";
  stopAll();
  state.archivePlayer?.dispose();
  state.archivePlayer = null;
  clearTimeout(state.exportsTimer);
  state.exportsGeneration++;
  state.recordingGeneration++;
  state.cameras = [];
  state.managed = [];
  state.me = null;
  state.etag = "";
  state.recordingQuery = null;
  state.disableCamera = null;
  state.dialogTrigger = null;
  $("camera-form").reset();
  $("camera-list").replaceChildren();
  $("managed-list").replaceChildren();
  $("recording-camera").replaceChildren();
  $("recording-periods").replaceChildren();
  $("export-list").replaceChildren();
  $("play-recording").disabled = true;
  $("export-recording").disabled = true;
  $("archive-title").textContent = "녹화 영상";
  $("archive-range").textContent = "조회할 카메라와 시간 구간을 선택하세요.";
  $("account-name").textContent = "";
  for (const id of ["recording-status", "archive-player-status", "export-status", "manage-status", "camera-save-status", "disable-status"]) status(id);
  $("camera-dialog").close();
  $("disable-dialog").close();
  $("app").hidden = true;
  $("skip-link").hidden = true;
  $("login-view").hidden = false;
  $("password").value = "";
  $("password").type = "password";
  $("show-password").setAttribute("aria-pressed", "false");
  $("show-password").textContent = "표시";
  status("login-status", message);
  $("username").focus();
}
$("logout").addEventListener("click", () => logout());

function showView(view) {
  if (!state.token) return;
  if (!["live", "recordings", "cameras"].includes(view) || (view === "cameras" && $("manage-nav").hidden)) view = "live";
  const previous = state.view;
  state.view = view;
  for (const name of ["live", "recordings", "cameras"]) $(`view-${name}`).hidden = name !== view;
  document.querySelectorAll("[data-view]").forEach(link => {
    if (link.dataset.view === view) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  });
  if (previous === "live" && view !== "live") stopAll(false);
  if (previous === "recordings" && view !== "recordings") {
    state.archivePlayer?.dispose();
    state.archivePlayer = null;
    clearTimeout(state.exportsTimer);
    state.exportsGeneration++;
  }
  if (view === "cameras") loadManaged();
  if (view === "recordings") refreshExports();
}
window.addEventListener("hashchange", () => showView(location.hash.slice(1)));

async function refreshCatalog() {
  const generation = ++state.catalogGeneration;
  status("catalog-status", "카메라 목록을 불러오는 중입니다.");
  $("camera-list").setAttribute("aria-busy", "true");
  try {
    const { data } = await api("/v1/cameras");
    if (generation !== state.catalogGeneration) return;
    state.cameras = data.cameras;
    const centers = [...new Set(state.cameras.map(camera => camera.center_id))];
    const selectedCenter = $("center-filter").value;
    $("center-filter").replaceChildren(new Option("전체 센터", ""), ...centers.map(center => new Option(center, center)));
    if (centers.includes(selectedCenter)) $("center-filter").value = selectedCenter;
    const validKeys = new Set(state.cameras.filter(cam => cam.enabled && cam.permissions.includes("live")).map(keyOf));
    for (const key of state.selected) if (!validKeys.has(key)) state.selected.delete(key);
    for (const [key, player] of state.players) {
      if (!validKeys.has(key)) { player.dispose(); state.players.delete(key); document.querySelector(`[data-camera-key="${CSS.escape(key)}"]`)?.remove(); }
    }
    renderCameraList();
    const archiveKey = $("recording-camera").value;
    const archiveCameras = state.cameras.filter(cam => cam.permissions.includes("recording"));
    $("recording-camera").replaceChildren(...archiveCameras.map(cam => new Option(`${cam.center_id} · ${cam.name}`, keyOf(cam))));
    if (archiveCameras.some(cam => keyOf(cam) === archiveKey)) $("recording-camera").value = archiveKey;
    status("catalog-status", state.cameras.length ? "" : "이 계정에 연결된 카메라가 없습니다.");
    updateLiveCount();
  } catch (error) { report("catalog-status", error); }
  finally { if (generation === state.catalogGeneration) $("camera-list").removeAttribute("aria-busy"); }
}
function renderCameraList() {
  const center = $("center-filter").value;
  const term = $("camera-search").value.toLocaleLowerCase();
  const cameras = state.cameras.filter(cam => (!center || cam.center_id === center) &&
    `${cam.name} ${cam.camera_id} ${cam.center_id}`.toLocaleLowerCase().includes(term));
  $("camera-count").textContent = cameras.length;
  const fragment = document.createDocumentFragment();
  for (const camera of cameras) {
    const label = el("label", "camera-option");
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.checked = state.selected.has(keyOf(camera));
    checkbox.disabled = !camera.enabled || !camera.permissions.includes("live");
    checkbox.setAttribute("aria-label", `${camera.center_id} ${camera.name} 선택`);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked && state.selected.size >= 16) {
        checkbox.checked = false;
        status("catalog-status", "한 번에 최대 16대까지 선택할 수 있습니다.", "error");
        return;
      }
      if (checkbox.checked) state.selected.add(keyOf(camera));
      else state.selected.delete(keyOf(camera));
      updateSelection();
    });
    const info = el("span");
    info.append(el("strong", "", camera.name), el("small", "", `${camera.center_id} / ${camera.camera_id}${!camera.enabled ? " · 수집 중지" : !camera.permissions.includes("live") ? " · 라이브 권한 없음" : ""}`));
    label.append(checkbox, info);
    fragment.append(label);
  }
  if (!cameras.length) fragment.append(el("p", "muted", "조건에 맞는 카메라가 없습니다."));
  $("camera-list").replaceChildren(fragment);
  updateSelection();
}
function updateSelection() {
  $("selection-count").textContent = `선택한 카메라 ${state.selected.size}대`;
  $("watch-selected").disabled = state.selected.size === 0;
}
$("center-filter").addEventListener("change", renderCameraList);
$("camera-search").addEventListener("input", renderCameraList);
$("refresh-catalog").addEventListener("click", async () => {
  const done = busy($("refresh-catalog"), "새로고침 중");
  if (done) { try { await refreshCatalog(); } finally { done(); } }
});
function emptyStage() {
  const empty = el("div", "empty-stage");
  empty.append(el("h2", "", "시청할 카메라를 선택하세요"), el("p", "", "카메라를 선택한 다음 ‘선택 영상 보기’를 누르세요."));
  return empty;
}
function updateLiveCount() { $("live-count").textContent = `열린 화면 ${state.players.size}개`; }
function stopAll(clearSelection = true) {
  state.liveGeneration++;
  for (const player of state.players.values()) player.dispose();
  state.players.clear();
  if (clearSelection) state.selected.clear();
  $("video-grid").replaceChildren(emptyStage());
  updateLiveCount();
  renderCameraList();
}
$("stop-all").addEventListener("click", () => stopAll());
$("layout").addEventListener("change", () => {
  const limit = Number($("layout").value);
  $("video-grid").className = `video-grid grid-${limit}`;
  let n = 0;
  for (const [key, player] of state.players) {
    if (++n > limit) {
      player.dispose();
      state.players.delete(key);
      state.selected.delete(key);
      document.querySelector(`[data-camera-key="${CSS.escape(key)}"]`)?.remove();
    }
  }
  renderCameraList();
  updateLiveCount();
});

class Player {
  constructor(video, feedback, mode, retryButton = null) {
    this.video = video;
    this.feedback = feedback;
    this.mode = mode;
    this.retryButton = retryButton;
    this.disposed = false;
    this.generation = 0;
    this.video.muted = true;
    this.playing = () => {
      clearTimeout(this.startTimer);
      status(this.feedback, this.mode === "live" ? "라이브 재생 중" : "녹화 재생 중", "success");
      if (this.retryButton) this.retryButton.hidden = true;
    };
    this.nativeError = () => this.error("영상을 재생하지 못했습니다. 연결 상태와 브라우저의 코덱 지원을 확인하세요.");
    video.addEventListener("playing", this.playing);
    video.addEventListener("error", this.nativeError);
  }
  error(message) {
    if (this.disposed) return;
    clearTimeout(this.startTimer);
    status(this.feedback, message, "error");
    if (this.retryButton) this.retryButton.hidden = false;
  }
  attach(stream, offset = 0) {
    if (this.disposed) return;
    const generation = ++this.generation;
    clearTimeout(this.timer);
    clearTimeout(this.startTimer);
    if (this.nativeReady) this.video.removeEventListener("loadedmetadata", this.nativeReady);
    this.hls?.destroy();
    this.hls = null;
    this.stream = stream;
    status(this.feedback, "영상에 연결하고 있습니다.");
    const ready = () => {
      if (this.disposed || generation !== this.generation) return;
      if (this.mode === "recording" && offset > 0) this.video.currentTime = offset;
      this.video.play().catch(() => status(this.feedback, "영상의 재생 버튼을 누르세요."));
    };
    if (window.Hls?.isSupported()) {
      this.hls = new Hls({ debug: false, maxBufferLength: 30, maxMaxBufferLength: 60, backBufferLength: 30, liveSyncDurationCount: 3 });
      this.hls.on(Hls.Events.MANIFEST_PARSED, ready);
      this.hls.on(Hls.Events.ERROR, (_, data) => {
        if (data.fatal && generation === this.generation) {
          this.error("연결이 끊겼거나 지원되지 않는 영상입니다. 다시 연결할 수 있습니다.");
          this.hls?.stopLoad();
        }
      });
      this.hls.loadSource(stream.url);
      this.hls.attachMedia(this.video);
    } else if (this.video.canPlayType("application/vnd.apple.mpegurl")) {
      this.nativeReady = ready;
      this.video.addEventListener("loadedmetadata", ready, { once: true });
      // A newly created tile uses preload=none. Waiting for metadata without
      // starting the native loader would leave playback permanently idle.
      this.video.preload = "auto";
      this.video.src = stream.url;
      this.video.load();
    } else {
      this.error("이 브라우저는 HLS 재생을 지원하지 않습니다.");
      return;
    }
    this.startTimer = setTimeout(() => {
      if (!this.disposed && generation === this.generation && this.video.readyState < 2) {
        this.error("영상 연결이 지연되고 있습니다. 잠시 후 다시 연결하세요.");
      }
    }, 30000);
    this.timer = setTimeout(async () => {
      if (this.disposed || generation !== this.generation) return;
      try {
        const token = new URL(stream.url).pathname.split("/")[2];
        const { data } = await api("/v1/playback-sessions/renew", { method: "POST", body: { session_token: token } });
        if (!this.disposed && generation === this.generation) this.attach(data, this.mode === "recording" ? this.video.currentTime : 0);
      } catch (error) { if (!error.stale) this.error("재생 세션을 갱신하지 못했습니다. 다시 연결하세요."); }
    }, Math.max(1000, new Date(stream.expires_at).getTime() - Date.now() - 60000));
  }
  dispose() {
    if (this.disposed) return;
    this.disposed = true;
    this.generation++;
    clearTimeout(this.timer);
    clearTimeout(this.startTimer);
    if (this.nativeReady) this.video.removeEventListener("loadedmetadata", this.nativeReady);
    this.hls?.destroy();
    this.video.pause();
    this.video.removeAttribute("src");
    this.video.load();
    this.video.removeEventListener("playing", this.playing);
    this.video.removeEventListener("error", this.nativeError);
  }
}

function makeTile(camera, generation) {
  const key = keyOf(camera);
  const tile = el("article", "video-tile");
  tile.dataset.cameraKey = key;
  const heading = el("div", "tile-heading");
  const title = el("h2");
  title.append(el("small", "", camera.center_id), document.createTextNode(camera.name));
  const close = el("button", "plain", "닫기");
  close.type = "button";
  close.setAttribute("aria-label", `${camera.name} 영상 닫기`);
  heading.append(title, close);
  const video = document.createElement("video");
  video.controls = true; video.playsInline = true; video.preload = "none";
  video.setAttribute("aria-label", `${camera.name} 라이브 영상`);
  const footer = el("div", "tile-footer");
  const feedback = el("p", "status", "연결 대기 중");
  feedback.setAttribute("role", "status");
  const retry = el("button", "plain", "다시 연결");
  retry.type = "button"; retry.hidden = true;
  footer.append(feedback, retry);
  tile.append(heading, video, footer);
  const player = new Player(video, feedback, "live", retry);
  state.players.set(key, player);
  close.addEventListener("click", () => {
    player.dispose(); state.players.delete(key); state.selected.delete(key); tile.remove();
    if (!state.players.size) $("video-grid").replaceChildren(emptyStage());
    renderCameraList(); updateLiveCount();
  });
  const connect = async () => {
    if (player.disposed) return;
    const done = busy(retry, "연결 중");
    if (!done) return;
    status(feedback, "영상에 연결하고 있습니다.");
    try {
      const { data } = await api("/v1/playback-sessions", { method: "POST", body: {
        center_id: camera.center_id, camera_ids: [camera.camera_id], mode: "live",
      } });
      if (generation === state.liveGeneration && !player.disposed) player.attach(data.streams[0]);
    } catch (error) { if (!error.stale) player.error(error.message); }
    finally { done(); }
  };
  retry.addEventListener("click", connect);
  return { tile, connect };
}
$("watch-selected").addEventListener("click", async () => {
  const done = busy($("watch-selected"), "연결 중");
  if (!done) return;
  const chosen = state.cameras.filter(cam => state.selected.has(keyOf(cam)) && cam.enabled && cam.permissions.includes("live"));
  stopAll(false);
  const generation = state.liveGeneration;
  const size = [1, 4, 9, 16].find(size => size >= chosen.length) || 16;
  if (Number($("layout").value) < size) $("layout").value = String(size);
  $("video-grid").className = `video-grid grid-${$("layout").value}`;
  $("video-grid").replaceChildren();
  const waiting = chosen.map(cam => makeTile(cam, generation));
  waiting.forEach(item => $("video-grid").append(item.tile));
  updateLiveCount();
  status("live-status", `${waiting.length}대의 카메라에 연결하고 있습니다.`);
  try {
    let next = 0;
    await Promise.all(Array.from({ length: Math.min(4, waiting.length) }, async () => {
      while (next < waiting.length && generation === state.liveGeneration) await waiting[next++].connect();
    }));
    if (generation === state.liveGeneration) status("live-status");
  } finally { done(); }
});

const now = new Date();
$("recording-start").value = localInput(new Date(now.getTime() - 10 * 60000));
$("recording-end").value = localInput(now);
$("timezone").textContent = `표시 시간대 · ${Intl.DateTimeFormat().resolvedOptions().timeZone}`;
function recordingRequest() {
  const key = $("recording-camera").value;
  const camera = state.cameras.find(cam => keyOf(cam) === key);
  const start = new Date($("recording-start").value);
  const end = new Date($("recording-end").value);
  if (!camera || !Number.isFinite(+start) || !Number.isFinite(+end) || +start >= +end || +end - +start > 86400000) {
    throw new Error("카메라와 시작·종료 시각을 확인하세요. 한 번에 최대 24시간까지 조회할 수 있습니다.");
  }
  return { center_id: camera.center_id, camera_id: camera.camera_id, start: start.toISOString(), end: end.toISOString() };
}
function invalidateRecording() {
  state.recordingGeneration++;
  state.recordingQuery = null;
  $("play-recording").disabled = true;
  $("export-recording").disabled = true;
  $("recording-periods").replaceChildren();
  status("recording-status", "조건이 바뀌었습니다. 녹화 구간을 다시 조회하세요.");
}
for (const id of ["recording-camera", "recording-start", "recording-end"]) $(id).addEventListener("change", invalidateRecording);
$("recording-form").addEventListener("submit", async event => {
  event.preventDefault();
  const done = busy($("recording-search"), "조회 중");
  if (!done) return;
  const generation = ++state.recordingGeneration;
  status("recording-status", "녹화가 있는 구간을 확인하고 있습니다.");
  $("recording-periods").setAttribute("aria-busy", "true");
  try {
    const req = recordingRequest();
    const { data } = await api(`/v1/recordings?${new URLSearchParams(req)}`);
    if (generation !== state.recordingGeneration) return;
    state.recordingQuery = req;
    const camera = state.cameras.find(cam => keyOf(cam) === `${req.center_id}/${req.camera_id}`);
    const hasVideo = data.periods.length > 0;
    $("play-recording").disabled = !hasVideo;
    $("export-recording").disabled = !hasVideo || !camera.permissions.includes("export");
    const fragment = document.createDocumentFragment();
    for (const period of data.periods) {
      const item = el("div", "period");
      item.append(el("p", "", `${formatTime(period.start)} — ${formatTime(period.end)}`));
      const button = el("button", "secondary", `구간 선택 · ${readableSize(period.bytes)}`);
      button.type = "button";
      button.addEventListener("click", () => {
        state.recordingGeneration++;
        $("recording-start").value = localInput(new Date(period.start));
        $("recording-end").value = localInput(new Date(period.end));
        state.recordingQuery = { ...req, start: period.start, end: period.end };
        status("recording-status", "구간을 선택했습니다. 재생 또는 내보내기를 누르세요.");
      });
      item.append(button); fragment.append(item);
    }
    $("recording-periods").replaceChildren(fragment);
    status("recording-status", hasVideo ? `${data.periods.length}개 녹화 구간을 찾았습니다.${!data.index_complete ? " 과거 목록을 복구 중이므로 결과가 추가될 수 있습니다." : ""}` :
      `조회된 녹화 영상이 없습니다.${!data.index_complete ? " 과거 목록을 복구 중입니다. 잠시 후 다시 조회하세요." : " 다른 시간을 선택하세요."}`);
  } catch (error) { report("recording-status", error); }
  finally {
    done();
    $("recording-periods").removeAttribute("aria-busy");
  }
});
$("play-recording").addEventListener("click", async () => {
  const done = busy($("play-recording"), "불러오는 중");
  if (!done || !state.recordingQuery) { done?.(); return; }
  const req = { ...state.recordingQuery };
  const generation = state.recordingGeneration;
  status("archive-player-status", "선택한 녹화 영상을 불러오고 있습니다.");
  try {
    const { data } = await api("/v1/playback-sessions", { method: "POST", body: {
      center_id: req.center_id, camera_ids: [req.camera_id], mode: "recording", start: req.start, end: req.end,
    } });
    if (generation !== state.recordingGeneration || state.view !== "recordings") return;
    state.archivePlayer?.dispose();
    state.archivePlayer = new Player($("archive-video"), $("archive-player-status"), "recording");
    const stream = data.streams[0];
    state.archivePlayer.attach(stream);
    const camera = state.cameras.find(cam => keyOf(cam) === `${req.center_id}/${req.camera_id}`);
    $("archive-title").textContent = camera?.name || req.camera_id;
    $("archive-range").textContent = `${formatTime(stream.actual_start)} — ${formatTime(stream.actual_end)}`;
  } catch (error) { report("archive-player-status", error); }
  finally { done(); }
});
$("export-recording").addEventListener("click", async () => {
  const done = busy($("export-recording"), "요청 중");
  if (!done || !state.recordingQuery) { done?.(); return; }
  try {
    await api("/v1/exports", { method: "POST", body: state.recordingQuery });
    status("export-status", "내보내기 작업이 시작되었습니다. 파일이 준비되면 다운로드할 수 있습니다.");
    await refreshExports();
  } catch (error) { report("export-status", error); }
  finally { done(); }
});
async function refreshExports() {
  if (!state.token || state.view !== "recordings") return;
  clearTimeout(state.exportsTimer);
  const generation = ++state.exportsGeneration;
  try {
    const { data } = await api("/v1/exports");
    const details = await Promise.all(data.exports.map(async job => {
      try { return { ...job, ...(await api(`/v1/exports/${job.id}`)).data }; }
      catch (error) { if (error.stale) throw error; return job; }
    }));
    if (generation !== state.exportsGeneration) return;
    const fragment = document.createDocumentFragment();
    for (const job of details) {
      const item = el("div", "export-item");
      item.dataset.exportId = job.id;
      const label = el("div", "export-label");
      label.append(el("strong", "", `${job.center_id} / ${job.camera_id}`));
      const phase = job.state === "processing" ? "MP4 파일 준비 중" : job.state === "ready" ? `${readableSize(job.bytes)} · 1시간 보관` :
        job.state === "cancelled" ? "취소됨" : messages[job.error] || "내보내기에 실패했습니다.";
      label.append(el("span", "", phase));
      item.append(label);
      if (job.url) {
        const link = el("a", "", "MP4 다운로드");
        link.href = job.url;
        link.dataset.action = "download";
        link.referrerPolicy = "no-referrer";
        item.append(link);
      }
      const remove = el("button", "plain", job.state === "processing" ? "작업 취소" : "목록에서 제거");
      remove.type = "button";
      remove.dataset.action = "remove";
      remove.addEventListener("click", async () => {
        const done = busy(remove, "처리 중");
        if (!done) return;
        try {
          await api(`/v1/exports/${job.id}`, { method: "DELETE" });
          status("export-status", job.state === "processing" ? "작업을 취소했습니다." : "보관된 파일을 제거했습니다.");
          await refreshExports();
        } catch (error) { report("export-status", error); }
        finally { done(); }
      });
      item.append(remove); fragment.append(item);
    }
    if (!details.length) fragment.append(el("p", "muted", "준비된 MP4 파일은 여기서 다운로드할 수 있습니다."));
    const focused = document.activeElement;
    const focusID = focused.closest?.("[data-export-id]")?.dataset.exportId;
    const action = focused.dataset?.action;
    $("export-list").replaceChildren(fragment);
    if (focusID && action && document.activeElement === document.body) {
      $("export-list").querySelector(`[data-export-id="${CSS.escape(focusID)}"] [data-action="${action}"]`)?.focus();
    }
    state.exportsTimer = setTimeout(refreshExports, details.some(job => job.state === "processing") ? 2000 : 60000);
  } catch (error) {
    report("export-status", error);
    if (state.token && generation === state.exportsGeneration) state.exportsTimer = setTimeout(refreshExports, 10000);
  }
}
$("refresh-exports").addEventListener("click", refreshExports);

async function loadManaged() {
  const generation = ++state.managedGeneration;
  status("manage-status", "카메라 설정을 불러오는 중입니다.");
  try {
    const { data, etag } = await api("/v1/admin/cameras");
    if (generation !== state.managedGeneration) return;
    state.managed = data.cameras;
    state.etag = etag;
    $("edit-shard").replaceChildren(...data.shards.map(shard => new Option(`${shard.id} · 최대 ${shard.max_channels}대`, shard.id)));
    $("add-camera").disabled = !data.editable;
    const fragment = document.createDocumentFragment();
    for (const camera of data.cameras) {
      const row = el("tr");
      const name = el("td", "", camera.name);
      name.append(el("small", "", camera.camera_id));
      const statusCell = el("td");
      statusCell.append(el("span", `state-badge${camera.enabled ? "" : " off"}`, camera.enabled ? "수집 사용" : "수집 중지"));
      const actions = el("td");
      const actionGroup = el("div", "table-actions");
      const edit = el("button", "secondary", "편집");
      edit.type = "button"; edit.disabled = !data.editable;
      edit.setAttribute("aria-label", `${camera.name} 편집`);
      edit.addEventListener("click", () => openCamera(camera, edit));
      actionGroup.append(edit);
      if (camera.enabled && data.editable) {
        const disable = el("button", "danger", "수집 중지");
        disable.type = "button";
        disable.setAttribute("aria-label", `${camera.name} 수집 중지`);
        disable.addEventListener("click", () => {
          state.disableCamera = { ...camera, etag: state.etag };
          state.dialogTrigger = disable;
          $("disable-description").textContent = `${camera.name}의 라이브 영상과 새 녹화 수집을 중지합니다.`;
          status("disable-status");
          $("confirm-disable").disabled = false;
          $("disable-dialog").showModal();
          $("cancel-disable").focus();
        });
        actionGroup.append(disable);
      }
      actions.append(actionGroup);
      row.append(name, el("td", "", camera.center_id), statusCell,
        el("td", "", `${camera.format === "fmp4" ? "fMP4" : "MPEG-TS"} / ${camera.audio === "none" ? "음성 없음" : "AAC"}`),
        el("td", "", camera.last_recorded_at ? formatTime(camera.last_recorded_at) : "최근 녹화 없음"), actions);
      fragment.append(row);
    }
    $("managed-list").replaceChildren(fragment);
    status("manage-status", data.editable ? `${data.cameras.length}대 등록 · 수집 서버당 최대 ${data.max_active_cameras}대 활성화` : messages.external_camera_provider);
  } catch (error) { report("manage-status", error); }
}
$("refresh-managed").addEventListener("click", loadManaged);
$("add-camera").addEventListener("click", () => openCamera(null, $("add-camera")));
function openCamera(camera, trigger) {
  state.dialogTrigger = trigger;
  $("camera-form").reset();
  $("camera-dialog").dataset.editing = camera ? "true" : "";
  $("camera-dialog").dataset.etag = state.etag;
  $("camera-dialog-title").textContent = camera ? "카메라 편집" : "카메라 추가";
  $("edit-center").value = camera?.center_id || "";
  $("edit-id").value = camera?.camera_id || "";
  $("edit-center").readOnly = Boolean(camera);
  $("edit-id").readOnly = Boolean(camera);
  $("edit-name").value = camera?.name || "";
  if (camera?.shard_id) $("edit-shard").value = camera.shard_id;
  $("edit-url").value = "";
  $("edit-url").required = !camera;
  $("url-help").textContent = camera ? "주소를 바꾸려면 새 RTSP 주소를 입력하세요. 비워두면 기존 연결 정보를 유지합니다." :
    "카메라 제조사에서 제공한 rtsp:// 또는 rtsps:// 주소를 입력하세요.";
  $("edit-format").value = camera?.format || "mpegts";
  $("edit-codec").value = camera?.video_codec || "h264";
  syncCodecFormat();
  $("edit-audio").value = camera?.audio || "none";
  $("edit-enabled").checked = camera ? camera.enabled : true;
  $("save-camera").disabled = false;
  status("camera-save-status");
  $("camera-dialog").showModal();
  $(camera ? "edit-name" : "edit-center").focus();
}
function syncCodecFormat() {
  const hevc = $("edit-codec").value === "hevc";
  if (hevc) $("edit-format").value = "fmp4";
  $("edit-format").options[0].disabled = hevc;
}
$("edit-codec").addEventListener("change", syncCodecFormat);
function closeDialog(dialog) {
  dialog.close();
  if (state.dialogTrigger?.isConnected) state.dialogTrigger.focus();
}
for (const id of ["close-camera-dialog", "cancel-camera-dialog"]) $(id).addEventListener("click", () => closeDialog($("camera-dialog")));
$("cancel-disable").addEventListener("click", () => closeDialog($("disable-dialog")));
for (const id of ["camera-dialog", "disable-dialog"]) $(id).addEventListener("close", () => {
  if (state.dialogTrigger?.isConnected) state.dialogTrigger.focus();
});
$("camera-form").addEventListener("submit", async event => {
  event.preventDefault();
  const done = busy($("save-camera"), "저장 중");
  if (!done || $("save-camera").disabled) { done?.(); return; }
  const editing = Boolean($("camera-dialog").dataset.editing);
  const body = {
    center_id: $("edit-center").value.trim(), camera_id: $("edit-id").value.trim(),
    name: $("edit-name").value.trim(), rtsp_url: $("edit-url").value.trim(),
    shard_id: $("edit-shard").value,
    video_codec: $("edit-codec").value,
    audio: $("edit-audio").value, format: $("edit-format").value, enabled: $("edit-enabled").checked,
  };
  status("camera-save-status", "카메라 설정을 저장하고 있습니다.");
  try {
    const url = editing ? `/v1/admin/cameras/${encodeURIComponent(body.center_id)}/${encodeURIComponent(body.camera_id)}` : "/v1/admin/cameras";
    const { data } = await api(url, { method: editing ? "PUT" : "POST", body, headers: { "If-Match": $("camera-dialog").dataset.etag } });
    closeDialog($("camera-dialog"));
    await Promise.all([loadManaged(), refreshCatalog()]);
    status("manage-status", `카메라 설정을 저장했습니다. 수집 서버가 ${data.apply_within_seconds}초 이내에 변경을 확인합니다.`, "success");
  } catch (error) {
    report("camera-save-status", error);
    if (error.uncertain || error.httpStatus === 412 || error.httpStatus >= 500) {
      $("save-camera").disabled = true;
      status("camera-save-status", `${error.message} 창을 닫고 목록을 새로고침해 저장 결과를 확인하세요.`, "error");
    }
  } finally { done(); }
});
$("confirm-disable").addEventListener("click", async () => {
  const done = busy($("confirm-disable"), "중지 요청 중");
  if (!done || $("confirm-disable").disabled) { done?.(); return; }
  const camera = state.disableCamera;
  try {
    await api(`/v1/admin/cameras/${camera.center_id}/${camera.camera_id}`, {
      method: "DELETE", headers: { "If-Match": camera.etag },
    });
    closeDialog($("disable-dialog"));
    await Promise.all([loadManaged(), refreshCatalog()]);
    status("manage-status", `${camera.name}의 수집 중지를 저장했습니다. 기존 녹화는 계속 조회할 수 있습니다.`, "success");
  } catch (error) {
    report("disable-status", error);
    if (error.uncertain || error.httpStatus === 412 || error.httpStatus >= 500) $("confirm-disable").disabled = true;
  } finally { done(); }
});
