<!-- markdownlint-disable MD013 -->
# 1. Frontend Re-baseline on the Registry Stack

**Status**: Accepted

## Context

A core goal of the revitalization is that the State Manager UI be **indistinguishable** from the Terraform Registry UI in style, components, iconography, CSS, layout, i18n, authentication, and admin controls — a shared design philosophy that could later be extracted into a standalone framework.

`terraform-state-manager-frontend` is a v0.1 first draft that was clearly seeded from an *older* registry frontend: the two still share `AuthContext`, `ThemeContext`, `HelpContext`, `Layout`, `ProtectedRoute`, `DevUserSwitcher`, and `SetupWizardPage`. But the registry has since moved well ahead and the State Manager frontend is behind on every parity axis:

- React 18 vs **19**; MUI 6 vs **9**.
- **No** i18n (registry has `i18next` with 10 locales).
- **No** icon system (registry uses FontAwesome + simple-icons).
- **No** `react-query`, command palette, or accessibility/consent/announcer infrastructure.

Three ways to reach parity were considered:

1. **Retrofit in place** — incrementally upgrade the existing frontend.
2. **Rebuild on the current registry scaffold** — re-baseline from the registry FE as it exists today, then port State Manager pages onto it.
3. **Extract a shared design-system framework first**, then have both apps consume it.

## Decision

Re-baseline the State Manager frontend on a copy of the **current** registry frontend scaffold (React 19 / MUI 9 theme + `ThemeContext`, `i18next` + locale files, the icon system, `react-query`, `Layout`, auth/route/setup infrastructure, command palette, a11y), then port the existing State Manager feature pages (analysis, snapshots, backups, migrations, compliance, reports, alerts, scheduler) onto it with i18n keys.

Defer extracting a shared design-system framework until **both** apps are stable and the truly-shared surface is known — extracting it now would be premature abstraction and would churn the production registry app.

### Alternatives considered

- **Retrofit in place** rejected: the gap (major React/MUI versions, missing i18n/icons/query/a11y) is large enough that retrofitting would chase registry drift indefinitely and never fully close to "identical."
- **Extract framework first** rejected *for now*: no second stable consumer exists yet, so the abstraction boundary is unknown; it would also force risky refactoring of the production registry frontend up front. It remains the intended end state (see Consequences).

## Consequences

**Easier**:

- Fastest credible route to true visual/interaction/i18n/auth/admin parity, because the admin and shell screens are copied from the reference rather than reimplemented.
- The shared identity backend (ADR 002) means admin screens talk to the same API and behave identically by construction.

**Harder**:

- A larger up-front re-baseline than an incremental retrofit.
- Until the shared framework is extracted, registry design changes must be re-applied to the State Manager frontend by hand; the two will drift between syncs.
- Extraction of the shared framework is deferred work that must be scheduled once both apps stabilize.
