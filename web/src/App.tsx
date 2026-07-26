import { lazy, Suspense, useEffect, useState, type ReactNode } from "react";
import { LoaderCircle } from "lucide-react";
import { api, setUnauthorizedHandler } from "./api";
import { AppShell, type PageID } from "./components/AppShell";
import { Login } from "./components/Login";
import { OverviewPage } from "./pages/OverviewPage";
import { AccountsPage } from "./pages/AccountsPage";
import { StoragePage } from "./pages/StoragePage";
import { AIPage } from "./pages/AIPage";
import { AccessPage } from "./pages/AccessPage";
import { ActivityPage } from "./pages/ActivityPage";
import { PageTransition } from "./components/Motion";
import { ToastProvider } from "./components/Toast";

const D1Page = lazy(() => import("./pages/D1Page").then((module) => ({ default: module.D1Page })));

const pages: Record<PageID, ReactNode> = {
  overview: <OverviewPage />,
  accounts: <AccountsPage />,
  storage: <StoragePage />,
  d1: <D1Page />,
  ai: <AIPage />,
  access: <AccessPage />,
  activity: <ActivityPage />,
};

function currentPage(): PageID {
  const value = window.location.hash.replace("#", "") as PageID;
  return Object.prototype.hasOwnProperty.call(pages, value) ? value : "overview";
}

export default function App() {
  const [auth, setAuth] = useState<"checking" | "guest" | "authenticated">("checking");
  const [page, setPage] = useState<PageID>(currentPage());

  useEffect(() => {
    api.session().then(() => setAuth("authenticated")).catch(() => setAuth("guest"));
    setUnauthorizedHandler(() => setAuth("guest"));
    const onHashChange = () => setPage(currentPage());
    window.addEventListener("hashchange", onHashChange);
    return () => {
      setUnauthorizedHandler(null);
      window.removeEventListener("hashchange", onHashChange);
    };
  }, []);

  useEffect(() => {
    window.scrollTo(0, 0);
  }, [page]);

  if (auth === "checking") {
    return <main className="centered"><LoaderCircle className="spin" aria-label="加载中" /></main>;
  }
  if (auth === "guest") {
    return <Login onAuthenticated={() => setAuth("authenticated")} />;
  }
  return (
    <ToastProvider>
      <AppShell
        page={page}
        onNavigate={(next) => { window.location.hash = next; setPage(next); }}
        onLogout={async () => {
          try { await api.logout(); } catch { /* 会话可能已失效；本地登出不依赖服务端结果 */ }
          setAuth("guest");
        }}
      >
        <Suspense fallback={<div className="centered-page"><LoaderCircle className="spin" aria-label="加载中" /></div>}>
          <PageTransition pageKey={page}>{pages[page]}</PageTransition>
        </Suspense>
      </AppShell>
    </ToastProvider>
  );
}
