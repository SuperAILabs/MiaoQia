/* 由 tools/contracts（mqc generate）从 contracts/schemas 生成，请勿手改。 */

/**
 * 十进制字符串，避免浮点误差
 */
export type Decimal = string;
/**
 * 以组织为主体的司法与信用记录：失信被执行人、限制消费、被执行人、行政处罚、裁判文书、信用评价。主体为自然人的记录不进入本信息领域；记录中出现的自然人进入 persons。
 */
export type CreditRecord = PublicFactMeta & {
  info_domain: "INF-CRED";
  id: string;
  record_type: "dishonest" | "consumption_limit" | "enforcement" | "penalty" | "judgment" | "credit_rating";
  subject: OrgRef;
  case_or_doc_no: Text;
  authority?: OrgRef;
  filed_on?: Instant;
  published_on?: Instant;
  amount?: Money;
  /**
   * 已履行、已结案、已移出名单或已修复
   */
  resolved: boolean;
  status_raw?: Text;
  resolved_on?: Instant;
  enforcement?: {
    performance?: "not_performed" | "partially_performed" | "performed";
    unperformed_amount?: Money;
  };
  penalty?: {
    penalty_types?: ("warning" | "fine" | "confiscation" | "suspension" | "license_revocation" | "other")[];
    violation?: Text;
    legal_basis?: Text;
  };
  judgment?: {
    /**
     * 主体在本案中的地位
     */
    role: "plaintiff" | "defendant" | "third_party" | "applicant" | "respondent";
    cause?: Text;
    procedure?: "first_instance" | "second_instance" | "retrial" | "enforcement" | "other";
    /**
     * 组织当事人；自然人当事人不记录身份
     */
    counterparties?: OrgRef[];
    outcome?: Text;
  };
  rating?: {
    scheme?: Text;
    level: string;
    valid?: DateRange;
  };
  persons?: Person[];
  [k: string]: unknown;
};
/**
 * 类型化 ULID，前缀登记在 contracts/identifiers.yaml
 */
export type UlidId = string;
export type EvaluationMethod = "lowest_price" | "comprehensive_scoring" | "other";
/**
 * 原始制品稳定对象键/引用，不是短时签名URL
 */
export type EventArtifactRef = string;
/**
 * @minItems 1
 */
export type EventChangedFields = [string, ...string[]];
export type EventEntityId = string;
export type EventFactId = string;
export type EventInfoDomain = string;
export type EventOccurredAt = string;
export type EventQualityStatus = "ok" | "warning" | "suspect";
export type EventRelationshipId = string;
export type EventRevisionRef = string;
/**
 * 有效权利集合；撤回全部权利时允许为空。与事实的溯源rights_tags不同。
 */
export type EventRightsSet = string[];
/**
 * @minItems 1
 */
export type EventRightsTags = [string, ...string[]];
export type EventSha256 = string;
export type EventSnapshotRef = string;
export type EventSourceId = string;
export type EventVersion = number;
/**
 * 投资项目从备案核准到竣工的一条审批或许可记录。跨信息领域的投资项目实体（Project）由 entity-graph 拥有，本记录以 investment_project_id 引用。
 */
export type InvestmentRecord = PublicFactMeta & {
  info_domain: "INF-INVP";
  id: string;
  /**
   * 备案、核准、可研批复、环评批复、用地批复、规划许可、施工许可、竣工验收备案
   */
  record_type:
    | "filing"
    | "approval"
    | "feasibility_approval"
    | "eia_approval"
    | "land_approval"
    | "planning_permit"
    | "construction_permit"
    | "completion_filing";
  project_code?: Text;
  project_name: Text;
  /**
   * entity-graph 的投资项目实体
   */
  investment_project_id?: string;
  /**
   * 关联的采购项目聚合（如施工许可对应的施工招标项目），由聚合规则维护
   */
  procurement_project_id?: string;
  owner?: OrgRef;
  authority?: Text;
  document_no?: Text;
  decided_on: Instant;
  location?: Region;
  total_investment?: Money;
  funding_source?: Text;
  construction_scale?: Text;
  industry?: Category;
  construction_permit?: {
    contractor?: OrgRef;
    designer?: OrgRef;
    supervisor?: OrgRef;
    surveyor?: OrgRef;
    contract_amount?: Money;
    area_sqm?: Decimal;
    planned_period?: DateRange;
  };
  completion?: {
    completed_on?: Instant;
    actual_cost?: Money;
  };
  [k: string]: unknown;
};
export type ProcurementCategory = "goods" | "works" | "services";
/**
 * 一条公告按原文表达的领域事实（随解析修订产生新版本）。跨公告的当前状态见 procurement-project.schema.json。未分包的公告视为一个标包（lot_key = "1"）。
 */
export type Notice = PublicFactMeta & {
  id: string;
  info_domain: "INF-PROC";
  notice_type: NoticeType;
  trading_system?: TradingSystem;
  method?: ProcurementMethod;
  procurement_category?: ProcurementCategory;
  title: Text;
  notice_code?: Text;
  project_code?: Text;
  approval_code?: Text;
  /**
   * 当前归并到的采购项目。权威记录是 domain-intelligence 的 AggregationLink，本字段为其当前指针
   */
  project_id?: string;
  /**
   * 公告中显式引用的前序公告
   */
  related_notice_ids?: string[];
  buyer?: OrgRef;
  agency?: OrgRef;
  project_owner?: OrgRef;
  single_source_supplier?: OrgRef;
  location?: Region;
  funding_source?: Text;
  budget?: Money;
  max_price?: Money;
  /**
   * @minItems 1
   */
  lots: [LotSection, ...LotSection[]];
  amendment?: AmendmentSection;
  contract?: ContractSection;
  acceptance?: AcceptanceSection;
  ending?: EndingSection;
  contacts?: Contact[];
  award_amount?: Money;
  /**
   * 结果主体/标包只在当前不可得附件中，已记录quality警告时为true；不伪造中标人
   */
  result_details_pending?: boolean;
  [k: string]: unknown;
};
/**
 * 意向、计划、资格预审、招标、谈判、磋商、询价、单一来源公示、框架协议征集、更正、澄清、终止、候选人公示、中标/成交结果、流标/废标、合同、验收、最高限价独立公告、供应商名录征集（不等同框架协议）
 */
export type NoticeType =
  | "intent"
  | "plan"
  | "prequalification"
  | "tender"
  | "negotiation"
  | "consultation"
  | "inquiry"
  | "single_source"
  | "framework"
  | "amendment"
  | "clarification"
  | "termination"
  | "candidate"
  | "award"
  | "failure"
  | "contract"
  | "acceptance"
  | "price_limit"
  | "supplier_enrollment";
/**
 * 政府采购、依法必须招标的工程建设项目、国企自主采购
 */
export type TradingSystem = "government_procurement" | "construction_bidding" | "soe_procurement";
/**
 * 公开招标、邀请招标、竞争性谈判、竞争性磋商、询价、单一来源、框架协议、比选、直接采购、电商采购、其他、公开遴选
 */
export type ProcurementMethod =
  | "open_tender"
  | "invited_tender"
  | "negotiation"
  | "consultation"
  | "inquiry"
  | "single_source"
  | "framework"
  | "comparison"
  | "direct"
  | "e_mall"
  | "other"
  | "selection";
/**
 * 列入或移出经营异常名录（含严重违法失信名单）的记录。移出时在同一事实上产生新版本。
 */
export type OrgAbnormal = PublicFactMeta & {
  info_domain: "INF-ORG";
  id: string;
  subject: OrgRef;
  /**
   * 经营异常名录、严重违法失信名单
   */
  list_type: "abnormal_operation" | "serious_violation";
  reason?: Text;
  reason_code?: "no_annual_report" | "unreachable_address" | "false_disclosure" | "non_disclosure" | "other";
  listed_on: Instant;
  decision_authority?: Text;
  removed_on?: Instant;
  removal_reason?: Text;
  [k: string]: unknown;
};
/**
 * 登记信息的一条变更记录。before / after 保留原文变更前后内容（含法定代表人、高管、自然人股东）；组织股东变化另以结构化方式记录。
 */
export type OrgChange = PublicFactMeta & {
  [k: string]: unknown;
} & {
  info_domain: "INF-ORG";
  id: string;
  subject: OrgRef;
  change_type: OrgChangeType;
  change_item_raw?: Text;
  changed_on?: Instant;
  before?: Text;
  after?: Text;
  status_after?: OrgStatus;
  capital_before?: Money;
  capital_after?: Money;
  org_shareholders_added?: OrgRef[];
  org_shareholders_removed?: OrgRef[];
  status_after_prov?: Provenance;
  [k: string]: unknown;
};
export type OrgChangeType =
  | "name"
  | "address"
  | "legal_representative"
  | "executive"
  | "equity"
  | "capital"
  | "scope"
  | "status"
  | "enterprise_type"
  | "other";
/**
 * 存续（在业）、停业、吊销、注销、迁出
 */
export type OrgStatus = "active" | "suspended" | "revoked" | "cancelled" | "moved_out";
/**
 * 有限责任公司、股份有限公司、全民所有制、集体所有制、合伙企业、个人独资企业、外商投资企业、分支机构、个体工商户、事业单位、社会组织、其他
 */
export type OrgEnterpriseType =
  | "limited_liability"
  | "joint_stock"
  | "state_owned"
  | "collective"
  | "partnership"
  | "sole_proprietorship"
  | "foreign_invested"
  | "branch"
  | "individual_business"
  | "public_institution"
  | "social_organization"
  | "other";
/**
 * 一家企业在来源中公示的登记信息（照面信息、股东、分支机构），随来源变化产生新版本。
 */
export type OrgRegistration = PublicFactMeta & {
  info_domain: "INF-ORG";
  id: string;
  subject: OrgRef;
  /**
   * 统一社会信用代码
   */
  uscc?: string;
  name: Text;
  former_names?: Text[];
  enterprise_type?: OrgEnterpriseType;
  enterprise_type_raw?: Text;
  status?: OrgStatus;
  established_on?: Instant;
  business_term?: DateRange;
  registered_capital?: Money;
  paid_in_capital?: Money;
  region?: Region;
  address?: Text;
  industry?: Category;
  business_scope?: Text;
  registration_authority?: Text;
  approved_on?: Instant;
  legal_representative?: Person;
  shareholders?: OrgShareholder[];
  branches?: OrgRef[];
  parent?: OrgRef;
  uscc_prov?: Provenance;
  status_prov?: Provenance;
  legal_representative_prov?: Provenance;
  [k: string]: unknown;
};
/**
 * 同一采购项目的多条公告归并后的当前状态（领域项目聚合）。每个字段的 prov 指向其最新依据所在公告的证据；更正按 amendment.changes 应用后得到当前值。阶段由 packs/information_domains/procurement/lifecycle.yaml 的状态机维护。
 */
export type ProcurementProject = PublicFactMeta & {
  id: string;
  info_domain: "INF-PROC";
  stage:
    | "intent"
    | "announced"
    | "amended"
    | "evaluating"
    | "candidate_published"
    | "awarded"
    | "failed"
    | "terminated"
    | "contracted"
    | "accepted"
    | "shortlisted"
    | "acceptance_failed"
    | "partially_accepted"
    | "acceptance_pending";
  trading_system?: TradingSystem;
  method?: ProcurementMethod;
  procurement_category?: ProcurementCategory;
  title: Text;
  project_code?: Text;
  approval_code?: Text;
  /**
   * 所属投资项目（entity-graph 的 Project 实体）
   */
  investment_project_id?: string;
  buyer: OrgRef;
  agency?: OrgRef;
  project_owner?: OrgRef;
  location?: Region;
  budget?: Money;
  /**
   * 按发布时间排序
   *
   * @minItems 1
   */
  notice_ids: [string, ...string[]];
  first_published_at?: Instant;
  /**
   * @minItems 1
   */
  lots: [ProjectLot, ...ProjectLot[]];
  [k: string]: unknown;
};
/**
 * detection 对一个求值单位按租户策略求值得到的结果（草案；语义见 docs/03-architecture/11-policy-dsl.md 第 4 节与 subsystems/detection.md）。同一策略对同一求值单位（subject 的 type、id 与 part_id）只有一条信号；求值单位状态变化导致的处置变化以 SignalUpdate 追加记录，并更新本对象的当前处置。no_signal 不产生信号。
 */
export type Signal = TenantMeta & {
  id?: string;
  /**
   * 租户启用的商业目标 TenantObjective
   */
  objective_id: string;
  /**
   * 应用领域包中的目标编号，如 APP-TENDER.A01
   */
  objective_code: string;
  policy: PolicyRef;
  subject: SubjectRef;
  disposition: "pursue_candidate" | "review_candidate" | "watch_signal" | "excluded";
  /**
   * 存在按 missing: review 处理的未知硬条件
   */
  needs_review?: boolean;
  /**
   * 同一策略下的排序分，不代表成交概率
   */
  score: number;
  /**
   * excluded 时的原因代码（应用领域包 reason_codes 登记）
   */
  reason_code?: string;
  trace: RuleTrace[];
  /**
   * 特征快照：特征名 → 值；未知的特征不出现，不含其他租户的信息
   */
  features?: {
    [k: string]: unknown;
  };
  view_snapshot_hash: string;
  context_snapshot_version: number;
  /**
   * 逻辑求值时间，即触发事件的时间（表达式中的 now）
   */
  evaluated_at: string;
  trigger_event_id: string;
  /**
   * open 待处理；triaged 已由用户调整处置；converted 已发起研究或推进；dismissed 用户忽略；closed 求值单位进入终态（如截止、终止）后关闭
   */
  status: "open" | "triaged" | "converted" | "dismissed" | "closed";
  /**
   * 已追加的 SignalUpdate 数
   */
  update_count: number;
  [k: string]: unknown;
};
/**
 * 目标对象类型（业务架构第 3 节）
 */
export type SubjectType = "project" | "domain_fact" | "organization" | "market" | "relationship" | "program";

export interface AcceptanceSection {
  id: string;
  contract_id?: string;
  accepted_at?: Instant;
  result?: "passed" | "partial" | "failed";
  contract_code?: Text;
  contract_name?: Text;
  supplier?: OrgRef;
  opinion?: Text;
  result_prov?: Provenance;
  scope?: Text;
  dispute?: Text;
  /**
   * 裁判结论原文段落分别溯源；技术质量、合同解除及付款结论分别保留，不把当事人主张作为法院结论。
   *
   * @minItems 1
   */
  judicial_outcome?: [Text, ...Text[]];
}
export interface Instant {
  /**
   * UTC；精度为日时取原始时区当日零点换算为 UTC
   */
  value: string;
  precision: "year" | "month" | "day" | "minute" | "second";
  tz_raw?: string;
  prov: Provenance;
}
/**
 * 字段溯源：证据位置、提取方法与置信度
 */
export interface Provenance {
  /**
   * 只能引用 evidence 的 ev_ 标识，不接受其他类型标识
   */
  evidence_ref: string;
  method: "rule" | "model" | "manual";
  confidence: number;
  /**
   * 解析器标识与版本
   */
  extractor?: string;
}
export interface Text {
  value: string;
  prov: Provenance;
}
/**
 * 组织引用：保留原文名称，解析结果引用 entity-graph 的组织
 */
export interface OrgRef {
  name_raw: string;
  org_id?: string;
  /**
   * 统一社会信用代码（原文出现时）
   */
  uscc?: string;
  resolution_confidence?: number;
  prov: Provenance;
  address?: Text;
  uscc_prov?: Provenance;
}
export interface AmendmentSection {
  /**
   * 已解析的前序公告标识；暂未归并时允许为空，保留 original_reference 并进入复核，不伪造标识
   */
  amends_notice_ids: string[];
  changes: {
    id: string;
    /**
     * 被更正公告中的字段 JSON Pointer，如 /lots/0/deadlines/1/at；无法定位时为说明性路径并降为人工复核
     */
    field_path: string;
    before?: unknown;
    after?: unknown;
    prov: Provenance;
  }[];
  original_reference?: Text;
  summary?: Text;
}
export interface Attachment {
  name: string;
  snapshot_ref?: string;
  media_type?: string;
}
/**
 * 页面坐标（左上角为原点，单位为点），用于界面高亮
 */
export interface Bbox {
  x: number;
  y: number;
  w: number;
  h: number;
}
/**
 * 开标记录、候选人或中标结果中的一名投标人
 */
export interface BidResult {
  id: string;
  stage: "opening" | "candidate" | "award";
  bidder: OrgRef;
  /**
   * @minItems 1
   */
  consortium_members?: [OrgRef, ...OrgRef[]];
  rank?: number;
  is_winner?: boolean;
  price?: Money;
  score?: number;
  price_rate?: Rate;
  /**
   * 普通中标或框架第一阶段入围；入围不代表第二阶段成交/签约
   */
  result_kind?: "contract_award" | "framework_shortlist";
  consortium_lead?: OrgRef;
  /**
   * 报价金额含义；年单价合计、单价和估算额不得当项目总成交额
   */
  price_kind?: "total" | "unit" | "annual_unit_sum" | "estimated_total";
  price_kind_prov?: Provenance;
}
export interface Money {
  value: Decimal;
  currency: string;
  /**
   * 原文写法，如“120.5 万元”
   */
  raw?: string;
  /**
   * 是否含税；原文未说明时不出现
   */
  tax_included?: boolean;
  prov: Provenance;
}
/**
 * 百分比报价或限价；100 表示 100%，不得写入 money 或换算为项目总价
 */
export interface Rate {
  value: string;
  raw?: string;
  basis?: Text;
  prov: Provenance;
}
/**
 * 文档中的一个块；index 在文档内从 0 连续编号，是定位的主坐标
 */
export interface Block {
  index: number;
  kind: "heading" | "paragraph" | "list_item" | "table";
  /**
   * 仅 heading
   */
  level?: number;
  /**
   * heading、paragraph、list_item 的纯文本；字符区间以 Unicode 码点计
   */
  text?: string;
  /**
   * 仅 table：完整网格，每行列数相同；合并单元格的覆盖位置以 covered_by 指向起始单元格
   */
  rows?: Cell[][];
  /**
   * 分页文档的页码
   */
  page?: number;
  bbox?: Bbox;
}
export interface Cell {
  text: string;
  row_span?: number;
  col_span?: number;
  header?: boolean;
  /**
   * 被合并单元格覆盖的位置：表格按完整网格存放，覆盖位置的 text 为空并指向起始单元格
   */
  covered_by?: {
    row: number;
    col: number;
  };
}
export interface Category {
  /**
   * 分类体系，如 gov-procurement-catalog、industry:building-materials
   */
  scheme: string;
  scheme_version: string;
  code: string;
  name_raw?: string;
  confidence?: number;
}
/**
 * 公开业务联系人，可明文检索与进入模型；姓名、电话、邮箱分别带证据定位，未知字段省略
 */
export interface Contact {
  role: "buyer" | "agency" | "other" | "project_owner" | "technical_support" | "supervisor";
  name?: string;
  phone?: string;
  email?: string;
  organization?: OrgRef;
  name_prov?: Provenance;
  phone_prov?: Provenance;
  email_prov?: Provenance;
}
export interface ContractSection {
  id: string;
  lot_id?: string;
  supplier: OrgRef;
  amount: Money;
  signed_at?: Instant;
  term_start?: Instant;
  term_end?: Instant;
  code?: Text;
  name?: Text;
  /**
   * 原文明确金额语义时填写；最高合同金额不得当作实际成交额
   */
  amount_kind?: "total" | "ceiling" | "estimated";
  conditions?: ProcurementConditions;
  amount_kind_prov?: Provenance;
}
/**
 * 公告与项目标包共用的带证据采购条件；只记录已确认的原文，不把模板未选项当成事实
 */
export interface ProcurementConditions {
  scope?: Text;
  delivery_period?: Text;
  delivery_location?: Text;
  payment_terms?: Text;
  document_access?: Text;
  submission_place?: Text;
  opening_place?: Text;
  consortium_terms?: Text;
  sme_policy?: Text;
  imported_products?: Text;
  agent_participation?: Text;
  contract_pricing?: Text;
  planned_publication?: Text;
}
/**
 * 公共域领域事实的通用字段（数据架构第 5、7 节）
 */
export interface PublicFactMeta {
  id: UlidId;
  schema_version: string;
  /**
   * 领域事实版本号
   */
  version: number;
  info_domain: string;
  snapshot_ref: string;
  revision_ref: string;
  /**
   * @minItems 1
   */
  rights_tags: [string, ...string[]];
  classification: "public" | "internal" | "sensitive" | "personal";
  published_at?: Instant;
  observed_at: string;
  recorded_at: string;
  quality?: Quality;
  [k: string]: unknown;
}
export interface Quality {
  status: "ok" | "warning" | "suspect";
  warnings?: {
    code: string;
    /**
     * JSON Pointer
     */
    field_path?: string;
    message?: string;
  }[];
}
export interface DateRange {
  from?: Instant;
  to?: Instant;
}
/**
 * 自然人（法定代表人、自然人股东、高管、联系人等）。公开来源的个人业务信息按公开数据处理
 */
export interface Person {
  name: string;
  /**
   * 原文职务，如 董事长、执行董事、经理
   */
  role?: string;
}
export interface Deadline {
  id: string;
  type:
    | "registration_end"
    | "doc_obtain_end"
    | "clarification_end"
    | "bid_submission"
    | "bid_opening"
    | "publicity_end"
    | "registration_start"
    | "doc_obtain_start"
    | "bid_bond_end"
    | "site_visit"
    | "clarification_meeting";
  at: Instant;
}
/**
 * 终止、流标或废标
 */
export interface EndingSection {
  ending_type: "terminated" | "failed_bid" | "voided";
  /**
   * 涉及的标包；为空表示全部
   */
  lot_keys?: string[];
  reason?: Text;
}
/**
 * 妙洽CloudEvents 1.0结构化JSON配置；时间和对象载荷为本平台必填；未知信封字段拒绝，空域字段省略。
 */
export interface EventEnvelope {
  specversion: "1.0";
  id: string;
  source: string;
  type: string;
  time: string;
  subject?: string;
  datacontenttype: "application/json";
  dataschema?: string;
  miaoqiazone: "public" | "tenant" | "platform";
  /**
   * 已验证的租户标识；新分配使用登记的tnt_ULID，不能从业务参数派生。
   */
  miaoqiatenant?: string;
  miaoqiacell?: string;
  traceparent?: string;
  /**
   * 由对应AsyncAPI消息Schema校验；信封Schema仅要求对象。
   */
  data: {
    [k: string]: unknown;
  };
}
/**
 * 可被引用的证据片段：指向某个快照在某个处理管线版本下的规范化文档中的一个位置。由 evidence 分配 ev_ 标识；字段溯源（provenance.evidence_ref）与研究报告引用都指向它。
 */
export interface EvidenceRef {
  id: string;
  snapshot_ref: string;
  pipeline_version: string;
  locator: Locator;
  /**
   * 被引用原文片段的哈希，用于校验定位未漂移
   */
  quote_hash: string;
}
/**
 * 证据定位：块序号 + 可选的表格单元 + 字符区间 [char_start, char_end)；跨多个连续块时用 block_end，原文为各块文本直接拼接后的区间（如分两行排版的标题）
 */
export interface Locator {
  block: number;
  row?: number;
  col?: number;
  char_start?: number;
  char_end?: number;
  /**
   * 跨块定位的最后一个块；char_end 相对该块
   */
  block_end?: number;
}
export interface Region {
  /**
   * GB/T 2260 行政区划代码
   */
  code: string;
  name_raw?: string;
  prov: Provenance;
}
/**
 * 采购标的
 */
export interface Item {
  id: string;
  name: Text;
  categories?: Category[];
  spec?: Text;
  quantity?: Quantity;
  brand_required?: Text;
  model_required?: Text;
  unit_price?: Money;
}
export interface Quantity {
  value: Decimal;
  /**
   * 归一单位
   */
  unit?: string;
  unit_raw?: string;
  prov: Provenance;
}
/**
 * 本公告中出现的一个标包
 */
export interface LotSection {
  /**
   * 原文标包号；未分包为 "1"
   */
  lot_key: string;
  /**
   * 归并到的项目标包
   */
  lot_id?: string;
  name?: Text;
  procurement_category?: ProcurementCategory;
  items?: Item[];
  requirements?: Requirement[];
  deadlines?: Deadline[];
  budget?: Money;
  max_price?: Money;
  consortium_allowed?: boolean;
  /**
   * 是否要求原厂授权
   */
  authorization_required?: boolean;
  bid_bond?: Money;
  document_fee?: Money;
  /**
   * 是否接受保函
   */
  bond_guarantee_accepted?: boolean;
  evaluation_method?: EvaluationMethod;
  bid_results?: BidResult[];
  conditions?: ProcurementConditions;
  max_price_rate?: Rate;
  project_name?: Text;
  project_code?: Text;
  project_owner?: OrgRef;
  estimated_contract_amount?: Money;
  ending?: EndingSection;
}
/**
 * 资格与技术要求
 */
export interface Requirement {
  id: string;
  kind:
    | "qualification"
    | "performance"
    | "personnel"
    | "financial"
    | "quality"
    | "credit"
    | "authorization"
    | "consortium"
    | "technical"
    | "other";
  /**
   * 归一的资质或要求代码；无法归一时省略，不使用 null
   */
  code?: string;
  text: Text;
}
/**
 * evidence 把原始制品（HTML、PDF、Office、扫描件）转换后的统一文档表示，存放于对象存储（数据分层 L1，按 快照 × 处理管线版本 存放）。它是证据定位的坐标系：解析器只读取规范化文档，字段溯源以块序号与字符区间定位。
 */
export interface NormalizedDocument {
  snapshot_ref: string;
  /**
   * 文档处理管线标识与版本
   */
  pipeline_version: string;
  media_type: "text_html" | "application_pdf" | "office_document" | "image_scan" | "plain_text";
  source_url?: string;
  /**
   * 来源页面标题（未经解析器确认）
   */
  title?: string;
  language?: string;
  blocks: Block[];
  /**
   * 附件：各自是独立快照与独立规范化文档
   */
  attachments?: Attachment[];
}
/**
 * 股东：组织股东以组织引用表达，自然人股东以 person 表达
 */
export interface OrgShareholder {
  kind: "organization" | "natural_person";
  org?: OrgRef;
  person?: Person;
  /**
   * 0—1持股比例，原文百分数除以100
   */
  share_ratio?: string;
  subscribed_capital?: Money;
  share_ratio_prov?: Provenance;
}
/**
 * 产生结果的策略版本：租户策略及其所基于的应用领域包模板
 */
export interface PolicyRef {
  policy_id: string;
  policy_version: number;
  template_id: string;
  template_version: string;
}
export interface ProjectLot {
  id: string;
  lot_key: string;
  name?: Text;
  procurement_category?: ProcurementCategory;
  items?: Item[];
  requirements?: Requirement[];
  /**
   * 应用全部更正后的当前时间节点
   */
  deadlines?: Deadline[];
  budget?: Money;
  max_price?: Money;
  consortium_allowed?: boolean;
  authorization_required?: boolean;
  status?:
    | "open"
    | "evaluating"
    | "candidate_published"
    | "awarded"
    | "failed"
    | "terminated"
    | "contracted"
    | "accepted"
    | "shortlisted"
    | "acceptance_failed"
    | "partially_accepted"
    | "acceptance_pending";
  candidates?: BidResult[];
  /**
   * 实际中标/成交供应商，可多家；框架第一阶段入围另存shortlisted_suppliers
   */
  winners?: BidResult[];
  award_amount?: Money;
  contract_ids?: string[];
  conditions?: ProcurementConditions;
  max_price_rate?: Rate;
  project_name?: Text;
  project_code?: Text;
  project_owner?: OrgRef;
  estimated_contract_amount?: Money;
  ending?: EndingSection;
  shortlisted_suppliers?: BidResult[];
}
/**
 * 内部可信边界的请求上下文；Base64URL无填充编码JSON；不接受外部请求提供的此头。
 */
export interface RequestContext {
  principal: {
    id: string;
    kind: "user" | "service" | "service_account";
  };
  /**
   * 已验证的租户标识；新分配使用登记的tnt_ULID，不能从业务参数派生。
   */
  tenant?: string;
  cell?: string;
  purpose?: string;
}
export interface RuleTrace {
  id: string;
  clause: "require" | "exclude" | "watch" | "boost" | "penalize";
  result: "true" | "false" | "unknown";
  /**
   * 导致未知的字段（缺失或证据不达标）
   */
  unknown_fields?: string[];
  /**
   * 渲染后的业务说明
   */
  say?: string;
}
/**
 * 租户域对象的通用字段
 */
export interface TenantMeta {
  id: UlidId;
  tenant_id: string;
  schema_version: string;
  created_at: string;
  updated_at: string;
  [k: string]: unknown;
}
/**
 * 目标对象引用 {type, id, version}。id 前缀须与 type 匹配：project 为实体图谱 prj_（跨公告、跨信息领域的业务项目）；domain_fact 为领域事实标识（如 pp_ 采购项目、ntc_ 公告、crd_ 信用记录）；organization 为 org_；market 为 mkt_；relationship 为 rel_；program 为 prg_。version 为被引用对象在求值或判断时的版本，domain_fact 必填；实体图谱对象的版本语义尚未定义，暂为可选。part_id 指向对象内部的求值单位（如采购项目的标包 lot_）。
 */
export interface SubjectRef {
  type: SubjectType;
  id: UlidId;
  version?: number;
  part_id?: UlidId;
}
