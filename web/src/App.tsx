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
import { FilesPage } from "./pages/FilesPage";
import { PageTransition } from "./components/Motion";
import { PageErrorBoundary } from "./components/ErrorBoundary";
import { ToastProvider } from "./components/Toast";

const D1Page = lazy(() => import("./pages/D1Page").then((module) => ({ default: module.D1Page })));

const pages: Record<Exclude<PageID, "files">, ReactNode> = {
  overview: <OverviewPage />,
  accounts: <AccountsPage />,
  storage: <StoragePage />,
  d1: <D1Page />,
  ai: <AIPage />,
  access: <AccessPage />,
  activity: <ActivityPage />,
};

interface RouteState { page: PageID; fileMountID: string; filePath: string }

function currentRoute(): RouteState {
  const value = window.location.hash.replace("#", "");
  const [pageValue, query = ""] = value.split("?", 2);
  const knownPages: PageID[] = ["overview", "accounts", "storage", "files", "d1", "ai", "access", "activity"];
  const page = knownPages.includes(pageValue as PageID) ? pageValue as PageID : "overview";
  const params = new URLSearchParams(query);
  return {
    page,
    fileMountID: page === "files" ? params.get("mount") ?? "" : "",
    filePath: page === "files" ? params.get("path") ?? "" : "",
  };
}

export default function App() {
  const [auth, setAuth] = useState<"checking" | "guest" | "authenticated">("checking");
  const [route, setRoute] = useState<RouteState>(currentRoute());

  useEffect(() => {
    api.session().then(() => setAuth("authenticated")).catch(() => setAuth("guest"));
    setUnauthorizedHandler(() => setAuth("guest"));
    const onHashChange = () => setRoute(currentRoute());
    window.addEventListener("hashchange", onHashChange);
    return () => {
      setUnauthorizedHandler(null);
      window.removeEventListener("hashchange", onHashChange);
    };
  }, []);

  useEffect(() => {
    window.scrollTo(0, 0);
  }, [route.page, route.fileMountID, route.filePath]);

  if (auth === "checking") {
    return <main className="centered"><LoaderCircle className="spin" aria-label="加载中" /></main>;
  }
  if (auth === "guest") {
    return <Login onAuthenticated={() => setAuth("authenticated")} />;
  }
  const content = route.page === "files" ? (
    <FilesPage
      mountID={route.fileMountID}
      path={route.filePath}
      onNavigate={(mountID, path) => {
        const query = new URLSearchParams();
        if (mountID) query.set("mount", mountID);
        if (path) query.set("path", path);
        const next = query.size ? `files?${query}` : "files";
        window.location.hash = next;
        setRoute({ page: "files", fileMountID: mountID, filePath: path });
      }}
    />
  ) : pages[route.page];

  return (
    <ToastProvider>
      <AppShell
        page={route.page}
        onNavigate={(next) => { window.location.hash = next; setRoute({ page: next, fileMountID: "", filePath: "" }); }}
        onLogout={async () => {
          try { await api.logout(); } catch { /* 会话可能已失效；本地登出不依赖服务端结果 */ }
          setAuth("guest");
        }}
      >
        <Suspense fallback={<div className="centered-page"><LoaderCircle className="spin" aria-label="加载中" /></div>}>
          <PageTransition pageKey={route.page}><PageErrorBoundary resetKey={`${route.page}:${route.fileMountID}:${route.filePath}`}>{content}</PageErrorBoundary></PageTransition>
        </Suspense>
      </AppShell>
    </ToastProvider>
  );
}
