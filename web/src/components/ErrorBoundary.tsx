import { Component, type ReactNode } from "react";
import { RefreshCw, TriangleAlert } from "lucide-react";

interface PageErrorBoundaryProps {
  /** 变化时（如切换页面）自动清除错误状态重新渲染。 */
  resetKey: string;
  children: ReactNode;
}

interface PageErrorBoundaryState {
  error: Error | null;
}

/** 捕获页面渲染异常，避免单页错误把整个应用卸载成白屏。 */
export class PageErrorBoundary extends Component<PageErrorBoundaryProps, PageErrorBoundaryState> {
  state: PageErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): PageErrorBoundaryState {
    return { error };
  }

  componentDidUpdate(previous: PageErrorBoundaryProps) {
    if (previous.resetKey !== this.props.resetKey && this.state.error) {
      this.setState({ error: null });
    }
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="error-boundary" role="alert">
        <TriangleAlert size={22} aria-hidden="true" />
        <strong>页面渲染出错</strong>
        <code>{this.state.error.message}</code>
        <button onClick={() => this.setState({ error: null })}>
          <RefreshCw size={14} aria-hidden="true" />重试
        </button>
      </div>
    );
  }
}
