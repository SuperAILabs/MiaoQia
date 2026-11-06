import { useEffect, useMemo, useState } from "react";
import { api, type ApiResult, type Briefing, type BriefingSummary, type EvidenceView, type Objective, type ObjectPage, type ObjectSection, type Policy, type PolicyForm, type Profile, type SignalPage, type SignalRow, type TriageResult, type WebSession } from "@miaoqia/api";
import { navigation } from "@miaoqia/copy";
import { PageFrame, StateBadge, StatusPanel } from "@miaoqia/ui";

type Route =
  | { name: "start" }
  | { name: "signals"; objectiveId: string; signalId?: string }
  | { name: "briefings"; briefingId?: string }
  | { name: "profile" }
  | { name: "objectives"; objectiveId?: string }
  | { name: "object"; objectId: string; signalId?: string }
  | { name: "evidence"; evidenceId: string }
  | { name: "approval"; approvalId: string }
  | { name: "link"; token: string }
  | { name: "channel-confirm"; token: string; oauthCode?: string }
  | { name: "inbox" }
  | { name: "unavailable" };

function routeFromLocation(): Route {
  const parts = window.location.pathname.split("/").filter(Boolean);
  const query = new URLSearchParams(window.location.search);
  if (parts[0] === "signals" && parts[1]) return { name: "signals", objectiveId: parts[1], signalId: parts[3] };
  if (parts[0] === "briefings") return { name: "briefings", briefingId: parts[1] };
  if (parts[0] === "settings" && parts[1] === "profile") return { name: "profile" };
  if (parts[0] === "settings" && parts[1] === "objectives") return { name: "objectives", objectiveId: parts[2] };
  if (parts[0] === "objects" && parts[1]) return { name: "object", objectId: parts[1], signalId: query.get("signal") ?? undefined };
  if (parts[0] === "evidence" && parts[1]) return { name: "evidence", evidenceId: parts[1] };
  if (parts[0] === "approvals" && parts[1]) return { name: "approval", approvalId: parts[1] };
  if ((parts[0] === "links" || parts[0] === "h5") && query.get("token")) return { name: "link", token: query.get("token")! };
  if ((parts[0] === "channel" && parts[1] === "confirm" && query.get("link_token"))) return { name: "channel-confirm", token: query.get("link_token")!, oauthCode: query.get("oauth_code") ?? undefined };
  if (parts[0] === "inbox") return { name: "inbox" };
  if (parts[0] === "unavailable") return { name: "unavailable" };
  return { name: "start" };
}

function navigate(path: string) {
  window.history.pushState({}, "", path);
  window.dispatchEvent(new PopStateEvent("popstate"));
}

export function App() {
  const [route, setRoute] = useState<Route>(() => routeFromLocation());
  const [session, setSession] = useState<ApiResult<WebSession> | null>(null);
  useEffect(() => { void api.getSession().then(setSession); }, []);
  useEffect(() => {
    const onPop = () => setRoute(routeFromLocation());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  return (
    <div className="app-shell">
      <header className="topbar">
        <button className="brand" onClick={() => navigate("/start")} aria-label="回到开始页">
          <span className="brand-mark">妙</span>
          <span>妙洽</span>
        </button>
        <nav className="topnav" aria-label="主导航">
          <NavLink active={route.name === "briefings"} onClick={() => navigate("/briefings")}>{navigation.briefings}</NavLink>
          <NavLink active={route.name === "signals"} onClick={() => navigate("/signals")}>{navigation.signals}</NavLink>
          <NavLink active={route.name === "inbox"} onClick={() => navigate("/inbox")}>{navigation.inbox}</NavLink>
          <NavLink active={route.name === "objectives" || route.name === "profile"} onClick={() => navigate("/settings/objectives")}>{navigation.settings}</NavLink>
          <NavLink active={false} onClick={() => navigate("/unavailable")}>检索 · M3</NavLink>
        </nav>
        {session?.ok ? <div className="session-menu"><span className="session-subject">{session.data.subject}</span><button className="login-link" onClick={() => { void api.logout(session.data.csrf_token).then((result) => { if (result.ok) setSession({ ok: false, error: { kind: "unauthenticated", problem: { code: "unauthenticated" } } }); }); }}>退出</button></div> : <a className="login-link" href={`/auth/login?return_to=${encodeURIComponent(window.location.pathname)}`}>登录</a>}
      </header>
      <div className="route-content">
        {route.name === "start" ? <StartPage /> : null}
        {route.name === "signals" ? <SignalsPage objectiveId={route.objectiveId} signalId={route.signalId} /> : null}
        {route.name === "briefings" ? <BriefingsPage briefingId={route.briefingId} /> : null}
        {route.name === "profile" ? <ProfilePage /> : null}
        {route.name === "objectives" ? <ObjectivesPage objectiveId={route.objectiveId} /> : null}
        {route.name === "object" ? <ObjectPageView objectId={route.objectId} signalId={route.signalId} /> : null}
        {route.name === "evidence" ? <EvidencePage evidenceId={route.evidenceId} /> : null}
        {route.name === "approval" ? <ApprovalPage approvalId={route.approvalId} /> : null}
        {route.name === "link" ? <LinkLandingPage token={route.token} /> : null}
        {route.name === "channel-confirm" ? <ChannelBindingPage token={route.token} oauthCode={route.oauthCode} /> : null}
        {route.name === "inbox" ? <InboxPage /> : null}
        {route.name === "unavailable" ? <UnavailablePage /> : null}
      </div>
    </div>
  );
}

function NavLink({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return <button className={`nav-link ${active ? "active" : ""}`} onClick={onClick}>{children}</button>;
}

function StartPage() {
  return (
    <PageFrame title="从一条信号开始" eyebrow="工作台">
      <section className="hero-card">
        <div>
          <StateBadge tone="good">A02 · 建材供应商</StateBadge>
          <h2>配置目标、收到信号，再回到证据判断。</h2>
          <p>Web 工作台与企业微信内置浏览器共用同一套页面。当前登录会话由 gateway 管理，浏览器不会保存访问令牌。</p>
          <a className="primary-button" href="/auth/login?return_to=/briefings">登录工作台</a>
        </div>
        <div className="hero-flow" aria-label="A02 首条旅程">
          <span>目标配置</span><i>→</i><span>信号收件箱</span><i>→</i><span>对象与证据</span><i>→</i><span>简报</span>
        </div>
      </section>
      <section className="info-grid">
        <InfoCard title="数据状态" body="未登录时不读取业务数据；未接入、未知和暂不可用会分别显示。" />
        <InfoCard title="安全边界" body="页面请求使用 HttpOnly 会话 Cookie。带 Cookie 与 Bearer 的混合请求由 gateway 拒绝。" />
        <InfoCard title="当前开放" body="A02 信号、对象、证据与简报是 L2 首批页面；检索、研究和决策入口按批次开放。" />
      </section>
    </PageFrame>
  );
}

function SignalsPage({ objectiveId, signalId }: { objectiveId: string; signalId?: string }) {
  const [result, setResult] = useState<ApiResult<SignalPage> | null>(null);
  const [detail, setDetail] = useState<ApiResult<SignalRow> | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [batch, setBatch] = useState<ApiResult<import("@miaoqia/api").SignalBatch> | null>(null);
  const [batching, setBatching] = useState(false);
  useEffect(() => { setResult(null); void api.listSignals(objectiveId).then(setResult); }, [objectiveId]);
  useEffect(() => { setDetail(null); if (signalId) void api.getSignal(signalId).then(setDetail); }, [signalId]);
  const toggle = (signalId: string) => setSelected((current) => { const next = new Set(current); if (next.has(signalId)) next.delete(signalId); else next.add(signalId); return next; });
  const runBatch = (action: "relevant" | "irrelevant" | "dismiss") => {
    if (!result?.ok || selected.size === 0 || batching) return;
    const items = result.data.signals.filter((signal) => selected.has(signal.id)).map((signal) => ({ signal_id: signal.id, seen_content_version: signal.content_version, state_version: signal.state_version, feedback_version: signal.feedback_version }));
    setBatching(true); setBatch(null);
    void api.triageSignals(objectiveId, { action, items, ...(action === "irrelevant" ? { reasons: ["OTHER"], note: "工作台批量反馈", dismiss: true } : {}) }).then((next) => { setBatch(next); setBatching(false); setSelected(new Set()); if (next.ok) void api.listSignals(objectiveId).then(setResult); });
  };

  if (result === null) return <LoadingPage title="读取信号收件箱" />;
  if (!result.ok) return <FailurePage error={result.error} />;
  return (
    <PageFrame title="信号收件箱" eyebrow={`目标 ${objectiveId}`}>
      <div className="toolbar"><StateBadge>{result.data.signals.length} 条</StateBadge><span className="muted">按相关性排序 · 只显示当前成员可见目标</span>{result.data.viewer_can_feedback && selected.size > 0 ? <><button className="secondary-button" disabled={batching} onClick={() => runBatch("relevant")}>批量标记相关</button><button className="secondary-button" disabled={batching} onClick={() => runBatch("irrelevant")}>批量不相关</button><button className="secondary-button" disabled={batching} onClick={() => runBatch("dismiss")}>批量忽略</button></> : null}</div>
      {result.data.signals.length === 0 ? <EmptyState title="当前没有待处理信号" body="本期没有可展示的 A02 信号；如果数据来源尚未接入，会在这里明确标注状态。" /> : (
        <div className="signal-list">{result.data.signals.map((signal) => <SignalCard key={signal.id} signal={signal} canFeedback={result.data.viewer_can_feedback} selected={selected.has(signal.id)} onToggle={() => toggle(signal.id)} />)}</div>
      )}
      {batch?.ok ? <div className="save-result"><StateBadge tone="good">批量作业已提交</StateBadge><span className="muted">{batch.data.state} · {JSON.stringify(batch.data.summary)}</span></div> : batch && !batch.ok ? <FailurePage error={batch.error} compact /> : null}
      {signalId && detail?.ok ? <SignalDetail signal={detail.data} /> : null}
      {signalId && detail && !detail.ok ? <FailurePage error={detail.error} compact /> : null}
    </PageFrame>
  );
}

function SignalCard({ signal, canFeedback, selected, onToggle }: { signal: SignalRow; canFeedback: boolean; selected: boolean; onToggle: () => void }) {
  const objectId = signal.procurement_project_id;
  const [triage, setTriage] = useState<ApiResult<TriageResult> | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const submit = (result: "relevant" | "irrelevant") => {
    if (!canFeedback || submitting) return;
    setSubmitting(true);
    setTriage(null);
    void api.triageSignal(signal.id, {
      operation_id: `web-${crypto.randomUUID()}`,
      action: "feedback",
      seen_content_version: signal.content_version,
      state_version: signal.state_version,
      feedback_version: signal.feedback_version,
      feedback: result === "relevant" ? { result } : { result, reasons: ["OTHER"], note: "工作台反馈" },
      dismiss: result === "irrelevant",
      channel: "web",
    }).then((next) => { setTriage(next); setSubmitting(false); });
  };
  return (
    <article className="signal-card">
      <div className="card-heading"><div className="card-heading"><StateBadge tone={signal.updated_recently ? "warn" : "neutral"}>{signal.disposition}</StateBadge>{canFeedback ? <label className="select-signal"><input type="checkbox" checked={selected} onChange={onToggle} /> 选择</label> : null}</div><span className="muted">{new Date(signal.created_at).toLocaleString("zh-CN")}</span></div>
      <h2>{signal.title}</h2>
      <p>{signal.winner?.name ? `中标方：${signal.winner.name}` : "中标方：未能识别"}{signal.amount ? ` · ${signal.amount.value} ${signal.amount.currency}` : " · 金额未公布"}</p>
      <div className="reason-list">{signal.reasons.map((reason) => <span key={reason}>{reason}</span>)}{signal.unknown_count > 0 ? <span className="unknown">未知项 {signal.unknown_count}</span> : null}</div>
      <div className="card-actions">
        {objectId ? <a href={`/objects/${objectId}?signal=${signal.id}`}>查看对象与证据</a> : <span className="muted">对象尚未解析</span>}
        <span className="muted">按策略 v{signal.policy?.version ?? "?"} 判断</span>
      </div>
      {canFeedback ? <div className="feedback-actions" aria-label="信号反馈"><button className="secondary-button" disabled={submitting} onClick={() => submit("relevant")}>相关</button><button className="secondary-button" disabled={submitting} onClick={() => submit("irrelevant")}>不相关并忽略</button></div> : <span className="muted">当前成员没有反馈权限</span>}
      {triage?.ok ? <div className="save-result"><StateBadge tone="good">{triage.data.result === "succeeded" ? "反馈已保存" : "反馈无需变化"}</StateBadge></div> : triage && !triage.ok ? <FailurePage error={triage.error} compact /> : null}
    </article>
  );
}

function SignalDetail({ signal }: { signal: SignalRow }) {
  return <section className="detail-panel"><div className="eyebrow">信号详情</div><h2>{signal.title}</h2><p>{signal.reasons.join("；") || "暂无可显示的命中理由"}</p><div className="metadata">求值时间 {new Date(signal.evaluated_at).toLocaleString("zh-CN")} · 发现 {new Date(signal.created_at).toLocaleString("zh-CN")}</div></section>;
}

function BriefingsPage({ briefingId }: { briefingId?: string }) {
  const [list, setList] = useState<ApiResult<{ briefings: BriefingSummary[] }> | null>(null);
  const [detail, setDetail] = useState<ApiResult<Briefing> | null>(null);
  useEffect(() => { setList(null); void api.listBriefings().then(setList); }, []);
  useEffect(() => { setDetail(null); if (briefingId) void api.getBriefing(briefingId).then(setDetail); }, [briefingId]);
  if (list === null) return <LoadingPage title="读取简报历史" />;
  if (!list.ok) return <FailurePage error={list.error} />;
  return <PageFrame title="今日简报 / 历史" eyebrow="channels"><div className="briefing-list">{list.data.briefings.length === 0 ? <EmptyState title="还没有简报" body="首次组装后，简报会按周期出现在这里。" /> : list.data.briefings.map((briefing) => <BriefingCard key={briefing.id} briefing={briefing} />)}</div>{briefingId && detail?.ok ? <BriefingDetail briefing={detail.data} /> : null}{briefingId && detail && !detail.ok ? <FailurePage error={detail.error} compact /> : null}</PageFrame>;
}

function BriefingCard({ briefing }: { briefing: BriefingSummary }) {
  return <article className="briefing-card"><div><h2>{briefing.source}</h2><p className="muted">{briefing.period.from} – {briefing.period.to}</p></div><div className="card-heading"><StateBadge tone={briefing.partial ? "warn" : "good"}>{briefing.empty ? "空简报" : briefing.partial ? "部分成功" : "已组装"}</StateBadge><a href={`/briefings/${briefing.id}`}>查看</a></div></article>;
}

function BriefingDetail({ briefing }: { briefing: Briefing }) {
  return <section className="detail-panel"><div className="eyebrow">简报详情 · 内容截至 {briefing.content_as_of}</div><h2>{briefing.empty ? "本期没有新信号" : "本期信号"}</h2><div className="section-list">{briefing.sections.map((section, index) => <pre key={index}>{JSON.stringify(section, null, 2)}</pre>)}</div></section>;
}

function ProfilePage() {
  const [result, setResult] = useState<ApiResult<Profile> | null>(null);
  useEffect(() => { setResult(null); void api.getProfile().then(setResult); }, []);
  if (result === null) return <LoadingPage title="读取客户档案" />;
  if (!result.ok) return <FailurePage error={result.error} />;
  const content = result.data.content;
  return <PageFrame title="客户档案" eyebrow={`版本 ${result.data.version}`}><div className="profile-grid"><ProfileCard title="价值链角色" values={content.value_chain_roles} /><ProfileCard title="行业包" values={content.industry_packs} /><ProfileCard title="品类" values={content.offering_categories} /><ProfileCard title="服务区域" values={content.service_regions} /><ProfileCard title="品牌" values={content.brands} /></div><p className="muted">档案读取为只读切片；保存、并发版本检查和批量导入按 W1 继续交付。</p></PageFrame>;
}

function ProfileCard({ title, values }: { title: string; values?: string[] }) {
  return <article className="info-card"><h3>{title}</h3>{values?.length ? <div className="tag-list">{values.map((value) => <StateBadge key={value}>{value}</StateBadge>)}</div> : <span className="muted">暂无记录</span>}</article>;
}

function ObjectivesPage({ objectiveId }: { objectiveId?: string }) {
  const [list, setList] = useState<ApiResult<{ objectives: Objective[] }> | null>(null);
  const [objective, setObjective] = useState<ApiResult<Objective> | null>(null);
  const [policy, setPolicy] = useState<ApiResult<Policy> | null>(null);
  useEffect(() => { setList(null); void api.listObjectives().then(setList); }, []);
  useEffect(() => { setObjective(null); setPolicy(null); if (objectiveId) { void api.getObjective(objectiveId).then(setObjective); void api.getPolicy(objectiveId).then(setPolicy); } }, [objectiveId]);
  if (list === null) return <LoadingPage title="读取目标" />;
  if (!list.ok) return <FailurePage error={list.error} />;
  return <PageFrame title="目标与策略" eyebrow="customer-context"><div className="objectives-layout"><div className="objective-list">{list.data.objectives.length === 0 ? <EmptyState title="还没有目标" body="管理员配置档案后，可以在这里启用 A02 目标。" /> : list.data.objectives.map((item) => <a className={`objective-row ${item.id === objectiveId ? "selected" : ""}`} key={item.id} href={`/settings/objectives/${item.id}`}><div><strong>{item.template_id}</strong><span className="muted">{item.org_unit_id} · {item.owner}</span></div><StateBadge tone={item.state === "active" ? "good" : item.state === "disabled" ? "warn" : "neutral"}>{item.state}</StateBadge></a>)}</div>{objectiveId ? <ObjectiveDetail objective={objective} policy={policy} /> : <EmptyState title="选择一个目标" body="目标详情会显示当前状态、策略版本和信号入口。" />}</div></PageFrame>;
}

function ObjectiveDetail({ objective, policy }: { objective: ApiResult<Objective> | null; policy: ApiResult<Policy> | null }) {
  if (objective === null || policy === null) return <div className="loading">读取目标详情…</div>;
  if (!objective.ok) return <FailurePage error={objective.error} compact />;
  return <section className="detail-panel"><div className="card-heading"><div><div className="eyebrow">{objective.data.template_id}</div><h2>{objective.data.id}</h2></div><StateBadge tone={objective.data.state === "active" ? "good" : "warn"}>{objective.data.state}</StateBadge></div><p>负责人：{objective.data.owner} · 单元：{objective.data.org_unit_id} · 参与人：{objective.data.participants.length} 人</p>{policy.ok ? <><div className="policy-summary"><StateBadge tone={policy.data.active_version > 0 ? "good" : "neutral"}>{policy.data.active_version > 0 ? `生效版本 v${policy.data.active_version}` : "尚未激活"}</StateBadge><span className="muted">草案版本 {policy.data.draft_version} · 代次 {policy.data.generation}</span></div><PolicyEditor objectiveId={objective.data.id} policy={policy.data} /></> : <StatusPanel kind="unavailable" detail="策略读取暂不可用，目标基本信息仍可查看。" />}<a className="primary-button" href={`/signals/${objective.data.id}`}>打开信号收件箱</a></section>;
}

function PolicyEditor({ objectiveId, policy }: { objectiveId: string; policy: Policy }) {
  const initial = policy.draft?.form;
  const [form, setForm] = useState<PolicyForm | null>(initial ?? null);
  const [saving, setSaving] = useState(false);
  const [result, setResult] = useState<ApiResult<Policy> | null>(null);
  const [activation, setActivation] = useState<ApiResult<{ approval_id: string; state: string }> | null>(null);
  const [requesting, setRequesting] = useState(false);
  const [simulationRun, setSimulationRun] = useState<ApiResult<import("@miaoqia/api").SimulationRun> | null>(null);
  useEffect(() => setForm(policy.draft?.form ?? null), [policy.draft?.revision]);
  const embeddedSimulation = policy.draft?.simulation as { simulation_id?: string; status?: string; stale?: boolean; failure_reason?: string } | undefined;
  useEffect(() => {
    const id = embeddedSimulation?.simulation_id;
    if (!id) { setSimulationRun(null); return; }
    let cancelled = false;
    let timer: number | undefined;
    const refresh = () => { void api.getSimulation(id).then((next) => { if (cancelled) return; setSimulationRun(next); if (next.ok && next.data.status !== "completed" && next.data.status !== "failed") timer = window.setTimeout(refresh, 5000); }); };
    refresh();
    return () => { cancelled = true; if (timer) window.clearTimeout(timer); };
  }, [embeddedSimulation?.simulation_id]);
  if (!form) return <div className="empty-state compact-empty"><h3>当前没有可编辑草案</h3><p>首次策略草案由目标模板初始化；正式激活仍需仿真、强认证和审批。</p></div>;
  const updateThreshold = (key: keyof PolicyForm["thresholds"], value: string) => setForm({ ...form, thresholds: { ...form.thresholds, [key]: Number(value) } });
  const effectivePolicy = result?.ok ? result.data : policy;
  const simulation = simulationRun?.ok ? { ...embeddedSimulation, ...simulationRun.data, simulation_id: simulationRun.data.id } : embeddedSimulation;
  const save = () => { setSaving(true); setResult(null); setActivation(null); void api.draftPolicy(objectiveId, effectivePolicy.draft_version, form).then((next) => { setResult(next); setSaving(false); }); };
  const requestActivation = () => { if (!effectivePolicy.draft?.revision || !simulation?.simulation_id) return; setRequesting(true); setActivation(null); void api.requestPolicyActivation(objectiveId, { expected_version: effectivePolicy.draft_version, revision: effectivePolicy.draft.revision, simulation_id: simulation.simulation_id, note: "通过 Web 工作台提交 A02 策略激活申请", ...(effectivePolicy.active_version === 0 ? { backfill_days: 30 } : {}) }).then((next) => { setActivation(next as ApiResult<{ approval_id: string; state: string }>); setRequesting(false); }); };
  return <div className="policy-editor"><div className="eyebrow">策略草案</div><div className="threshold-grid">{(["base_score", "pursue_at", "drop_below"] as const).map((key) => <label key={key}><span>{key}</span><input type="number" min={0} max={1000} value={form.thresholds[key]} onChange={(event) => updateThreshold(key, event.target.value)} /></label>)}</div><div className="editor-actions"><button className="primary-button" disabled={saving} onClick={save}>{saving ? "保存中…" : "保存草案"}</button>{simulation?.status === "completed" && !simulation.stale && effectivePolicy.draft?.valid ? <button className="secondary-button" disabled={requesting} onClick={requestActivation}>{requesting ? "提交中…" : "申请激活"}</button> : null}</div>{simulation ? <div className="simulation-status"><StateBadge tone={simulation.status === "completed" && !simulation.stale ? "good" : simulation.status === "failed" ? "warn" : "neutral"}>{simulation.status === "completed" ? (simulation.stale ? "仿真已过期" : "仿真已完成") : simulation.status === "failed" ? "仿真失败" : "仿真进行中"}</StateBadge>{simulation.status === "failed" ? <span className="muted">{simulation.failure_reason ?? "数据或领域状态暂不可用"}</span> : null}</div> : <div className="muted">保存草案后将自动运行仿真。</div>}{result?.ok ? <div className="save-result"><StateBadge tone="good">已保存修订 r{result.data.draft?.revision ?? "?"}</StateBadge><span className="muted">保存后会重新仿真，旧报告不再直接用于申请。</span></div> : result && !result.ok ? <FailurePage error={result.error} compact /> : null}{activation?.ok ? <div className="save-result"><StateBadge tone="warn">已提交，待审批</StateBadge><a href={`/approvals/${activation.data.approval_id}`}>查看审批 {activation.data.approval_id}</a></div> : activation && !activation.ok ? <>{activation.error.problem.code === "mfa_required" ? <StatusPanel kind="forbidden" detail="提交激活申请需要 15 分钟内的强认证。开发环境可重新登录并附加 mfa=1。" action={<a href={`/auth/login?return_to=${encodeURIComponent(window.location.pathname)}&mfa=1`}>重新强认证</a>} /> : <FailurePage error={activation.error} compact />}</> : null}</div>;
}

function ObjectPageView({ objectId, signalId }: { objectId: string; signalId?: string }) {
  const [page, setPage] = useState<ApiResult<ObjectPage> | null>(null);
  const [section, setSection] = useState<ApiResult<ObjectSection> | null>(null);
  useEffect(() => { setPage(null); void api.getObject(objectId, signalId).then(setPage); }, [objectId, signalId]);
  useEffect(() => { setSection(null); void api.getObjectSection(objectId, "overview", signalId).then(setSection); }, [objectId, signalId]);
  if (page === null) return <LoadingPage title="读取对象页" />;
  if (!page.ok) return <FailurePage error={page.error} />;
  const objectName = String(page.data.object.name ?? page.data.object.id ?? objectId);
  return <PageFrame title={objectName} eyebrow={`对象 ${objectId}`}><div className="object-header"><StateBadge tone="good">当前视角</StateBadge><span className="muted">{page.data.signal_context ? "来自信号 · 可切换到信号判断时" : "公共事实"}</span></div>{page.data.signal_context_unavailable ? <StatusPanel kind="unavailable" detail="信号上下文暂时不可用，公共对象内容仍可查看。" /> : null}<section className="object-sections"><ObjectSectionView section={section} /><div className="object-links">{page.data.sections.map((name) => <span key={name}>{name}</span>)}</div></section><p className="muted">每个字段按来源权利与数据状态展示；未接入或未知不会被渲染为空事实。</p></PageFrame>;
}

function ObjectSectionView({ section }: { section: ApiResult<ObjectSection> | null }) {
  if (section === null) return <LoadingPage title="读取对象分区" compact />;
  if (!section.ok) return <FailurePage error={section.error} compact />;
  if (section.data.status === "empty") return <EmptyState title="暂无对象记录" body="当前分区没有可显示的记录。" />;
  if (section.data.status !== "ok") return <StatusPanel kind="unavailable" detail={`该分区状态：${section.data.status}${section.data.reason_code ? `（${section.data.reason_code}）` : ""}`} />;
  return <div className="section-list">{section.data.items.length === 0 ? <EmptyState title="暂无对象记录" body="当前分区没有可显示的记录。" /> : section.data.items.map((item, index) => <pre key={index}>{JSON.stringify(item, null, 2)}</pre>)}</div>;
}

function EvidencePage({ evidenceId }: { evidenceId: string }) {
  const [result, setResult] = useState<ApiResult<EvidenceView> | null>(null);
  useEffect(() => { setResult(null); void api.getEvidence(evidenceId).then(setResult); }, [evidenceId]);
  if (result === null) return <LoadingPage title="读取证据" />;
  if (!result.ok) return <FailurePage error={result.error} />;
  const evidence = result.data;
  return <PageFrame title="证据" eyebrow={evidenceId}><div className="evidence-meta"><StateBadge tone={evidence.display_level === "none" ? "warn" : "good"}>{evidence.display_level}</StateBadge><span>{evidence.source.title ?? evidence.source.name ?? "来源未命名"}</span></div>{evidence.retracted ? <StatusPanel kind="unavailable" detail="这份原文已经撤回，只保留撤回状态与来源信息。" /> : evidence.display_level === "none" ? <StatusPanel kind="forbidden" detail="当前来源权利不允许展示内容。" /> : <article className="evidence-body"><p>{evidence.snippet ?? evidence.record_value ?? "未提供摘录内容"}</p>{evidence.context ? <pre>{evidence.context}</pre> : null}{evidence.locator ? <div className="muted">定位：{evidence.locator}</div> : null}</article>}</PageFrame>;
}

function ApprovalPage({ approvalId }: { approvalId: string }) {
  const [result, setResult] = useState<ApiResult<import("@miaoqia/api").ApprovalRequest> | null>(null);
  const [decision, setDecision] = useState<ApiResult<import("@miaoqia/api").ApprovalRequest> | null>(null);
  const [reason, setReason] = useState("");
  const [submitting, setSubmitting] = useState(false);
  useEffect(() => { setResult(null); void api.getApproval(approvalId).then(setResult); }, [approvalId]);
  if (result === null) return <LoadingPage title="读取审批" />;
  if (!result.ok) return <FailurePage error={result.error} />;
  const approval = decision?.ok ? decision.data : result.data;
  const decide = (choice: "approve" | "reject") => {
    if (submitting || approval.state !== "pending" || (choice === "reject" && !reason.trim())) return;
    setSubmitting(true);
    setDecision(null);
    void api.decideApproval(approval.id, { expected_version: approval.version, decision: choice, ...(reason.trim() ? { reason: reason.trim() } : {}) }).then((next) => { setDecision(next); setSubmitting(false); if (!next.ok && next.error.problem.code === "mfa_required") window.location.assign(`/auth/login?return_to=${encodeURIComponent(window.location.pathname)}&mfa=1`); });
  };
  return <PageFrame title="审批详情" eyebrow={approval.id}><section className="detail-panel"><div className="card-heading"><div><div className="eyebrow">{approval.type}</div><h2>{approval.resource}</h2></div><StateBadge tone={approval.state === "pending" ? "warn" : approval.state === "approved" ? "good" : "neutral"}>{approval.state}</StateBadge></div><p>{approval.note}</p><div className="metadata">申请人：{approval.applicant} · 提交：{new Date(approval.submitted_at).toLocaleString("zh-CN")} · 到期：{new Date(approval.expires_at).toLocaleString("zh-CN")}</div>{approval.state === "pending" ? <div className="approval-actions"><label><span>驳回原因（驳回必填）</span><textarea value={reason} onChange={(event) => setReason(event.target.value)} maxLength={500} /></label><div className="editor-actions"><button className="primary-button" disabled={submitting} onClick={() => decide("approve")}>{submitting ? "处理中…" : "批准"}</button><button className="secondary-button" disabled={submitting || !reason.trim()} onClick={() => decide("reject")}>驳回</button></div><p className="muted">决定会实时重核批准关系、版本和申请人状态；需要 15 分钟内强认证。</p></div> : approval.decision_reason ? <p className="muted">决定理由：{approval.decision_reason}</p> : null}{decision && !decision.ok ? <FailurePage error={decision.error} compact /> : null}</section></PageFrame>;
}

function LinkLandingPage({ token }: { token: string }) {
  const [result, setResult] = useState<ApiResult<import("@miaoqia/api").BriefingLinkTarget> | null>(null);
  useEffect(() => { setResult(null); void api.openBriefingLink(token).then(setResult); }, [token]);
  useEffect(() => {
    if (!result?.ok) return;
    const target = result.data;
    if (target.kind === "signal") { void api.getSignal(target.id).then((signal) => { if (signal.ok) navigate(`/signals/${encodeURIComponent(signal.data.objective_id)}/s/${encodeURIComponent(target.id)}`); }); return; }
    if (target.kind === "briefing") navigate(`/briefings/${encodeURIComponent(target.id)}`);
    if (target.kind === "approval") navigate(`/approvals/${encodeURIComponent(target.id)}`);
  }, [result]);
  if (result === null) return <LoadingPage title="打开卡片链接" />;
  if (!result.ok) {
    const code = result.error.problem.code;
    if (code === "account_mismatch") return <PageFrame title="账号不匹配"><StatusPanel kind="forbidden" detail="这条链接发给了其他成员。请切换到对应账号后重新打开，当前不会替你切换账号。" /></PageFrame>;
    if (code === "card_link_expired") return <PageFrame title="链接已过期"><StatusPanel kind="unavailable" detail="卡片链接已过期，请从最新简报重新打开。" /></PageFrame>;
    return <FailurePage error={result.error} />;
  }
  return <LoadingPage title="正在跳转" />;
}

function ChannelBindingPage({ token, oauthCode }: { token: string; oauthCode?: string }) {
  const [preview, setPreview] = useState<ApiResult<import("@miaoqia/api").BindingPreview> | null>(null);
  const [confirmation, setConfirmation] = useState<ApiResult<import("@miaoqia/api").BindingConfirmation> | null>(null);
  const [confirming, setConfirming] = useState(false);
  useEffect(() => { setPreview(null); void api.prepareBinding(token, oauthCode).then(setPreview); }, [token, oauthCode]);
  const confirm = () => { if (!preview?.ok || confirming) return; setConfirming(true); void api.confirmBinding(token, preview.data.confirmation, oauthCode).then((next) => { setConfirmation(next); setConfirming(false); }); };
  if (preview === null) return <LoadingPage title="读取绑定确认" />;
  if (!preview.ok) { if (preview.error.problem.code === "channel_browser_required") return <PageFrame title="请在企业微信中打开"><StatusPanel kind="unavailable" detail="绑定链接必须在企业微信内置浏览器中打开，当前页面不会伪造渠道身份。" /></PageFrame>; return <FailurePage error={preview.error} />; }
  if (confirmation?.ok) return <PageFrame title="绑定完成"><section className="detail-panel"><StateBadge tone="good">{confirmation.data.outcome === "already_bound" ? "已完成" : "绑定成功"}</StateBadge><p>{confirmation.data.outcome === "already_bound" ? "这条绑定请求已经完成。" : "企业微信账号已绑定到当前成员。"}</p></section></PageFrame>;
  return <PageFrame title="确认企业微信绑定" eyebrow={preview.data.channel}><section className="detail-panel"><h2>{preview.data.corp_name ?? preview.data.corp_id}</h2><p>{preview.data.display_name ?? preview.data.channel_user_id}</p><p className="muted">将绑定到成员 {preview.data.subject}，确认页有效至 {new Date(preview.data.expires_at).toLocaleString("zh-CN")}。</p><button className="primary-button" disabled={confirming} onClick={confirm}>{confirming ? "绑定中…" : "确认绑定"}</button>{confirmation && !confirmation.ok ? <FailurePage error={confirmation.error} compact /> : null}</section></PageFrame>;
}

function InboxPage() { return <PageFrame title="待我处理" eyebrow="全局入口"><EmptyState title="待办接口将在下一批接入" body="当前 A02 L2 首批先交付信号、对象、证据与简报页面；审批和跟进会沿用同一会话与状态组件。" /></PageFrame>; }
function UnavailablePage() { return <PageFrame title="此模块尚未开放" eyebrow="开放批次"><StatusPanel kind="unavailable" detail="检索、关注、研究与决策按后续批次开放。当前页面不会伪装成已接入。" /></PageFrame>; }
function LoadingPage({ title, compact = false }: { title: string; compact?: boolean }) { return compact ? <div className="loading">{title}…</div> : <PageFrame title={title}><div className="loading">正在读取…</div></PageFrame>; }
function EmptyState({ title, body }: { title: string; body: string }) { return <section className="empty-state"><h2>{title}</h2><p>{body}</p></section>; }
function InfoCard({ title, body }: { title: string; body: string }) { return <article className="info-card"><h3>{title}</h3><p>{body}</p></article>; }
function FailurePage({ error, compact = false }: { error: { kind: "unauthenticated" | "forbidden" | "not_found" | "unavailable" | "unknown" }; compact?: boolean }) { const action = error.kind === "unauthenticated" ? <a href={`/auth/login?return_to=${encodeURIComponent(window.location.pathname)}`}>登录</a> : <button onClick={() => window.location.reload()}>重试</button>; return compact ? <StatusPanel kind={error.kind} action={action} /> : <PageFrame title="无法读取"><StatusPanel kind={error.kind} action={action} /></PageFrame>; }
