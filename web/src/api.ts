export class APIError extends Error {
  status: number;
  code: string;

  constructor(status: number, message: string, code = "") {
    super(message);
    this.status = status;
    this.code = code;
  }
}

let csrfToken = "";
let onUnauthorized: (() => void) | null = null;

/** 会话失效（非登录接口返回 401）时通知应用层回到登录页。 */
export function setUnauthorizedHandler(handler: (() => void) | null) {
  onUnauthorized = handler;
}

async function requestResponse(path: string, options: RequestInit = {}): Promise<Response> {
  const method = (options.method ?? "GET").toUpperCase();
  const headers = new Headers(options.headers);
  if (options.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  if (!["GET", "HEAD", "OPTIONS"].includes(method) && csrfToken) {
    headers.set("X-CSRF-Token", csrfToken);
  }
  const response = await fetch(path, { ...options, headers, credentials: "same-origin" });
  if (!response.ok) {
    let message = `请求失败 (${response.status})`;
    let code = "";
    try {
      const payload = await response.json();
      message = payload.error?.message ?? message;
      code = payload.error?.code ?? code;
    } catch {
      // The status code remains the useful error signal for non-JSON protocol errors.
    }
    if (response.status === 401 && path !== "/api/v1/session") {
      onUnauthorized?.();
    }
    throw new APIError(response.status, message, code);
  }
  return response;
}

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await requestResponse(path, options);
  const text = await response.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

function upload<T>(path: string, file: File, onProgress: (progress: number) => void, signal?: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", path);
    xhr.withCredentials = true;
    xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");
    if (csrfToken) xhr.setRequestHeader("X-CSRF-Token", csrfToken);
    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable && event.total > 0) onProgress(event.loaded / event.total);
    };
    xhr.onerror = () => reject(new APIError(0, "网络连接中断"));
    xhr.onabort = () => reject(new DOMException("上传已取消", "AbortError"));
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        try { resolve((xhr.responseText ? JSON.parse(xhr.responseText) : undefined) as T); }
        catch { reject(new APIError(xhr.status, "服务器返回了无效响应")); }
        return;
      }
      let message = `请求失败 (${xhr.status})`;
      let code = "";
      try {
        const payload = JSON.parse(xhr.responseText);
        message = payload.error?.message ?? message;
        code = payload.error?.code ?? code;
      } catch { /* retain status message */ }
      if (xhr.status === 401) onUnauthorized?.();
      reject(new APIError(xhr.status, message, code));
    };
    if (signal) {
      if (signal.aborted) { xhr.abort(); return; }
      signal.addEventListener("abort", () => xhr.abort(), { once: true });
    }
    xhr.send(file);
  });
}

export const api = {
  async session() {
    const session = await request<{ csrf_token: string }>("/api/v1/session");
    csrfToken = session.csrf_token;
    return session;
  },
  async login(password: string) {
    const session = await request<{ csrf_token: string }>("/api/v1/session", {
      method: "POST",
      body: JSON.stringify({ password }),
    });
    csrfToken = session.csrf_token;
    return session;
  },
  async logout() {
    try {
      await request<void>("/api/v1/session", { method: "DELETE" });
    } finally {
      csrfToken = "";
    }
  },
  get: <T>(path: string) => request<T>(path),
  text: async (path: string) => (await requestResponse(path)).text(),
  upload,
  post: <T>(path: string, body: unknown) => request<T>(path, { method: "POST", body: JSON.stringify(body) }),
  delete: <T>(path: string, body?: unknown) => request<T>(path, {
    method: "DELETE",
    body: body === undefined ? undefined : JSON.stringify(body),
  }),
};
