export class APIError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

let csrfToken = "";
let onUnauthorized: (() => void) | null = null;

/** 会话失效（非登录接口返回 401）时通知应用层回到登录页。 */
export function setUnauthorizedHandler(handler: (() => void) | null) {
  onUnauthorized = handler;
}

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
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
    try {
      const payload = await response.json();
      message = payload.error?.message ?? message;
    } catch {
      // The status code remains the useful error signal for non-JSON protocol errors.
    }
    if (response.status === 401 && path !== "/api/v1/session") {
      onUnauthorized?.();
    }
    throw new APIError(response.status, message);
  }
  const text = await response.text();
  return (text ? JSON.parse(text) : undefined) as T;
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
  post: <T>(path: string, body: unknown) => request<T>(path, { method: "POST", body: JSON.stringify(body) }),
  delete: <T>(path: string, body?: unknown) => request<T>(path, {
    method: "DELETE",
    body: body === undefined ? undefined : JSON.stringify(body),
  }),
};
