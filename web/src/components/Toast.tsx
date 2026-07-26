import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { AlertCircle, CheckCircle2, Info } from "lucide-react";
import { AnimatePresence, motion } from "motion/react";

type ToastTone = "success" | "info" | "error";
interface ToastItem { id: number; message: string; tone: ToastTone }
interface ToastAPI { show: (message: string, tone?: ToastTone) => void }

const ToastContext = createContext<ToastAPI | null>(null);
let nextToastID = 1;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const show = useCallback((message: string, tone: ToastTone = "success") => {
    const id = nextToastID++;
    setItems((current) => [...current, { id, message, tone }]);
    window.setTimeout(() => setItems((current) => current.filter((item) => item.id !== id)), tone === "error" ? 7000 : 3600);
  }, []);
  const api = useMemo(() => ({ show }), [show]);

  return (
    <ToastContext.Provider value={api}>
      {children}
      <div className="toast-region" aria-live="polite" aria-label="通知">
        <AnimatePresence>
          {items.map((item) => {
            const Icon = item.tone === "success" ? CheckCircle2 : item.tone === "error" ? AlertCircle : Info;
            return (
              <motion.div
                key={item.id}
                className={`toast ${item.tone}`}
                role={item.tone === "error" ? "alert" : "status"}
                layout
                initial={{ opacity: 0, x: 32, scale: 0.95 }}
                animate={{ opacity: 1, x: 0, scale: 1 }}
                exit={{ opacity: 0, x: 18, scale: 0.97 }}
                transition={{ type: "spring", stiffness: 380, damping: 28 }}
              >
                <Icon size={17} aria-hidden="true" /><span>{item.message}</span>
              </motion.div>
            );
          })}
        </AnimatePresence>
      </div>
    </ToastContext.Provider>
  );
}

export function useToast() {
  const value = useContext(ToastContext);
  if (!value) throw new Error("useToast must be used inside ToastProvider");
  return value;
}
