"use strict";

const base = "/about/fardrop/";
const byId = (id) => document.getElementById(id);
let token = "";
let uploadController;
let session = 0;

function make(tag, className, content) {
  const item = document.createElement(tag);
  if (className) item.className = className;
  if (content) item.textContent = content;
  return item;
}

function button(text, className) {
  const item = make("button", className, text);
  item.type = "button";
  return item;
}

function message(target, text, isError = false) {
  target.textContent = text;
  target.dataset.error = String(isError);
}

function formatSize(size) {
  for (const [unit, label] of [[2 ** 30, "GiB"], [2 ** 20, "MiB"], [2 ** 10, "KiB"]]) {
    if (size >= unit) return (size / unit).toFixed(2) + " " + label;
  }
  return size + " B";
}

function dateLabel(epoch) {
  return new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(new Date(epoch * 1000));
}

async function api(path, { method = "GET", body, hash, signal } = {}) {
  const headers = new Headers({ Authorization: "Bearer " + token });
  if (hash) headers.set("X-Chunk-SHA256", hash);
  if (body && !(body instanceof Blob)) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(body);
  }
  const response = await fetch(base + "api/" + path, { method, headers, body, signal, cache: "no-store" });
  if (response.status === 204) return;
  const json = await response.json().catch(() => null);
  if (!response.ok) {
    const error = new Error(json?.error || "请求失败（HTTP " + response.status + "）。");
    error.status = response.status;
    throw error;
  }
  if (json === null) throw new Error("服务器未返回有效数据。");
  return json;
}

async function sha(blob) {
  const sum = await crypto.subtle.digest("SHA-256", await blob.arrayBuffer());
  return Array.from(new Uint8Array(sum), (item) => item.toString(16).padStart(2, "0")).join("");
}

async function sendPart(path, part, hash, signal) {
  for (let attempt = 0; ; attempt++) {
    signal.throwIfAborted();
    try {
      await api(path, { method: "PUT", body: part, hash, signal });
      return;
    } catch (error) {
      const retryable = error instanceof TypeError || error.status === 429 || error.status >= 500;
      if (signal.aborted || !retryable || attempt >= 2) throw error;
      await new Promise((resolve) => setTimeout(resolve, 600 * 2 ** attempt));
    }
  }
}

async function uploadFile(file) {
  if (uploadController || !token || !file) return;
  const status = byId("upload-message");
  const liveSession = session;
  byId("progress-area").hidden = false;
  byId("progress").value = 0;
  if (file.size === 0 || file.size > 5 * 2 ** 30) {
    message(status, "请选择大于 0 字节且不超过 5 GiB 的文件。", true);
    return;
  }

  const controller = new AbortController();
  uploadController = controller;
  const { signal } = controller;
  byId("file-name").textContent = file.name;
  byId("pause").hidden = false;
  message(status, "正在检查已上传分块…");

  try {
    const sample = 2 ** 16;
    const fingerprint = await sha(new Blob([
      String(file.lastModified), ":", file.slice(0, sample), file.slice(Math.max(0, file.size - sample))
    ]));
    signal.throwIfAborted();
    const upload = await api("uploads", {
      method: "POST", body: { name: file.name, size: file.size, fingerprint }, signal
    });
    const partCount = Math.ceil(file.size / upload.chunkSize);
    for (let index = 0; index < partCount; index++) {
      signal.throwIfAborted();
      const end = Math.min((index + 1) * upload.chunkSize, file.size);
      const part = file.slice(index * upload.chunkSize, end);
      const hash = await sha(part);
      signal.throwIfAborted();
      const previous = upload.parts[index];
      if (previous && previous !== hash) {
        throw new Error("所选文件与已上传分块不一致，请重新选择原文件。");
      }
      if (!previous) {
        message(status, "正在上传分块 " + (index + 1) + "/" + partCount + "…");
        await sendPart("uploads/" + upload.id + "/parts/" + index, part, hash, signal);
      }
      byId("progress").value = 100 * end / file.size;
      message(status, "已确认 " + formatSize(end) + " / " + formatSize(file.size));
    }
    message(status, "正在校验并保存完整文件…");
    const complete = () => api("uploads/" + upload.id + "/complete", { method: "POST", signal });
    let saved = await complete();
    while (saved.pending) {
      await new Promise((resolve) => setTimeout(resolve, 1500));
      signal.throwIfAborted();
      saved = await complete();
    }
    if (session !== liveSession) return;
    message(status, "上传完成，可以生成分享链接。");
    await loadFiles(saved.id);
  } catch (error) {
    if (session !== liveSession) return;
    if (signal.aborted) {
      message(status, "上传已暂停，重新选择同一文件即可继续。");
    } else {
      message(status, error.message + " 重新选择同一文件可重试。", true);
    }
  } finally {
    if (uploadController === controller) uploadController = undefined;
    if (session === liveSession) byId("pause").hidden = true;
  }
}

function fileRow(file) {
  const column = make("article", "file-item");
  const top = make("div", "file-top");
  const description = make("div");
  description.append(make("div", "file-name", file.name), make("p", "hint", formatSize(file.size)));
  if (file.deleteAfter) {
    description.append(make("p", "hint", "自动删除 · " + dateLabel(file.deleteAfter)));
  }
  const actions = make("div", "file-actions");
  const toggle = button("分享链接", "secondary");
  toggle.setAttribute("aria-expanded", "false");
  const remove = button("删除", "secondary danger");
  remove.setAttribute("aria-label", "删除 " + file.name);
  remove.addEventListener("click", async () => {
    if (!window.confirm("永久删除「" + file.name + "」？\n该文件的所有分享链接将失效。")) return;
    const liveSession = session;
    const status = byId("files-message");
    remove.disabled = true;
    message(status, "正在删除文件…");
    try {
      await api("files/" + file.id, { method: "DELETE" });
      if (session === liveSession) await loadFiles(undefined, "文件已删除。");
    } catch (error) {
      if (session === liveSession) message(status, error.message, true);
    } finally {
      remove.disabled = false;
    }
  });
  actions.append(toggle, remove);
  const content = make("div", "share-tools");
  content.hidden = true;
  top.append(description, actions);
  column.append(top, content);
  toggle.addEventListener("click", () => {
    content.hidden = !content.hidden;
    toggle.setAttribute("aria-expanded", String(!content.hidden));
    if (!content.hidden && !content.hasChildNodes()) shareTools(file, content);
  });
  return { column, toggle };
}

function shareTools(file, content) {
  const form = make("form");
  const expiry = make("input");
  expiry.type = "number";
  expiry.id = "hours-" + file.id;
  expiry.min = 1;
  const maxHours = file.deleteAfter ? Math.max(1, Math.ceil((file.deleteAfter - Date.now() / 1000) / 3600)) : 720;
  expiry.max = Math.min(720, maxHours);
  expiry.value = Math.min(24, maxHours);
  expiry.required = true;
  const label = make("label", "hint", "有效期（小时）");
  label.htmlFor = expiry.id;
  const submit = make("button", "primary", "生成链接");
  submit.type = "submit";
  form.append(label, expiry, submit);

  const result = make("div", "field-line");
  result.hidden = true;
  const link = make("input");
  link.type = "text";
  link.readOnly = true;
  link.setAttribute("aria-label", "下载分享链接");
  const copy = button("复制", "secondary");
  result.append(link, copy);
  const notice = make("p", "message");
  notice.setAttribute("role", "status");
  const list = make("div");
  content.append(form, result, notice, list);

  async function renderLinks() {
    const shares = await api("files/" + file.id + "/shares");
    list.replaceChildren();
    if (shares.length === 0) list.append(make("p", "hint", "还没有生成分享链接。"));
    for (const share of shares) {
      const row = make("div", "share-item");
      const expired = share.expiresAt <= Date.now() / 1000;
      const status = share.revokedAt ? "已撤销" : expired ? "已过期" : "有效至";
      row.append(make("span", "", status + " · " + dateLabel(share.expiresAt)));
      if (!share.revokedAt && !expired) {
        const revoke = button("撤销", "plain");
        revoke.addEventListener("click", async () => {
          revoke.disabled = true;
          try {
            await api("shares/" + share.id, { method: "DELETE" });
            if (link.dataset.shareId === share.id) {
              result.hidden = true;
              link.value = "";
            }
            await renderLinks();
          } catch (error) {
            message(notice, error.message, true);
            revoke.disabled = false;
          }
        });
        row.append(revoke);
      }
      list.append(row);
    }
  }

  copy.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(link.value);
      message(notice, "分享链接已复制。");
    } catch {
      link.focus();
      link.select();
      message(notice, "请选择并手动复制链接。");
    }
  });

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    submit.disabled = true;
    message(notice, "");
    try {
      const share = await api("files/" + file.id + "/shares", { method: "POST", body: { hours: expiry.valueAsNumber } });
      link.value = new URL(base + "s/" + share.token, location.origin).href;
      link.dataset.shareId = share.id;
      result.hidden = false;
      message(notice, "链接有效至 " + dateLabel(share.expiresAt) + "。请复制链接。");
      await renderLinks();
    } catch (error) {
      message(notice, error.message, true);
    } finally {
      submit.disabled = false;
    }
  });

  renderLinks().catch((error) => message(notice, error.message, true));
}

function renderFiles(files, openId) {
  const list = byId("files");
  list.replaceChildren();
  if (files.length === 0) list.append(make("p", "hint", "上传完成的文件会显示在这里。"));
  for (const file of files) {
    const { column, toggle } = fileRow(file);
    list.append(column);
    if (file.id === openId) toggle.click();
  }
}

async function loadFiles(openId, notice = "") {
  const liveSession = session;
  try {
    const files = await api("files");
    if (session !== liveSession) return;
    renderFiles(files, openId);
    message(byId("files-message"), notice);
  } catch (error) {
    if (session === liveSession) message(byId("files-message"), error.message, true);
  }
}

byId("login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const submit = event.currentTarget.querySelector("button");
  const input = byId("token");
  const status = byId("login-message");
  if (!/^[0-9a-f]{64}$/.test(input.value.trim())) {
    message(status, "管理令牌应为 64 位十六进制字符。", true);
    return;
  }
  token = input.value.trim();
  input.value = "";
  submit.disabled = true;
  message(status, "正在验证…");
  try {
    const files = await api("files");
    session++;
    renderFiles(files);
    byId("login").hidden = true;
    byId("dashboard").hidden = false;
    byId("logout").focus();
  } catch (error) {
    token = "";
    message(status, error.message, true);
  } finally {
    submit.disabled = false;
  }
});

byId("logout").addEventListener("click", () => {
  session++;
  uploadController?.abort();
  uploadController = undefined;
  token = "";
  byId("files").replaceChildren();
  byId("file-name").textContent = "点击选择或拖入文件";
  byId("progress-area").hidden = true;
  byId("pause").hidden = true;
  byId("dashboard").hidden = true;
  byId("login").hidden = false;
  message(byId("login-message"), "");
  message(byId("files-message"), "");
  byId("token").focus();
});

byId("pause").addEventListener("click", () => uploadController?.abort());
byId("file").addEventListener("change", (event) => {
  const file = event.target.files[0];
  event.target.value = "";
  void uploadFile(file);
});
const drop = byId("drop");
drop.addEventListener("dragover", (event) => { event.preventDefault(); drop.dataset.active = "true"; });
drop.addEventListener("dragleave", () => { drop.dataset.active = "false"; });
drop.addEventListener("drop", (event) => {
  event.preventDefault();
  drop.dataset.active = "false";
  void uploadFile(event.dataTransfer.files[0]);
});
