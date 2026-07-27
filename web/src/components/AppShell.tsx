import { useState, type ReactNode } from "react";
import { Activity, Bot, Cloud, Database, Folder, Gauge, HardDrive, KeyRound, LogOut, Menu, Users, X } from "lucide-react";
import { motion } from "motion/react";
import { Dialog, Modal, ModalOverlay } from "react-aria-components";

export type PageID = "overview" | "accounts" | "storage" | "files" | "d1" | "ai" | "access" | "activity";

const navigation = [
  { id: "overview" as const, label: "概览", icon: Gauge },
  { id: "accounts" as const, label: "账号", icon: Users },
  { id: "storage" as const, label: "R2 存储", icon: HardDrive },
  { id: "files" as const, label: "文件管理", icon: Folder },
  { id: "d1" as const, label: "D1 数据库", icon: Database },
  { id: "ai" as const, label: "Workers AI", icon: Bot },
  { id: "access" as const, label: "访问密钥", icon: KeyRound },
  { id: "activity" as const, label: "任务与审计", icon: Activity },
];

export function AppShell({ page, onNavigate, onLogout, children }: {
  page: PageID;
  onNavigate: (page: PageID) => void;
  onLogout: () => void;
  children: ReactNode;
}) {
  const [drawerOpen, setDrawerOpen] = useState(false);

  function navigate(next: PageID) {
    onNavigate(next);
    setDrawerOpen(false);
  }

  const brand = <div className="brand"><span className="brand-mark"><Cloud size={17} /></span><strong>CF-R2Manager</strong></div>;

  return (
    <div className="app-shell">
      <aside className="sidebar">
        {brand}
        <nav aria-label="主导航">
          {navigation.map(({ id, label, icon: Icon }) => (
            <button key={id} className={page === id ? "active" : ""} onClick={() => navigate(id)} aria-current={page === id ? "page" : undefined}>
              {page === id && <motion.span className="nav-indicator" layoutId="nav-active" transition={{ type: "spring", stiffness: 420, damping: 34 }} />}
              <Icon size={17} /><span>{label}</span>
            </button>
          ))}
        </nav>
        <button className="logout" onClick={onLogout}><LogOut size={16} /><span>退出登录</span></button>
      </aside>
      <header className="mobile-topbar">
        {brand}
        <button className="icon-button" onClick={() => setDrawerOpen(true)} aria-label="打开导航"><Menu size={18} /></button>
      </header>
      <main className="workspace">{children}</main>
      <ModalOverlay className="mobile-drawer-overlay" isOpen={drawerOpen} onOpenChange={setDrawerOpen} isDismissable>
        <Modal className="mobile-drawer-modal">
          <Dialog className="mobile-drawer" aria-label="主导航">
            <header>{brand}<button className="icon-button" onClick={() => setDrawerOpen(false)} aria-label="关闭导航"><X size={18} /></button></header>
            <nav aria-label="移动端主导航">
              {navigation.map(({ id, label, icon: Icon }) => (
                <button key={id} className={page === id ? "active" : ""} onClick={() => navigate(id)} aria-current={page === id ? "page" : undefined}>
                  <Icon size={18} /><span>{label}</span>
                </button>
              ))}
            </nav>
            <button className="logout" onClick={onLogout}><LogOut size={18} /><span>退出登录</span></button>
          </Dialog>
        </Modal>
      </ModalOverlay>
    </div>
  );
}
