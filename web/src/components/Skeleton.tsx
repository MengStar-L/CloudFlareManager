import type { CSSProperties } from "react";

export function Skeleton({ width = "100%", height = 16, className = "" }: { width?: number | string; height?: number | string; className?: string }) {
  const style: CSSProperties = { width, height };
  return <span className={`skeleton ${className}`.trim()} style={style} aria-hidden="true" />;
}

export function TableSkeleton({ columns = 4, rows = 4 }: { columns?: number; rows?: number }) {
  return (
    <div className="table-wrap" aria-busy="true" aria-label="正在加载">
      <table><tbody>
        {Array.from({ length: rows }, (_, row) => <tr key={row}>
          {Array.from({ length: columns }, (_, column) => <td key={column}><Skeleton width={column === 0 ? "72%" : "58%"} height={12} /></td>)}
        </tr>)}
      </tbody></table>
    </div>
  );
}
