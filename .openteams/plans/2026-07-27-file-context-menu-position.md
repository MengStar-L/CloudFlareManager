# File Context Menu Position Implementation Plan

**Goal:** Make the file and directory context menu open at the pointer position while keeping the complete menu inside the browser viewport.

**Architecture:** Render `FileContextMenu` through a React portal attached to `document.body`, so its fixed coordinates use the same viewport coordinate system as `MouseEvent.clientX/clientY`. Measure the rendered menu before paint and clamp its position with an 8-pixel viewport margin; keep all existing menu callers and actions unchanged.

**Tech Stack:** React 19, TypeScript, React DOM portals, CSS fixed positioning, Vite production build, in-app browser verification.

---

### Task 1: Capture The Positioning Regression

**Files:**
- Inspect: `web/src/pages/FilesPage.tsx:107-109`
- Inspect: `web/src/pages/FilesPage.tsx:314-319`
- Inspect: `web/src/components/FileManagerDialogs.tsx:57-87`
- Inspect: `web/src/components/Motion.tsx:4-17`

- **Step 1: Open a populated file directory in the local test instance**

Use two entries, one directory and one file, so both menu heights can be exercised. Open the file management route at a desktop viewport of `1280x720`.

- **Step 2: Right-click a row and record the requested and rendered coordinates**

Use the browser to dispatch a real right-click near the center of a row. Record the click's `clientX/clientY` and read the context menu's `getBoundingClientRect()`.

Expected before the fix: the menu rectangle is displaced by the `PageTransition` containing block rather than starting at the recorded viewport coordinates.

- **Step 3: Confirm the containing-block cause**

Read the computed `transform` and `filter` of the ancestor Motion element and confirm that `FileContextMenu` is nested below it while using `position: fixed`.

Expected: at least one computed property establishes the fixed-position containing block, matching the observed offset.

### Task 2: Portal The Menu Into The Viewport Coordinate Space

**Files:**
- Modify: `web/src/components/FileManagerDialogs.tsx:1`
- Modify: `web/src/components/FileManagerDialogs.tsx:57-87`

- **Step 1: Add the React APIs required for a portal and pre-paint measurement**

Change the imports to include `useLayoutEffect` and `createPortal`:

```tsx
import { useEffect, useLayoutEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import { createPortal } from "react-dom";
```

- **Step 2: Add a focused viewport clamping helper**

Place this helper beside the menu component:

```tsx
const contextMenuMargin = 8;

function contextMenuPosition(x: number, y: number, width: number, height: number) {
  return {
    left: Math.max(contextMenuMargin, Math.min(x, window.innerWidth - width - contextMenuMargin)),
    top: Math.max(contextMenuMargin, Math.min(y, window.innerHeight - height - contextMenuMargin)),
  };
}
```

This keeps the pointer coordinate unchanged unless the rendered menu would cross a viewport edge.

- **Step 3: Measure the real menu dimensions before paint**

Inside `FileContextMenu`, replace the fixed width and height assumptions with state and a layout effect:

```tsx
const [position, setPosition] = useState({ left: x, top: y });

useLayoutEffect(() => {
  const menu = ref.current;
  if (!menu) return;
  setPosition(contextMenuPosition(x, y, menu.offsetWidth, menu.offsetHeight));
}, [entry.kind, x, y]);
```

Use `offsetWidth/offsetHeight` because they are not distorted by the menu's opening transform animation.

- **Step 4: Render the existing menu through `document.body`**

Keep the menu markup and event handlers intact, but return it with a portal:

```tsx
return createPortal(
  <div
    ref={ref}
    className="file-context-menu"
    role="menu"
    aria-label={`${entry.name} 操作`}
    style={position}
  >
    {item("open", entry.kind === "directory" ? <FolderOpen size={15} /> : <FileText size={15} />, entry.kind === "directory" ? "打开" : "预览")}
    {entry.kind === "file" && item("download", <Download size={15} />, "下载")}
    {item("details", <Info size={15} />, "详细信息")}
    <div className="menu-separator" />
    {item("rename", <Pencil size={15} />, "重命名")}
    {item("move", <Move size={15} />, "移动")}
    {item("delete", <Trash2 size={15} />, "删除", true)}
  </div>,
  document.body,
);
```

Do not modify `showContextMenu`, the row `onContextMenu` handler, or the ellipsis button anchor calculation.

- **Step 5: Build the production frontend**

Run:

```powershell
cd web
npm run build
```

Expected: TypeScript and Vite finish successfully. Existing bundle-size warnings are acceptable; TypeScript or JSX errors are not.

### Task 3: Verify Pointer Anchoring And Viewport Avoidance

**Files:**
- Verify: `web/src/components/FileManagerDialogs.tsx`
- Verify: `web/src/pages/FilesPage.tsx`

- **Step 1: Re-run the center-position browser check**

At `1280x720`, right-click a file and then a directory away from viewport edges. Compare each requested coordinate with the menu rectangle.

Expected: `menu.left === clientX` and `menu.top === clientY`, allowing at most one CSS pixel of rounding.

- **Step 2: Verify right and bottom edge avoidance**

Right-click within 20 pixels of the viewport's right and bottom edges.

Expected:

```text
menu.left >= 8
menu.top >= 8
menu.right <= viewportWidth - 8
menu.bottom <= viewportHeight - 8
```

- **Step 3: Verify the ellipsis-button menu**

Click the file and directory ellipsis buttons.

Expected: each menu opens beside and below its button, remains inside the viewport, and has no additional shell offset.

- **Step 4: Verify dismissal and focus behavior**

Open the menu repeatedly and test outside click, Escape, and page scroll.

Expected: every action closes the menu, and the first menu item receives focus when opened.

- **Step 5: Check mobile layout and console output**

At `390x844`, open the menu from the ellipsis button and inspect the viewport.

Expected: no overlap outside the viewport and no browser console errors.

- **Step 6: Run final repository checks**

Run:

```powershell
git diff --check
git status --short
```

Expected: no whitespace errors; only the design, plan, and focused menu component changes are present.

- **Step 7: Commit the completed fix**

```powershell
git add .openteams/specs/2026-07-27-file-context-menu-position-design.html `
  .openteams/plans/2026-07-27-file-context-menu-position.md `
  web/src/components/FileManagerDialogs.tsx
git commit -m "fix: anchor file context menu to pointer"
```
