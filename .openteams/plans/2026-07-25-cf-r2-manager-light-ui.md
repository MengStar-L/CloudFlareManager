# CF-R2Manager Light UI Implementation Plan

**Goal:** Replace the current dark, cramped administration UI with the approved light cloud-workspace design, accessible animated controls, and responsive desktop/mobile layouts without changing backend behavior.

**Architecture:** Keep every existing page and API call, but add a small presentation layer built from Motion and React Aria Components. Split the global stylesheet into design tokens, foundations, reusable components, page layouts, and responsive rules; Vite continues to bundle all assets into the Go binary.

**Tech Stack:** React 19, TypeScript 5.7, Vite 6, Motion, React Aria Components, Lucide React, local Manrope variable font, Go embedded frontend.

---

## File Map

- Create `web/src/styles/tokens.css`: color, type, spacing, elevation, radius, and motion variables.
- Create `web/src/styles/base.css`: reset, font loading, body, inputs, focus, and reduced-motion behavior.
- Create `web/src/styles/components.css`: buttons, panels, tables, status, forms, tabs, popovers, dialogs, toasts, and skeletons.
- Create `web/src/styles/pages.css`: login, overview, R2, D1, Workers AI, access, and activity layouts.
- Create `web/src/styles/responsive.css`: tablet and mobile shell, drawer, grids, forms, tabs, and tables.
- Modify `web/src/styles.css`: keep it as the ordered stylesheet aggregator.
- Create `web/src/components/SelectField.tsx`: controlled Select/ComboBox abstraction with hidden form value support.
- Create `web/src/components/Motion.tsx`: route, panel, and stagger wrappers with reduced-motion support.
- Create `web/src/components/ConfirmDialog.tsx`: accessible destructive-action confirmation, including optional password input.
- Create `web/src/components/Toast.tsx`: provider and hook for transient success feedback.
- Create `web/src/components/Skeleton.tsx`: stable loading placeholders.
- Modify `web/src/components/UI.tsx`: animated header, empty/error/status primitives, and shared class contracts.
- Modify `web/src/components/AppShell.tsx`: light desktop navigation and mobile drawer.
- Modify `web/src/components/Login.tsx`: approved light login composition and animated authentication state.
- Modify `web/src/App.tsx`: providers and keyed route transitions.
- Modify every file under `web/src/pages/*.tsx`: migrate selects, confirmations, tabs, loading states, and page surface classes.
- Modify `web/src/main.tsx`: import the local Manrope variable font before application styles.
- Modify `web/package.json` and `web/package-lock.json`: add UI dependencies.
- Modify `NOTICE`: record the Manrope OFL font and third-party UI libraries.

The workspace has no `.git` directory, so the plan uses verification checkpoints instead of commit steps.

### Task 1: Install Local UI Dependencies

**Files:**
- Modify: `web/package.json`
- Modify: `web/package-lock.json`
- Modify: `web/src/main.tsx`
- Modify: `NOTICE`

- **Step 1: Record the pre-change frontend baseline**

Run:

```powershell
Set-Location D:\Code\CF-R2Manager\web
npm run lint
npm run build
```

Expected: TypeScript exits with code 0 and Vite writes `web/dist`.

- **Step 2: Install the approved dependencies**

Run:

```powershell
Set-Location D:\Code\CF-R2Manager\web
npm install motion react-aria-components @fontsource-variable/manrope
```

Expected: `package.json` and `package-lock.json` contain all three packages and npm exits with code 0.

- **Step 3: Load the bundled font before the application styles**

Change `web/src/main.tsx` imports to:

```tsx
import "@fontsource-variable/manrope/wght.css";
import "./styles.css";
```

Expected: the font is emitted by Vite as a local hashed asset and no external font request appears at runtime.

- **Step 4: Add third-party notices**

Append to `NOTICE`:

```text
The web interface uses Motion and React Aria Components under the MIT License.
The bundled Manrope variable font is licensed under the SIL Open Font License 1.1.
```

- **Step 5: Verify dependency integration**

Run `npm run lint` from `web`.

Expected: exit code 0 with no unresolved modules.

### Task 2: Establish the Light Design System

**Files:**
- Create: `web/src/styles/tokens.css`
- Create: `web/src/styles/base.css`
- Create: `web/src/styles/components.css`
- Create: `web/src/styles/pages.css`
- Create: `web/src/styles/responsive.css`
- Modify: `web/src/styles.css`

- **Step 1: Create design tokens**

Define the approved palette and dimensions in `tokens.css`:

```css
:root {
  --canvas: #f4f7f8;
  --surface: #ffffff;
  --surface-soft: #f8fafb;
  --ink: #1c252c;
  --muted: #66757c;
  --line: #dfe7e9;
  --primary: #2b8a75;
  --primary-soft: #e8f3f0;
  --brand: #e85d2a;
  --info: #347ca5;
  --danger: #c94b46;
  --warning: #a96a18;
  --radius-sm: 6px;
  --radius-md: 8px;
  --shadow-1: 0 8px 26px rgba(39, 56, 63, .055);
  --motion-fast: 160ms;
  --motion-base: 220ms;
  --ease-out: cubic-bezier(.22, 1, .36, 1);
}
```

- **Step 2: Move reset and foundations into `base.css`**

Use `Manrope Variable` for Latin and numeric emphasis, retain system Chinese fallbacks, style focus with `:focus-visible`, and add:

```css
@media (prefers-reduced-motion: reduce) {
  *, *::before, *::after {
    scroll-behavior: auto !important;
    animation-duration: .01ms !important;
    animation-iteration-count: 1 !important;
    transition-duration: .01ms !important;
  }
}
```

- **Step 3: Rebuild reusable component styles**

In `components.css`, define stable 38px controls, 34px icon buttons, 6-8px radii, light tables, animated tab indicators, React Aria popovers/listboxes, dialog overlays, toast stack, and skeleton shimmer. Limit animated properties to `transform` and `opacity` except color/border transitions.

- **Step 4: Move domain layout styles**

In `pages.css`, preserve existing class names for `.metric-grid`, `.d1-layout`, `.playground`, `.rebalance-form`, `.result-view`, and `.editor-frame`, then update their spacing and light surfaces. Keep Monaco and preformatted output dark.

- **Step 5: Add responsive behavior**

In `responsive.css`, implement:

```css
@media (max-width: 960px) { /* compact desktop/tablet grids */ }
@media (max-width: 720px) { /* top bar, drawer, single-column forms */ }
```

The 720px layout must replace the horizontal navigation strip with a menu button and drawer, keep all text inside controls, and allow only table containers to scroll horizontally.

- **Step 6: Convert `styles.css` into an ordered aggregator**

```css
@import "./styles/tokens.css";
@import "./styles/base.css";
@import "./styles/components.css";
@import "./styles/pages.css";
@import "./styles/responsive.css";
```

- **Step 7: Verify CSS compilation**

Run `npm run build` from `web`.

Expected: Vite exits with code 0, all CSS imports resolve, and the only acceptable warning is the existing Monaco chunk-size warning.

### Task 3: Build Accessible Animated Primitives

**Files:**
- Create: `web/src/components/SelectField.tsx`
- Create: `web/src/components/Motion.tsx`
- Create: `web/src/components/ConfirmDialog.tsx`
- Create: `web/src/components/Toast.tsx`
- Create: `web/src/components/Skeleton.tsx`
- Modify: `web/src/components/UI.tsx`

- **Step 1: Define the Select contract**

Use this public interface in `SelectField.tsx`:

```tsx
export interface SelectOption {
  value: string;
  label: string;
  description?: string;
}

export interface SelectFieldProps {
  label: string;
  value: string;
  options: SelectOption[];
  onChange: (value: string) => void;
  name?: string;
  placeholder?: string;
  disabled?: boolean;
  required?: boolean;
  searchable?: boolean;
  className?: string;
}
```

Render React Aria `Select` when `searchable ?? options.length > 8` is false and `ComboBox` otherwise. Always render `<input type="hidden" name={name} value={value} />` when `name` is present so existing `FormData` handlers retain their behavior.

- **Step 2: Implement Select and ComboBox behavior**

Both variants must use `Label`, `Button`, `Popover`, `ListBox`, and `ListBoxItem`; the ComboBox also uses `Input`. Convert selected keys to strings, show `Check` for the selected item, show `ChevronDown` in the trigger, filter case-insensitively by label and description, and expose the empty result as `没有匹配项`.

- **Step 3: Add reusable Motion wrappers**

Export these components from `Motion.tsx`:

```tsx
export function PageTransition({ pageKey, children }: { pageKey: string; children: ReactNode }): JSX.Element;
export function Reveal({ children, delay?: number, className?: string }: { children: ReactNode; delay?: number; className?: string }): JSX.Element;
export function StaggerList({ children, className?: string }: { children: ReactNode; className?: string }): JSX.Element;
```

Use Motion variants for 12px page movement, 35ms child staggering, and `useReducedMotion()` to replace motion with opacity-only transitions.

- **Step 4: Add confirmation dialog behavior**

`ConfirmDialog` accepts `open`, `title`, `description`, `confirmLabel`, `danger`, optional `passwordLabel`, `onOpenChange`, and async `onConfirm(password)`. Keep focus inside the dialog, close on Escape, disable controls during submission, and restore focus through React Aria Dialog behavior.

- **Step 5: Add toast and skeleton primitives**

`ToastProvider` exposes `useToast().show(message, tone)` with `success`, `info`, and `error` tones. `Skeleton` accepts `width`, `height`, and `className`, uses `aria-hidden`, and never changes its parent dimensions.

- **Step 6: Update shared UI primitives**

Wrap `PageHeader` and empty states with `Reveal`, retain the current `ErrorBanner` API, and update `Status` so `pending`/`running` receive the animated status class while errors remain static.

- **Step 7: Type-check primitives**

Run `npm run lint`.

Expected: exit code 0 and no unsafe selection-key or ReactNode type errors.

### Task 4: Rebuild the Application Shell and Login

**Files:**
- Modify: `web/src/App.tsx`
- Modify: `web/src/components/AppShell.tsx`
- Modify: `web/src/components/Login.tsx`

- **Step 1: Add global providers and route animation**

Wrap authenticated content in `ToastProvider`. Render pages through:

```tsx
<PageTransition pageKey={page}>{pages[page]}</PageTransition>
```

Keep `Suspense`, session checking, hash routing, and logout behavior unchanged.

- **Step 2: Implement the light desktop shell**

Keep the existing `PageID` values and navigation labels. Add a status dot beside the product name, animate the active navigation background with Motion `layoutId`, and keep Lucide icons at a stable 18px size.

- **Step 3: Implement mobile drawer navigation**

At 720px and below, show a top bar with a `Menu` icon button. Use React Aria `DialogTrigger`, `Modal`, and `Dialog` for the left drawer. Selecting a page or logging out closes the drawer. The desktop sidebar remains mounted only at desktop size through CSS.

- **Step 4: Recompose the login page**

Use an unframed brand header, a single 390px login panel, subtle CSS grid texture, animated focus ring, and Motion entry. Preserve password autocomplete, busy state, error role, and submit semantics.

- **Step 5: Verify authentication flows**

Run `npm run lint` and `npm run build`.

Expected: both exit 0; the login form remains the only form before authentication and hash navigation still selects valid `PageID` values.

### Task 5: Migrate Forms, Dropdowns, Tabs, and Confirmations

**Files:**
- Modify: `web/src/pages/AccountsPage.tsx`
- Modify: `web/src/pages/StoragePage.tsx`
- Modify: `web/src/pages/D1Page.tsx`
- Modify: `web/src/pages/AIPage.tsx`
- Modify: `web/src/pages/AccessPage.tsx`
- Modify: `web/src/pages/ActivityPage.tsx`

- **Step 1: Replace account deletion confirmation**

Store the selected account in state, open `ConfirmDialog`, and call the existing delete API only from `onConfirm`. On success, close the dialog and show `账号已删除` through `useToast()`.

- **Step 2: Migrate R2 selects and destructive confirmation**

Add controlled state for bucket account, source bucket, and target bucket. Replace all three native selects with `SelectField`, retain hidden form names, prevent identical source/target selections, and replace `window.confirm` for bucket removal with `ConfirmDialog`.

- **Step 3: Migrate D1 controls and password confirmation**

Replace account and schema selects with `SelectField`; schema becomes searchable automatically when more than eight tables/views exist. Replace `window.prompt` database deletion with password-enabled `ConfirmDialog`. Keep the server-side administrator-password requirement.

- **Step 4: Migrate Workers AI controls**

Replace the account select with `SelectField`, replace the model datalist with a searchable `SelectField`/ComboBox, and replace Gateway deletion with `ConfirmDialog`. Preserve prompt submission, streaming output, tab state, and metadata-only logs.

- **Step 5: Migrate access credential controls**

Replace the credential-kind native select with `SelectField`. Replace rotation and revocation confirmations with separate `ConfirmDialog` states and retain the one-time secret display and clipboard action.

- **Step 6: Animate tab and panel transitions**

Use `PageTransition` or `Reveal` around Storage, D1, AI, and Activity tab panels. Keep each tab button's existing text and icon, add `aria-selected`, and keep the panel's stable minimum height to avoid layout jumps.

- **Step 7: Add success feedback without changing API behavior**

Show toasts after account creation/deletion, bucket registration/removal/job scheduling, database creation/deletion/backup, Gateway creation/deletion, and credential creation/rotation/revocation. Continue showing server errors through `ErrorBanner`.

- **Step 8: Type-check every migrated page**

Run `npm run lint`.

Expected: exit code 0 with no remaining `<select` or `window.confirm`/`window.prompt` occurrences in `web/src`.

Verify with:

```powershell
rg -n '<select|window\.confirm|window\.prompt' D:\Code\CF-R2Manager\web\src
```

Expected: no matches and ripgrep exit code 1.

### Task 6: Add Stable Loading and Dense Data Presentation

**Files:**
- Modify: `web/src/pages/OverviewPage.tsx`
- Modify: `web/src/pages/AccountsPage.tsx`
- Modify: `web/src/pages/StoragePage.tsx`
- Modify: `web/src/pages/D1Page.tsx`
- Modify: `web/src/pages/AIPage.tsx`
- Modify: `web/src/pages/AccessPage.tsx`
- Modify: `web/src/pages/ActivityPage.tsx`

- **Step 1: Add page loading state without removing existing data**

For each page, set `loading` only before its first successful load. Subsequent refreshes keep the current table visible and animate the refresh icon. Render skeleton rows only when `loading && data.length === 0`.

- **Step 2: Animate overview metrics**

Render the four metrics through `StaggerList`. Animate displayed values from the previous value to the new value, but return the final value immediately when reduced motion is active. Keep numeric blocks at a fixed minimum width.

- **Step 3: Apply dense table contracts**

Add scoped `data-label` attributes where mobile context benefits, preserve horizontal scrolling for wide D1/AI tables, truncate long IDs visually while keeping the full value in `title`, and keep all row action buttons at 34×34px.

- **Step 4: Verify layout stability**

Run `npm run build`.

Expected: exit code 0; Vite output includes local font assets, page chunks, and CSS without missing asset warnings.

### Task 7: Rebuild the Embedded Binary and Run Automated Checks

**Files:**
- Generated: `web/dist/**`
- Generated: `bin/cf-r2-manager.exe`

- **Step 1: Run frontend checks**

```powershell
Set-Location D:\Code\CF-R2Manager\web
npm run lint
npm run build
```

Expected: both commands exit 0; only the known Monaco chunk-size warning may remain.

- **Step 2: Run backend regression tests**

```powershell
Set-Location D:\Code\CF-R2Manager
go test -count=1 ./...
go vet ./...
```

Expected: all tested packages pass and vet emits no diagnostics.

- **Step 3: Build the embedded Windows test binary**

```powershell
Set-Location D:\Code\CF-R2Manager
go build -trimpath -o bin\cf-r2-manager.exe .\cmd\cf-r2-manager
```

Expected: exit code 0 and `bin/cf-r2-manager.exe` has a current timestamp.

- **Step 4: Restart the local test process**

Stop only the existing `cf-r2-manager` process serving `data/browser-test-config.yaml`, then start the rebuilt binary hidden with:

```powershell
Start-Process -FilePath 'D:\Code\CF-R2Manager\bin\cf-r2-manager.exe' -ArgumentList 'server','--config','D:\Code\CF-R2Manager\data\browser-test-config.yaml' -WorkingDirectory 'D:\Code\CF-R2Manager' -WindowStyle Hidden -PassThru
```

Expected: `http://127.0.0.1:18080/healthz` returns `{"status":"ok"}` and `/readyz` returns `{"status":"ready"}`.

### Task 8: Browser Acceptance Across Pages and Viewports

**Files:**
- Create: `data/screenshots/ui-light-overview-desktop.png`
- Create: `data/screenshots/ui-light-storage-desktop.png`
- Create: `data/screenshots/ui-light-d1-mobile.png`
- Create: `data/screenshots/ui-light-ai-mobile.png`

- **Step 1: Verify desktop shell at 1440×900**

Open the local admin UI, authenticate with the local test password, and verify the overview has a light sidebar, four stable metric cards, no overlapping text, no page-level horizontal overflow, and no console errors.

- **Step 2: Verify every desktop page**

Navigate through 账号, R2 存储, D1 数据库, Workers AI, 访问密钥, and 任务与审计. Open every tab and dropdown at least once. Verify popovers stay inside the viewport and tables remain readable.

- **Step 3: Verify keyboard and dialog behavior**

Use Tab/Arrow/Enter/Escape with a Select and searchable ComboBox. Open a destructive confirmation, verify focus remains inside, cancel it, and verify focus returns to the trigger. Do not confirm a real destructive operation.

- **Step 4: Verify 1024×768 tablet layout**

Confirm grid compaction, stable buttons, no clipped page headings, and no page-level horizontal overflow.

- **Step 5: Verify 390×844 mobile layout**

Confirm the top bar and navigation drawer replace the old horizontal icon strip. Check R2 maintenance, D1 data, AI Gateway, and access-key forms; verify controls stack without overlap and only table wrappers scroll horizontally.

- **Step 6: Verify reduced motion**

Enable reduced-motion emulation, reload, and verify route, list, dropdown, and status animations become effectively instantaneous while all controls remain usable.

- **Step 7: Capture final evidence and restore browser state**

Save the four named screenshots, check console errors once more, reset temporary viewport and motion overrides, and leave the overview tab open for user handoff.

Expected: all acceptance checks pass, screenshots are nonblank, and the user can continue using `http://127.0.0.1:18080`.
