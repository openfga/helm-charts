# Architecture Decision Records

This directory contains Architecture Decision Records (ADRs) for the OpenFGA Helm Charts project.

ADRs are short documents that capture significant architectural decisions along with their context, alternatives considered, and consequences. They serve as a decision log — not a living design doc, but a point-in-time record of *why* a decision was made.

We follow the format described by [Michael Nygard](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions).

## Index

| ADR | Title | Status | Date |
|-----|-------|--------|------|
| [ADR-001](001-adopt-openfga-operator.md) | Adopt a Kubernetes Operator for OpenFGA Lifecycle Management | Proposed | 2026-04-06 |
| [ADR-002](002-operator-managed-migrations.md) | Replace Helm Hook Migrations with Operator-Managed Migrations | Proposed | 2026-04-06 |
| [ADR-003](003-declarative-store-lifecycle-crds.md) | Declarative Store Lifecycle Management via CRDs | Proposed | 2026-04-06 |
| [ADR-004](004-operator-deployment-model.md) | Operator Deployment as Helm Subchart Dependency | Proposed | 2026-04-06 |

---

## What is an ADR?

An ADR captures a single architectural decision. It records:

- **What** was decided
- **Why** it was decided (the context and constraints at the time)
- **What alternatives** were considered and why they were rejected
- **What consequences** follow from the decision (positive, negative, and neutral)

ADRs are **immutable once accepted** — if a decision changes, you write a new ADR that supersedes the old one rather than editing it. This preserves the history of *why* things changed over time.

## ADR Lifecycle

```
Proposed → Accepted → (optionally) Superseded or Deprecated
              ↑
              │ feedback loop
              │
         Discussion
```

### Statuses

| Status | Meaning |
|--------|---------|
| **Proposed** | The ADR has been written and is open for discussion. No commitment has been made. |
| **Accepted** | The decision has been agreed upon by maintainers. Implementation can proceed. |
| **Deprecated** | The decision is no longer relevant (e.g., the feature was removed). |
| **Superseded by ADR-XXX** | A newer ADR has replaced this decision. The old ADR links to the new one. |

## How to Propose an ADR

1. **Create a branch** — e.g., `docs/adr-005-my-decision`

2. **Copy the template** — use `000-template.md` as a starting point

3. **Write the ADR** — fill in Context, Decision, and Consequences. Focus on *why*, not *how*. The most valuable part is the Alternatives Considered section — it shows reviewers what you evaluated and why you chose this path.

4. **Assign a number** — use the next sequential number. Check the index above.

5. **Open a pull request** — the PR is where discussion happens. Title it: `ADR-005: <title>`

6. **Add to the index** — update the table in this README with the new entry (status: Proposed)

### Proposing related ADRs together

When multiple ADRs are part of a single cohesive proposal — e.g., a foundational decision and several downstream decisions that depend on it — they can be submitted in a single PR. This lets reviewers see the full picture instead of bouncing between separate PRs.

When doing this:

- **Explain the relationship in the PR description** — identify which ADR is the foundational decision and which are downstream. For example: "ADR-001 is the core decision to build an operator. ADR-002, 003, and 004 are downstream decisions about how the operator handles migrations, CRDs, and deployment."
- **Each ADR can be accepted or rejected independently** — a reviewer might approve the foundational decision but push back on a downstream one. If that happens, split the PR: merge the accepted ADRs and keep the contested ones open for further discussion.
- **Keep each ADR self-contained** — even though they're in the same PR, each ADR should stand on its own. A reader should be able to understand ADR-003 without reading ADR-002 first (though they may reference each other).

## How to Give Feedback on an ADR

ADR review happens in the **pull request**, not by editing the ADR directly. This keeps the discussion visible and linked to the decision.

### As a reviewer

- **Comment on the PR** — ask questions, challenge assumptions, suggest alternatives. Good review questions:
  - "Did you consider X as an alternative?"
  - "What happens if Y fails?"
  - "This conflicts with how we do Z — can you address that?"
  - "I agree with the decision but the consequence about X should mention Y"

- **Request changes** if you believe the decision is wrong or incomplete

- **Approve** when you're satisfied the decision is sound and well-documented

### As the author responding to feedback

- **Update the ADR in the PR** based on feedback:
  - Add alternatives that reviewers suggested (with your evaluation of them)
  - Expand the Consequences section if reviewers identified impacts you missed
  - Clarify the Context if reviewers were confused about the problem
  - Adjust the Decision if feedback reveals a better approach

- **Do NOT delete feedback-driven changes** — if a reviewer raised a valid alternative and you addressed it, the ADR is stronger for including it

- **Resolve PR comments** as you address them so reviewers can track progress

### Reaching consensus

- ADRs move to **Accepted** when maintainers approve the PR
- Not every maintainer needs to approve — follow the project's normal review standards
- If consensus can't be reached, escalate to a synchronous discussion (meeting, call) and record the outcome in the PR
- Disagreement is fine — document it in the Consequences section as a risk or trade-off rather than hiding it

## How to Supersede an ADR

When a decision needs to change:

1. **Do NOT edit the original ADR** — it's a historical record

2. **Write a new ADR** that references the old one:
   ```markdown
   - **Supersedes:** [ADR-002](002-operator-managed-migrations.md)
   ```

3. **Update the old ADR's status** — change it to:
   ```markdown
   - **Status:** Superseded by [ADR-007](007-new-approach.md)
   ```

4. **Update the index** in this README

This way, anyone reading ADR-002 knows it's been replaced and can follow the link to understand what changed and why.

## ADR Format

Every ADR follows this structure:

```markdown
# ADR-NNN: Title

- **Status:** Proposed | Accepted | Deprecated | Superseded by ADR-XXX
- **Date:** YYYY-MM-DD
- **Deciders:** Who was involved in the decision
- **Related Issues:** GitHub issue references
- **Related ADR:** Links to related ADRs

## Context

What is the problem or situation that motivates this decision?
Include enough background that someone unfamiliar with the project
can understand why this decision matters.

## Decision

What is the decision and why was it chosen?

### Alternatives Considered

What other options were evaluated? Why were they rejected?
This is often the most valuable section — it prevents future
contributors from re-proposing rejected approaches.

## Consequences

### Positive
What improves as a result of this decision?

### Negative
What gets harder or more complex? Be honest — every decision has costs.

### Risks
What could go wrong? What assumptions might prove false?
```

## Template

A blank template is available at [000-template.md](000-template.md).

## Tips for Writing Good ADRs

- **Keep it short** — an ADR is one decision, not a design doc. If it's longer than 2-3 pages, consider splitting it.
- **Focus on why, not how** — implementation details change; the reasoning behind the decision is what matters long-term.
- **Be honest about trade-offs** — an ADR that lists only positive consequences isn't credible. Every decision has costs.
- **Write for your future self** — in 18 months, you won't remember why you chose this. The ADR should tell you.
- **Not every decision needs an ADR** — use ADRs for decisions that are hard to reverse, affect multiple components, or where the reasoning isn't obvious from the code.
