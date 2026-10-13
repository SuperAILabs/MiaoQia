/* 由 tools/contracts（mqc generate）从 contracts/schemas 生成，请勿手改。 */

/**
 * 类型化 ULID，前缀登记在 contracts/identifiers.yaml
 */
export type UlidId = string;
/**
 * 十进制字符串，避免浮点误差
 */
export type Decimal = string;
/**
 * 以组织为主体的司法与信用记录：失信被执行人、限制消费、被执行人、行政处罚、裁判文书、信用评价。主体为自然人的记录不进入本信息领域；记录中出现的自然人只进入 persons（个人信息）。
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
export type EvaluationMethod = "lowest_price" | "comprehensive_scoring" | "other";
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
  buyer: OrgRef;
  agency?: OrgRef;
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
  [k: string]: unknown;
};
/**
 * 意向、计划、资格预审、招标、谈判、磋商、询价、单一来源公示、框架协议征集、更正、澄清、终止、候选人公示、中标/成交结果、流标/废标、合同、验收
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
  | "acceptance";
/**
 * 政府采购、依法必须招标的工程建设项目、国企自主采购
 */
export type TradingSystem = "government_procurement" | "construction_bidding" | "soe_procurement";
/**
 * 公开招标、邀请招标、竞争性谈判、竞争性磋商、询价、单一来源、框架协议、比选、直接采购、电商采购、其他
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
  | "other";
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
 * 登记信息的一条变更记录。涉及自然人的变更（法定代表人、高管、自然人股东）只记录“发生了变更”，变更前后内容进入 personal_detail（个人信息）；组织股东变化以结构化方式记录。
 */
export type OrgChange = PublicFactMeta & {
  info_domain: "INF-ORG";
  id: string;
  subject: OrgRef;
  change_type: OrgChangeType;
  change_item_raw?: Text;
  changed_on: Instant;
  before?: Text;
  after?: Text;
  status_after?: OrgStatus;
  capital_before?: Money;
  capital_after?: Money;
  org_shareholders_added?: OrgRef[];
  org_shareholders_removed?: OrgRef[];
  /**
   * 涉及自然人的变更条数
   */
  natural_person_changes?: number;
  personal_detail?: Person[];
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
 * 一家企业在来源中公示的登记信息（照面信息、股东、分支机构），随来源变化产生新版本。个体工商户的经营者、法定代表人、自然人股东均为个人信息。
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
  status: OrgStatus;
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
    | "accepted";
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

export interface AcceptanceSection {
  id: string;
  contract_id?: string;
  accepted_at?: Instant;
  result?: "passed" | "partial" | "failed";
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
  evidence_ref: UlidId;
  method: "rule" | "model" | "manual";
  confidence: number;
  /**
   * 解析器标识与版本
   */
  extractor?: string;
}
export interface AmendmentSection {
  /**
   * @minItems 1
   */
  amends_notice_ids: [string, ...string[]];
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
  consortium_members?: OrgRef[];
  rank?: number;
  is_winner?: boolean;
  price?: Money;
  score?: number;
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
   * 仅 table：按行列出单元格
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
 * 公告中的联系人。个人信息：单独加密存储，不进入检索、策略视图与模型输入（安全与租户第 6 节）
 */
export interface Contact {
  role: "buyer" | "agency" | "other";
  name?: string;
  phone?: string;
  email?: string;
}
export interface ContractSection {
  id: string;
  lot_id?: string;
  supplier: OrgRef;
  amount: Money;
  signed_at?: Instant;
  term_start?: Instant;
  term_end?: Instant;
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
export interface Text {
  value: string;
  prov: Provenance;
}
export interface DateRange {
  from?: Instant;
  to?: Instant;
}
/**
 * 自然人（法定代表人、自然人股东、高管、联系人等）。个人信息：单独加密存储，不进入检索、策略视图与模型输入（docs/03-architecture/06-security-and-tenancy.md 第 6 节）
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
    "registration_end" | "doc_obtain_end" | "clarification_end" | "bid_submission" | "bid_opening" | "publicity_end";
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
 * 证据定位：块序号 + 可选的表格单元 + 字符区间 [char_start, char_end)
 */
export interface Locator {
  block: number;
  row?: number;
  col?: number;
  char_start?: number;
  char_end?: number;
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
  /**
   * 是否接受保函
   */
  bond_guarantee_accepted?: boolean;
  evaluation_method?: EvaluationMethod;
  bid_results?: BidResult[];
}
/**
 * 资格与技术要求
 */
export interface Requirement {
  id: string;
  kind:
    "qualification" | "performance" | "personnel" | "credit" | "authorization" | "consortium" | "technical" | "other";
  /**
   * 归一的资质或要求代码；无法归一时为 null
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
 * 股东：组织股东以组织引用表达；自然人股东进入 person（个人信息）
 */
export interface OrgShareholder {
  kind: "organization" | "natural_person";
  org?: OrgRef;
  person?: Person;
  share_ratio?: Decimal;
  subscribed_capital?: Money;
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
    "open" | "evaluating" | "candidate_published" | "awarded" | "failed" | "terminated" | "contracted" | "accepted";
  candidates?: BidResult[];
  /**
   * 中标或成交人；框架协议可有多个
   */
  winners?: BidResult[];
  award_amount?: Money;
  contract_ids?: string[];
}
