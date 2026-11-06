export type Problem = {
  type?: string;
  title?: string;
  status?: number;
  detail?: string;
  code?: string;
};

export type SignalRow = {
  id: string;
  objective_id: string;
  fact_type: string;
  title: string;
  winner?: { org_id?: string; name?: string };
  buyer?: { org_id?: string; name?: string };
  amount?: { value: string; currency: string };
  categories: string[];
  disposition: string;
  score: number;
  needs_review: boolean;
  reasons: string[];
  unknown_count: number;
  status: string;
  state_version: number;
  content_version: number;
  feedback_version: number;
  my_feedback?: string;
  backfill: boolean;
  updated_recently: boolean;
  policy?: { version?: string | number; [key: string]: unknown };
  evaluated_at: string;
  created_at: string;
  procurement_project_id?: string;
};

export type SignalPage = {
  objective_id: string;
  signals: SignalRow[];
  next_cursor?: string;
  viewer_can_feedback: boolean;
};

export type ObjectPage = {
  object: { id?: string; type?: string; name?: string; [key: string]: unknown };
  basis?: { view?: string; [key: string]: unknown };
  signal_context?: { signal_id?: string; [key: string]: unknown };
  signal_context_unavailable?: boolean;
  sections: string[];
  watermark?: { [key: string]: unknown };
};

export type ObjectSection = {
  object_id: string;
  section: string;
  status: "ok" | "empty" | "unavailable" | "history_unavailable" | "not_connected" | string;
  reason_code?: string;
  basis?: { view?: string; [key: string]: unknown };
  as_of?: string;
  time_axis?: string;
  items: Array<Record<string, unknown>>;
  next_cursor?: string;
};

export type EvidenceView = {
  evidence_id: string;
  kind: string;
  display_level: "full_text" | "excerpt" | "citation_only" | "none";
  reason_code?: string;
  object_id?: string;
  source: { name?: string; title?: string; url?: string; reachable?: boolean };
  metadata_only: boolean;
  retracted: boolean;
  snippet?: string;
  context?: string;
  locator?: string;
  record_key?: string;
  record_value?: string;
};

export type BriefingSummary = {
  id: string;
  source: string;
  period: { from: string; to: string };
  assembled_at: string;
  scheduled_at: string;
  empty: boolean;
  partial: boolean;
  delayed: boolean;
  sections?: Array<{ key?: string; title?: string; count?: number; [key: string]: unknown }>;
};

export type Briefing = BriefingSummary & {
  content_as_of: string;
  sections: Array<Record<string, unknown>>;
  deliveries: Array<Record<string, unknown>>;
};

export type BriefingLinkTarget = { kind: "signal" | "briefing" | "approval"; id: string; from_signal?: boolean };
export type BindingPreview = { channel: string; corp_id: string; corp_name?: string; channel_user_id: string; display_name?: string; subject: string; expires_at: string; confirmation: string };
export type BindingConfirmation = { binding: Record<string, unknown>; outcome: "bound" | "already_bound" | string };

export type WebSession = {
  subject: string;
  tenant?: string;
  cell?: string;
  csrf_token: string;
  expires_at: string;
};

export type Profile = {
  version: number;
  context_version: number;
  content: {
    subject_type?: string;
    entities?: Array<Record<string, unknown>>;
    value_chain_roles?: string[];
    industry_packs?: string[];
    offering_categories?: string[];
    brands?: string[];
    service_regions?: string[];
    qualifications?: Array<Record<string, unknown>>;
  };
};

export type Objective = {
  id: string;
  template_id: string;
  template_version: string;
  policy_template_id: string;
  policy_template_version: string;
  org_unit_id: string;
  state: "inactive" | "active" | "disabled" | string;
  owner: string;
  participants: string[];
  params: Record<string, unknown>;
  version: number;
};

export type Policy = {
  objective_id: string;
  generation: number;
  active_version: number;
  active?: Record<string, unknown>;
  draft_version: number;
  draft?: { revision?: number; valid?: boolean; form?: PolicyForm; simulation?: Record<string, unknown>; [key: string]: unknown };
  request?: Record<string, unknown>;
};

export type PolicyRequest = {
  approval_id: string;
  kind: "activation" | "rollback" | string;
  state: string;
  object_version: number;
  simulation_id: string;
  baseline_version: number;
  baseline_generation: number;
  backfill_days?: number;
  requested_by: string;
};

export type ApprovalRequest = {
  id: string;
  type: string;
  resource: string;
  applicant: string;
  note: string;
  state: string;
  version: number;
  submitted_at: string;
  expires_at: string;
  decided_by?: string;
  decision_reason?: string;
  decided_at?: string;
  closed_reason?: string;
  closed_at?: string;
};

export type DecideApprovalRequest = {
  expected_version: number;
  decision: "approve" | "reject";
  reason?: string;
};

export type SimulationRun = {
  id: string;
  objective_id: string;
  revision: number;
  status: "completed" | "failed" | string;
  failure_reason?: string;
  inputs?: Record<string, unknown>;
  report?: Record<string, unknown>;
  created_at: string;
  completed_at?: string;
};

export type TriageRequest = {
  operation_id: string;
  action: "feedback" | "withdraw_feedback" | "dismiss" | "restore" | "adjust";
  seen_content_version: number;
  state_version: number;
  feedback_version: number;
  feedback?: { result: "relevant" | "irrelevant" | "exclusion_wrong"; reasons?: string[]; note?: string };
  dismiss?: boolean;
  manual_disposition?: "none" | "pursue_candidate" | "review_candidate" | "watch_signal";
  reason?: string;
  channel?: "web" | "wecom";
};

export type TriageResult = {
  operation_id: string;
  signal_id: string;
  action: string;
  result: "succeeded" | "no_change";
  parts: Record<string, unknown>;
  state: {
    status: string;
    manual_disposition: string;
    state_version: number;
    content_version: number;
    feedback_version: number;
    feedback?: Record<string, unknown>;
  };
};

export type BatchItemInput = { signal_id: string; seen_content_version: number; state_version: number; feedback_version: number };
export type SignalBatch = { id: string; objective_id: string; action: string; state: string; summary: Record<string, unknown>; items: Array<Record<string, unknown>>; created_at: string; updated_at: string };

export type PolicyForm = {
  params: Record<string, unknown>;
  rules: Array<Record<string, unknown>>;
  thresholds: { base_score: number; pursue_at: number; drop_below: number };
  notify: Record<string, unknown>;
};

export type ApiFailure = {
  kind: "unauthenticated" | "forbidden" | "not_found" | "unavailable" | "unknown";
  problem: Problem;
};

export type ApiResult<T> = { ok: true; data: T } | { ok: false; error: ApiFailure };

function classify(status: number, problem: Problem): ApiFailure["kind"] {
  if (status === 401) return "unauthenticated";
  if (status === 403) return "forbidden";
  if (status === 404) return "not_found";
  if (status === 408 || status === 429 || status >= 500) return "unavailable";
  return "unknown";
}

export class MiaoQiaApi {
  constructor(private readonly baseUrl = "") {}

  private async get<T>(path: string, query?: Record<string, string | undefined>): Promise<ApiResult<T>> {
    const url = new URL(`${this.baseUrl}${path}`, window.location.origin);
    for (const [key, value] of Object.entries(query ?? {})) {
      if (value !== undefined && value !== "") url.searchParams.set(key, value);
    }
    try {
      const response = await fetch(url, { credentials: "include", headers: { Accept: "application/json" } });
      const body = (await response.json().catch(() => ({}))) as T | Problem;
      if (!response.ok) {
        const problem = body as Problem;
        return { ok: false, error: { kind: classify(response.status, problem), problem } };
      }
      return { ok: true, data: body as T };
    } catch (cause) {
      return {
        ok: false,
        error: { kind: "unavailable", problem: { title: "服务暂时不可用", detail: String(cause) } },
      };
    }
  }

  private async post<T>(path: string, headers: Record<string, string> = {}): Promise<ApiResult<T>> {
    try {
      const response = await fetch(`${this.baseUrl}${path}`, {
        method: "POST",
        credentials: "include",
        headers: { Accept: "application/json", ...headers },
      });
      const body = (await response.json().catch(() => ({}))) as T | Problem;
      if (!response.ok) {
        const problem = body as Problem;
        return { ok: false, error: { kind: classify(response.status, problem), problem } };
      }
      return { ok: true, data: body as T };
    } catch (cause) {
      return { ok: false, error: { kind: "unavailable", problem: { title: "服务暂时不可用", detail: String(cause) } } };
    }
  }

  private async sendJson<T>(method: "PUT" | "POST", path: string, body: unknown, headers: Record<string, string> = {}): Promise<ApiResult<T>> {
    try {
      const response = await fetch(`${this.baseUrl}${path}`, {
        method,
        credentials: "include",
        headers: { Accept: "application/json", "Content-Type": "application/json", ...headers },
        body: JSON.stringify(body),
      });
      const payload = (await response.json().catch(() => ({}))) as T | Problem;
      if (!response.ok) {
        const problem = payload as Problem;
        return { ok: false, error: { kind: classify(response.status, problem), problem } };
      }
      return { ok: true, data: payload as T };
    } catch (cause) {
      return { ok: false, error: { kind: "unavailable", problem: { title: "服务暂时不可用", detail: String(cause) } } };
    }
  }

  getSession() {
    return this.get<WebSession>("/auth/session");
  }

  getProfile() {
    return this.get<Profile>("/v1/customer-context/profile");
  }

  listObjectives() {
    return this.get<{ objectives: Objective[] }>("/v1/customer-context/objectives");
  }

  getObjective(objectiveId: string) {
    return this.get<Objective>(`/v1/customer-context/objectives/${encodeURIComponent(objectiveId)}`);
  }

  getPolicy(objectiveId: string) {
    return this.get<Policy>(`/v1/customer-context/objectives/${encodeURIComponent(objectiveId)}/policy`);
  }

  draftPolicy(objectiveId: string, expectedVersion: number, form: PolicyForm) {
    return this.sendJson<Policy>("PUT", `/v1/customer-context/objectives/${encodeURIComponent(objectiveId)}/policy/draft`, { expected_version: expectedVersion, form }, { "Idempotency-Key": `web-draft-${crypto.randomUUID()}` });
  }

  requestPolicyActivation(objectiveId: string, request: { expected_version: number; revision: number; simulation_id: string; note: string; backfill_days?: number }) {
    return this.sendJson<PolicyRequest>("POST", `/v1/customer-context/objectives/${encodeURIComponent(objectiveId)}/policy/activation-requests`, request, { "Idempotency-Key": `web-activation-${crypto.randomUUID()}` });
  }

  getApproval(approvalId: string) {
    return this.get<ApprovalRequest>(`/v1/identity/approvals/${encodeURIComponent(approvalId)}`);
  }

  decideApproval(approvalId: string, request: DecideApprovalRequest) {
    return this.sendJson<ApprovalRequest>("POST", `/v1/identity/approvals/${encodeURIComponent(approvalId)}/decide`, request, { "Idempotency-Key": `web-approval-${crypto.randomUUID()}` });
  }

  getSimulation(simulationId: string) {
    return this.get<SimulationRun>(`/v1/learning/simulations/${encodeURIComponent(simulationId)}`);
  }

  logout(csrf: string) {
    return this.post<never>("/auth/logout", {
      "X-Miaoqia-Request": "1",
      "X-Miaoqia-CSRF": csrf,
    });
  }

  listSignals(objectiveId: string, tab = "open") {
    return this.get<SignalPage>(`/v1/detection/objectives/${encodeURIComponent(objectiveId)}/signals`, { tab });
  }

  getSignal(signalId: string) {
    return this.get<SignalRow>(`/v1/detection/signals/${encodeURIComponent(signalId)}`);
  }

  triageSignal(signalId: string, request: TriageRequest) {
    return this.sendJson<TriageResult>("POST", `/v1/detection/signals/${encodeURIComponent(signalId)}/triage`, request, { "Idempotency-Key": `web-triage-${request.operation_id}` });
  }

  triageSignals(objectiveId: string, request: { action: "relevant" | "irrelevant" | "dismiss" | "restore" | "exclusion_correct" | "exclusion_wrong"; items: BatchItemInput[]; reasons?: string[]; note?: string; dismiss?: boolean }) {
    return this.sendJson<SignalBatch>("POST", `/v1/detection/objectives/${encodeURIComponent(objectiveId)}/signal-batches`, request, { "Idempotency-Key": `web-batch-${crypto.randomUUID()}` });
  }

  getObject(objectId: string, signalId?: string, view = "current") {
    return this.get<ObjectPage>(`/v1/objects/${encodeURIComponent(objectId)}`, { signal_id: signalId, view });
  }

  getObjectSection(objectId: string, section: string, signalId?: string, view = "current") {
    return this.get<ObjectSection>(`/v1/objects/${encodeURIComponent(objectId)}/sections/${encodeURIComponent(section)}`, {
      signal_id: signalId,
      view,
    });
  }

  getEvidence(evidenceId: string) {
    return this.get<EvidenceView>(`/v1/evidence/${encodeURIComponent(evidenceId)}`);
  }

  listBriefings() {
    return this.get<{ briefings: BriefingSummary[] }>("/v1/channels/briefings");
  }

  getBriefing(briefingId: string) {
    return this.get<Briefing>(`/v1/channels/briefings/${encodeURIComponent(briefingId)}`);
  }

  openBriefingLink(token: string) {
    return this.sendJson<BriefingLinkTarget>("POST", "/v1/channels/briefing-links:open", { token }, { "Idempotency-Key": `web-link-${crypto.randomUUID()}` });
  }

  prepareBinding(linkToken: string, oauthCode?: string) {
    return this.sendJson<BindingPreview>("POST", "/v1/channels/bindings:prepare", { link_token: linkToken, ...(oauthCode ? { oauth_code: oauthCode } : {}) }, { "Idempotency-Key": `web-bind-prepare-${crypto.randomUUID()}` });
  }

  confirmBinding(linkToken: string, confirmation: string, oauthCode?: string) {
    return this.sendJson<BindingConfirmation>("POST", "/v1/channels/bindings:confirm", { link_token: linkToken, confirmation, ...(oauthCode ? { oauth_code: oauthCode } : {}) }, { "Idempotency-Key": `web-bind-confirm-${crypto.randomUUID()}` });
  }
}

export const api = new MiaoQiaApi();
