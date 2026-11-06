import type { ReactNode } from "react";
import { problemCopy } from "@miaoqia/copy";

export function PageFrame({ title, eyebrow, children }: { title: string; eyebrow?: string; children: ReactNode }) {
  return (
    <main className="page-frame">
      {eyebrow ? <div className="eyebrow">{eyebrow}</div> : null}
      <h1>{title}</h1>
      {children}
    </main>
  );
}

export function StatusPanel({ kind, detail, action }: { kind: keyof typeof problemCopy; detail?: string; action?: ReactNode }) {
  const copy = problemCopy[kind];
  return (
    <section className={`status-panel status-${kind}`} role="status">
      <div className="status-icon">{kind === "unauthenticated" ? "↪" : kind === "forbidden" ? "⊘" : "…"}</div>
      <div>
        <h2>{copy.title}</h2>
        <p>{detail ?? copy.next}</p>
        {action ? <div className="status-action">{action}</div> : null}
      </div>
    </section>
  );
}

export function StateBadge({ children, tone = "neutral" }: { children: ReactNode; tone?: "neutral" | "good" | "warn" }) {
  return <span className={`state-badge state-${tone}`}>{children}</span>;
}
