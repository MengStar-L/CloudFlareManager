import { FormEvent, useState } from "react";
import { Cloud, LoaderCircle, LogIn } from "lucide-react";
import { motion, useReducedMotion } from "motion/react";
import { api } from "../api";

export function Login({ onAuthenticated }: { onAuthenticated: () => void }) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const reduced = useReducedMotion();

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.login(password);
      onAuthenticated();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "登录失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="login-page">
      <motion.section
        className="login-card"
        aria-labelledby="login-title"
        initial={{ opacity: 0, y: reduced ? 0 : 16, scale: reduced ? 1 : 0.985 }}
        animate={{ opacity: 1, y: 0, scale: 1 }}
        transition={{ duration: 0.5, ease: [0.22, 1, 0.36, 1] }}
      >
        <div className="brand-mark"><Cloud size={22} /></div>
        <h1 id="login-title">CF-R2Manager</h1>
        <p>登录后管理你的 R2 存储、D1 数据库、Workers AI 与访问密钥。</p>
        <form onSubmit={submit}>
          <label htmlFor="password">管理员密码</label>
          <input id="password" type="password" autoComplete="current-password" value={password} onChange={(event) => setPassword(event.target.value)} required autoFocus />
          {error && <p className="form-error" role="alert">{error}</p>}
          <button className="primary" type="submit" disabled={busy}>
            {busy ? <LoaderCircle className="spin" size={16} /> : <LogIn size={16} />}
            登录
          </button>
        </form>
      </motion.section>
    </main>
  );
}
