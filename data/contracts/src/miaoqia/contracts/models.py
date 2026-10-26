# 由 tools/contracts（mqc generate）从 contracts/schemas 生成，请勿手改。

from __future__ import annotations

from enum import Enum
from typing import Any, Literal

from pydantic import (
    AwareDatetime,
    BaseModel,
    ConfigDict,
    Field,
    RootModel,
    confloat,
    conint,
    constr,
)



class Result(Enum):
    passed = "passed"
    partial = "partial"
    failed = "failed"


class Attachment(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    name: str
    snapshot_ref: constr(pattern=r"^snp_[0-9A-HJKMNP-TV-Z]{26}$") | None = None
    media_type: str | None = None


class Bbox(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    x: float
    y: float
    w: confloat(ge=0.0)
    h: confloat(ge=0.0)


class Stage(Enum):
    opening = "opening"
    candidate = "candidate"
    award = "award"


class ResultKind(Enum):
    contract_award = "contract_award"
    framework_shortlist = "framework_shortlist"


class PriceKind(Enum):
    total = "total"
    unit = "unit"
    annual_unit_sum = "annual_unit_sum"
    estimated_total = "estimated_total"


class Kind(Enum):
    heading = "heading"
    paragraph = "paragraph"
    list_item = "list_item"
    table = "table"


class Category(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    scheme: str = Field(
        ...,
        description="分类体系，如 gov-procurement-catalog、industry:building-materials",
    )
    scheme_version: str
    code: str
    name_raw: str | None = None
    confidence: confloat(ge=0.0, le=1.0) | None = None


class CoveredBy(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    row: conint(ge=0)
    col: conint(ge=0)


class Cell(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    text: str
    row_span: conint(ge=1) | None = None
    col_span: conint(ge=1) | None = None
    header: bool | None = None
    covered_by: CoveredBy | None = Field(
        None,
        description="被合并单元格覆盖的位置：表格按完整网格存放，覆盖位置的 text 为空并指向起始单元格",
    )


class Kind1(Enum):
    previous_period = "previous_period"
    same_period_last_year = "same_period_last_year"
    custom = "custom"


class Role(Enum):
    buyer = "buyer"
    agency = "agency"
    other = "other"
    project_owner = "project_owner"
    technical_support = "technical_support"
    supervisor = "supervisor"


class OrgId(RootModel[constr(pattern=r"^org_[0-9A-HJKMNP-TV-Z]{26}$")]):
    root: constr(pattern=r"^org_[0-9A-HJKMNP-TV-Z]{26}$")


class OfferingCategory(RootModel[constr(min_length=1)]):
    root: constr(min_length=1)


class ServiceRegion(RootModel[constr(pattern=r"^[0-9]{6}$")]):
    root: constr(pattern=r"^[0-9]{6}$")


class Brand(RootModel[constr(min_length=1)]):
    root: constr(min_length=1)


class UnitChainItem(RootModel[constr(min_length=1)]):
    root: constr(min_length=1)


class AmountKind(Enum):
    total = "total"
    ceiling = "ceiling"
    estimated = "estimated"


class RecordType(Enum):
    dishonest = "dishonest"
    consumption_limit = "consumption_limit"
    enforcement = "enforcement"
    penalty = "penalty"
    judgment = "judgment"
    credit_rating = "credit_rating"


class Performance(Enum):
    not_performed = "not_performed"
    partially_performed = "partially_performed"
    performed = "performed"


class PenaltyType(Enum):
    warning = "warning"
    fine = "fine"
    confiscation = "confiscation"
    suspension = "suspension"
    license_revocation = "license_revocation"
    other = "other"


class Role1(Enum):
    plaintiff = "plaintiff"
    defendant = "defendant"
    third_party = "third_party"
    applicant = "applicant"
    respondent = "respondent"


class Procedure(Enum):
    first_instance = "first_instance"
    second_instance = "second_instance"
    retrial = "retrial"
    enforcement = "enforcement"
    other = "other"


class Type(Enum):
    registration_end = "registration_end"
    doc_obtain_end = "doc_obtain_end"
    clarification_end = "clarification_end"
    bid_submission = "bid_submission"
    bid_opening = "bid_opening"
    publicity_end = "publicity_end"
    registration_start = "registration_start"
    doc_obtain_start = "doc_obtain_start"
    bid_bond_end = "bid_bond_end"
    site_visit = "site_visit"
    clarification_meeting = "clarification_meeting"


class Decimal(RootModel[constr(pattern=r"^-?[0-9]+(\.[0-9]+)?$")]):
    root: constr(pattern=r"^-?[0-9]+(\.[0-9]+)?$") = Field(
        ..., description="十进制字符串，避免浮点误差"
    )


class EndingType(Enum):
    terminated = "terminated"
    failed_bid = "failed_bid"
    voided = "voided"


class QualificationsPendingItem(RootModel[constr(min_length=1)]):
    root: constr(min_length=1)


class EvaluationMethod(Enum):
    lowest_price = "lowest_price"
    comprehensive_scoring = "comprehensive_scoring"
    other = "other"


class EventArtifactRef(RootModel[constr(min_length=1)]):
    root: constr(min_length=1) = Field(
        ..., description="原始制品稳定对象键/引用，不是短时签名URL"
    )


class EventChangedField(RootModel[constr(pattern=r"^(/([^~]|~[01])*)+$")]):
    root: constr(pattern=r"^(/([^~]|~[01])*)+$")


class EventChangedFields(RootModel[list[EventChangedField]]):
    root: list[EventChangedField] = Field(..., min_length=1)


class EventEntityId(
    RootModel[
        constr(pattern=r"^org_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^prj_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^loc_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^cat_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^brd_[0-9A-HJKMNP-TV-Z]{26}$")
    ]
):
    root: (
        constr(pattern=r"^org_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^prj_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^loc_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^cat_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^brd_[0-9A-HJKMNP-TV-Z]{26}$")
    )


class Miaoqiazone(Enum):
    public = "public"
    tenant = "tenant"
    platform = "platform"


class EventEnvelope(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    specversion: Literal["1.0"]
    id: constr(pattern=r"^evt_[0-9A-HJKMNP-TV-Z]{26}$")
    source: constr(pattern=r"^miaoqia/[a-z][a-z0-9_-]*$")
    type: constr(pattern=r"^[a-z_]+\.[a-z_]+\.[a-z_]+\.v[1-9][0-9]*$")
    time: AwareDatetime
    subject: constr(min_length=1) | None = None
    datacontenttype: Literal["application/json"]
    dataschema: constr(min_length=1) | None = None
    miaoqiazone: Miaoqiazone
    miaoqiatenant: constr(min_length=1) | None = Field(
        None,
        description="已验证的租户标识；新分配使用登记的tnt_ULID，不能从业务参数派生。",
    )
    miaoqiacell: constr(pattern=r"^[a-z0-9][a-z0-9-]{0,62}$") | None = None
    traceparent: constr(pattern=r"^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$") | None = None
    data: dict[str, Any] = Field(
        ..., description="由对应AsyncAPI消息Schema校验；信封Schema仅要求对象。"
    )


class EventFactId(
    RootModel[
        constr(pattern=r"^ntc_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^pp_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^ogr_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^ogc_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^oga_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^ivr_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^crd_[0-9A-HJKMNP-TV-Z]{26}$")
    ]
):
    root: (
        constr(pattern=r"^ntc_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^pp_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^ogr_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^ogc_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^oga_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^ivr_[0-9A-HJKMNP-TV-Z]{26}$")
        | constr(pattern=r"^crd_[0-9A-HJKMNP-TV-Z]{26}$")
    )


class EventInfoDomain(RootModel[constr(pattern=r"^INF-[A-Z]+$")]):
    root: constr(pattern=r"^INF-[A-Z]+$")


class EventOccurredAt(RootModel[AwareDatetime]):
    root: AwareDatetime


class EventQualityStatus(Enum):
    ok = "ok"
    warning = "warning"
    suspect = "suspect"


class EventRelationshipId(RootModel[constr(pattern=r"^rel_[0-9A-HJKMNP-TV-Z]{26}$")]):
    root: constr(pattern=r"^rel_[0-9A-HJKMNP-TV-Z]{26}$")


class EventRevisionRef(RootModel[constr(pattern=r"^exr_[0-9A-HJKMNP-TV-Z]{26}$")]):
    root: constr(pattern=r"^exr_[0-9A-HJKMNP-TV-Z]{26}$")


class EventRightsSetItem(RootModel[constr(min_length=1)]):
    root: constr(min_length=1)


class EventRightsSet(RootModel[list[EventRightsSetItem]]):
    root: list[EventRightsSetItem] = Field(
        ...,
        description="有效权利集合；撤回全部权利时允许为空。与事实的溯源rights_tags不同。",
    )


class EventRightsTag(RootModel[constr(min_length=1)]):
    root: constr(min_length=1)


class EventRightsTags(RootModel[list[EventRightsTag]]):
    root: list[EventRightsTag] = Field(..., min_length=1)


class EventSha256(RootModel[constr(pattern=r"^sha256:[0-9a-f]{64}$")]):
    root: constr(pattern=r"^sha256:[0-9a-f]{64}$")


class EventSnapshotRef(RootModel[constr(pattern=r"^snp_[0-9A-HJKMNP-TV-Z]{26}$")]):
    root: constr(pattern=r"^snp_[0-9A-HJKMNP-TV-Z]{26}$")


class EventSourceId(RootModel[constr(min_length=1)]):
    root: constr(min_length=1)


class EventVersion(RootModel[conint(ge=1)]):
    root: conint(ge=1)


class Precision(Enum):
    year = "year"
    month = "month"
    day = "day"
    minute = "minute"
    second = "second"


class RecordType1(Enum):
    filing = "filing"
    approval = "approval"
    feasibility_approval = "feasibility_approval"
    eia_approval = "eia_approval"
    land_approval = "land_approval"
    planning_permit = "planning_permit"
    construction_permit = "construction_permit"
    completion_filing = "completion_filing"


class Locator(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    block: conint(ge=0)
    row: conint(ge=0) | None = None
    col: conint(ge=0) | None = None
    char_start: conint(ge=0) | None = None
    char_end: conint(ge=0) | None = None
    block_end: conint(ge=0) | None = Field(
        None, description="跨块定位的最后一个块；char_end 相对该块"
    )


class MediaType(Enum):
    text_html = "text_html"
    application_pdf = "application_pdf"
    office_document = "office_document"
    image_scan = "image_scan"
    plain_text = "plain_text"


class NoticeType(Enum):
    intent = "intent"
    plan = "plan"
    prequalification = "prequalification"
    tender = "tender"
    negotiation = "negotiation"
    consultation = "consultation"
    inquiry = "inquiry"
    single_source = "single_source"
    framework = "framework"
    amendment = "amendment"
    clarification = "clarification"
    termination = "termination"
    candidate = "candidate"
    award = "award"
    failure = "failure"
    contract = "contract"
    acceptance = "acceptance"
    price_limit = "price_limit"
    supplier_enrollment = "supplier_enrollment"


class ListType(Enum):
    abnormal_operation = "abnormal_operation"
    serious_violation = "serious_violation"


class ReasonCode(Enum):
    no_annual_report = "no_annual_report"
    unreachable_address = "unreachable_address"
    false_disclosure = "false_disclosure"
    non_disclosure = "non_disclosure"
    other = "other"


class OrgChangeType(Enum):
    name = "name"
    address = "address"
    legal_representative = "legal_representative"
    executive = "executive"
    equity = "equity"
    capital = "capital"
    scope = "scope"
    status = "status"
    enterprise_type = "enterprise_type"
    other = "other"


class OrgEnterpriseType(Enum):
    limited_liability = "limited_liability"
    joint_stock = "joint_stock"
    state_owned = "state_owned"
    collective = "collective"
    partnership = "partnership"
    sole_proprietorship = "sole_proprietorship"
    foreign_invested = "foreign_invested"
    branch = "branch"
    individual_business = "individual_business"
    public_institution = "public_institution"
    social_organization = "social_organization"
    other = "other"


class Kind2(Enum):
    organization = "organization"
    natural_person = "natural_person"


class OrgStatus(Enum):
    active = "active"
    suspended = "suspended"
    revoked = "revoked"
    cancelled = "cancelled"
    moved_out = "moved_out"


class Granularity(Enum):
    day = "day"
    week = "week"
    month = "month"
    quarter = "quarter"
    year = "year"
    custom = "custom"


class Period(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    start: AwareDatetime
    end: AwareDatetime
    granularity: Granularity | None = None
    time_basis: constr(pattern=r"^[a-z][a-z0-9_.]*$") = Field(
        ...,
        description="归入期间所依据的事实时间字段（例如结果公告的 published_at、合同的 signed_on），由统计口径声明；不用记录或采集时间代替",
    )
    time_zone: str | None = "Asia/Shanghai"


class Person(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    name: constr(min_length=1)
    role: str | None = Field(None, description="原文职务，如 董事长、执行董事、经理")


class PolicyRef(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    policy_id: constr(pattern=r"^pol_[0-9A-HJKMNP-TV-Z]{26}$")
    policy_version: conint(ge=1)
    template_id: constr(pattern=r"^TPL-[A-Z0-9]+(-[A-Z0-9]+)+$")
    template_version: constr(pattern=r"^[0-9]+\.[0-9]+\.[0-9]+$")


class ProcurementCategory(Enum):
    goods = "goods"
    works = "works"
    services = "services"


class ProcurementMethod(Enum):
    open_tender = "open_tender"
    invited_tender = "invited_tender"
    negotiation = "negotiation"
    consultation = "consultation"
    inquiry = "inquiry"
    single_source = "single_source"
    framework = "framework"
    comparison = "comparison"
    direct = "direct"
    e_mall = "e_mall"
    other = "other"
    selection = "selection"


class Stage1(Enum):
    intent = "intent"
    announced = "announced"
    amended = "amended"
    evaluating = "evaluating"
    candidate_published = "candidate_published"
    awarded = "awarded"
    failed = "failed"
    terminated = "terminated"
    contracted = "contracted"
    accepted = "accepted"
    shortlisted = "shortlisted"
    acceptance_failed = "acceptance_failed"
    partially_accepted = "partially_accepted"
    acceptance_pending = "acceptance_pending"


class NoticeId(RootModel[constr(pattern=r"^ntc_[0-9A-HJKMNP-TV-Z]{26}$")]):
    root: constr(pattern=r"^ntc_[0-9A-HJKMNP-TV-Z]{26}$")


class Status(Enum):
    open = "open"
    evaluating = "evaluating"
    candidate_published = "candidate_published"
    awarded = "awarded"
    failed = "failed"
    terminated = "terminated"
    contracted = "contracted"
    accepted = "accepted"
    shortlisted = "shortlisted"
    acceptance_failed = "acceptance_failed"
    partially_accepted = "partially_accepted"
    acceptance_pending = "acceptance_pending"


class Method(Enum):
    rule = "rule"
    model = "model"
    manual = "manual"


class Provenance(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    evidence_ref: constr(pattern=r"^ev_[0-9A-HJKMNP-TV-Z]{26}$") = Field(
        ..., description="只能引用 evidence 的 ev_ 标识，不接受其他类型标识"
    )
    method: Method
    confidence: confloat(ge=0.0, le=1.0)
    extractor: constr(pattern=r"^[a-z0-9_.-]+@[0-9]+\.[0-9]+\.[0-9]+$") | None = Field(
        None, description="解析器标识与版本"
    )


class Classification(Enum):
    public = "public"
    internal = "internal"
    sensitive = "sensitive"
    personal = "personal"


class QualificationStatus(Enum):
    valid = "valid"
    expired = "expired"


class Status1(Enum):
    ok = "ok"
    warning = "warning"
    suspect = "suspect"


class Warning(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    code: str
    field_path: str | None = Field(None, description="JSON Pointer")
    message: str | None = None


class Quality(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    status: Status1
    warnings: list[Warning] | None = None


class Quantity(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    value: Decimal
    unit: str | None = Field(None, description="归一单位")
    unit_raw: str | None = None
    prov: Provenance


class RecordEvidenceRef(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^ev_[0-9A-HJKMNP-TV-Z]{26}$")
    kind: Literal["record"] = Field(
        ..., description="证据形态；文档证据（evidence-ref 1.0）不带本字段"
    )
    snapshot_ref: constr(pattern=r"^snp_[0-9A-HJKMNP-TV-Z]{26}$")
    pipeline_version: constr(pattern=r"^[a-z0-9_.-]+@[0-9]+\.[0-9]+\.[0-9]+$")
    record_key: constr(min_length=1) = Field(
        ..., description="structured-record 中记录的 key"
    )
    field_path: constr(pattern=r"^(/([^/~]|~[01])*)*$") = Field(
        ..., description="记录 data 内的 JSON Pointer（RFC 6901）；空串表示整条记录"
    )
    value_hash: constr(pattern=r"^sha256:[0-9a-f]{64}$") = Field(
        ..., description="被引用取值按 RFC 8785 规范化后的哈希，用于校验定位未漂移"
    )


class RecordValue(RootModel["str | bool | list[RecordValue | None] | dict[str, RecordValue]"]):
    root: str | bool | list[RecordValue | None] | dict[str, RecordValue] = Field(
        ...,
        description="记录取值的无损表示：数字一律保留来源的十进制原文为字符串（不经浮点，避免 2^53 以上整数和小数在各语言中改值），文本为字符串，布尔保留布尔，数组与对象逐层同样表示；来源的 null 在对象中以省略该字段表示（字段缺失即未知），在数组中保留为 null 以保持元素位置（不删除元素）。CSV、XML 的取值本来就是文本。value_hash 按此表示做 RFC 8785 规范化后计算，各语言一致",
    )


class ReferenceDataVersion(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    name: constr(pattern=r"^[a-z][a-z0-9-]*(:[a-z0-9-]+)*$") = Field(
        ..., description="参考数据标识，例如 gb2260、cn-holidays"
    )
    version: constr(min_length=1) = Field(
        ..., description="必须对应不可变且可取得的内容；同一版本不得重新发布为不同内容"
    )


class Region(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    code: constr(pattern=r"^[0-9]{6}$") = Field(
        ..., description="GB/T 2260 行政区划代码"
    )
    name_raw: str | None = None
    prov: Provenance
    scheme_version: constr(min_length=1) | None = Field(
        None,
        description="GB/T 2260 代码表版本（如民政部发布年份 2024）；缺失表示按观察时有效的代码表、版本未知。历史统计保留原代码与版本，跨版本换算是派生数据，另记换算表版本与依据，不改写原值",
    )


class Kind3(Enum):
    user = "user"
    service = "service"
    service_account = "service_account"


class Principal(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(min_length=1)
    kind: Kind3


class RequestContext(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    principal: Principal
    tenant: constr(min_length=1) | None = Field(
        None,
        description="已验证的租户标识；新分配使用登记的tnt_ULID，不能从业务参数派生。",
    )
    cell: constr(pattern=r"^[a-z0-9][a-z0-9-]{0,62}$") | None = None
    purpose: constr(min_length=1) | None = None


class Kind4(Enum):
    qualification = "qualification"
    performance = "performance"
    personnel = "personnel"
    financial = "financial"
    quality = "quality"
    credit = "credit"
    authorization = "authorization"
    consortium = "consortium"
    technical = "technical"
    other = "other"


class Clause(Enum):
    require = "require"
    exclude = "exclude"
    watch = "watch"
    boost = "boost"
    penalize = "penalize"


class Result1(Enum):
    true = "true"
    false = "false"
    unknown = "unknown"


class RuleTrace(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^[RXWBP]-[A-Z0-9-]+$")
    clause: Clause
    result: Result1
    unknown_fields: list[str] | None = Field(
        None, description="导致未知的字段（缺失或证据不达标）"
    )
    say: str | None = Field(None, description="渲染后的业务说明")


class Disposition(Enum):
    pursue_candidate = "pursue_candidate"
    review_candidate = "review_candidate"
    watch_signal = "watch_signal"
    excluded = "excluded"


class Status2(Enum):
    open = "open"
    triaged = "triaged"
    converted = "converted"
    dismissed = "dismissed"
    closed = "closed"


class StructuredRecord(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    key: constr(min_length=1) = Field(
        ...,
        description="记录键：优先取来源自身的主键；来源没有主键时由处理管线按 key_rule 生成，不用行号（行号随排序变化）",
    )
    data: dict[str, RecordValue] = Field(
        ...,
        description="记录内容，保留来源字段名，只做字符集与格式解码，不做语义归一（归一属于解析器）。取值无损表示见 record_value",
    )


class MediaType1(Enum):
    application_json = "application_json"
    text_csv = "text_csv"
    application_xml = "application_xml"


class StructuredRecordSet(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    snapshot_ref: constr(pattern=r"^snp_[0-9A-HJKMNP-TV-Z]{26}$")
    pipeline_version: constr(pattern=r"^[a-z0-9_.-]+@[0-9]+\.[0-9]+\.[0-9]+$") = Field(
        ..., description="结构化处理管线标识与版本（字符集解码、格式转换、记录键规则）"
    )
    media_type: MediaType1 = Field(
        ..., description="原始制品格式；表格文件以 CSV 形式导出时记为 text_csv"
    )
    source_url: str | None = None
    key_rule: constr(min_length=1) | None = Field(
        None,
        description="记录键的生成规则说明（例如“统一社会信用代码+处罚决定书文号”）；同一来源对象在不同快照中按同一规则生成，使字段可跨快照对比",
    )
    records: list[StructuredRecord] = Field(
        ..., description="记录键在一个记录集内唯一（由 SDK 校验）"
    )


class Kind5(Enum):
    tenant_list = "tenant_list"
    group = "group"
    market = "market"
    explicit = "explicit"


class ListKind(Enum):
    customers = "customers"
    partners = "partners"
    competitors = "competitors"
    blocked = "blocked"
    tracked = "tracked"
    counterparties = "counterparties"


class SubjectType(Enum):
    project = "project"
    domain_fact = "domain_fact"
    organization = "organization"
    market = "market"
    relationship = "relationship"
    program = "program"


class Text(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    value: constr(min_length=1)
    prov: Provenance


class TradingSystem(Enum):
    government_procurement = "government_procurement"
    construction_bidding = "construction_bidding"
    soe_procurement = "soe_procurement"


class UlidId(RootModel[constr(pattern=r"^[a-z]{2,4}_[0-9A-HJKMNP-TV-Z]{26}$")]):
    root: constr(pattern=r"^[a-z]{2,4}_[0-9A-HJKMNP-TV-Z]{26}$") = Field(
        ..., description="类型化 ULID，前缀登记在 contracts/identifiers.yaml"
    )


class Change(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^amd_[0-9A-HJKMNP-TV-Z]{26}$")
    field_path: str = Field(
        ...,
        description="被更正公告中的字段 JSON Pointer，如 /lots/0/deadlines/1/at；无法定位时为说明性路径并降为人工复核",
    )
    before: Any | None = None
    after: Any | None = None
    prov: Provenance


class AmendmentSection(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    amends_notice_ids: list[constr(pattern=r"^ntc_[0-9A-HJKMNP-TV-Z]{26}$")] = Field(
        ...,
        description="已解析的前序公告标识；暂未归并时允许为空，保留 original_reference 并进入复核，不伪造标识",
    )
    changes: list[Change]
    original_reference: Text | None = None
    summary: Text | None = None


class Block(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    index: conint(ge=0)
    kind: Kind
    level: conint(ge=1, le=6) | None = Field(None, description="仅 heading")
    text: str | None = Field(
        None,
        description="heading、paragraph、list_item 的纯文本；字符区间以 Unicode 码点计",
    )
    rows: list[list[Cell]] | None = Field(
        None,
        description="仅 table：完整网格，每行列数相同；合并单元格的覆盖位置以 covered_by 指向起始单元格",
    )
    page: conint(ge=1) | None = Field(None, description="分页文档的页码")
    bbox: Bbox | None = None


class Penalty(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    penalty_types: list[PenaltyType] | None = None
    violation: Text | None = None
    legal_basis: Text | None = None


class EndingSection(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    ending_type: EndingType
    lot_keys: list[str] | None = Field(None, description="涉及的标包；为空表示全部")
    reason: Text | None = None


class EntityQualifications(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    qualifications: dict[str, QualificationStatus] = Field(
        ...,
        description="资质代码 → 状态；代码属于平台登记资质体系（行业包资质模板映射），与事实视图中换算后的要求代码同一体系",
    )
    qualifications_pending: list[QualificationsPendingItem] = Field(
        ...,
        description="待核对的资质代码（来源观察冲突，等待 can_edit_profile 成员确认）",
        max_length=20,
    )
    qualifications_complete: bool = Field(
        ..., description="该主体已声明“资质已录全”（写入档案版本与审计）"
    )


class EvidenceRef(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^ev_[0-9A-HJKMNP-TV-Z]{26}$")
    snapshot_ref: constr(pattern=r"^snp_[0-9A-HJKMNP-TV-Z]{26}$")
    pipeline_version: constr(pattern=r"^[a-z0-9_.-]+@[0-9]+\.[0-9]+\.[0-9]+$")
    locator: Locator
    quote_hash: constr(pattern=r"^sha256:[0-9a-f]{64}$") = Field(
        ..., description="被引用原文片段的哈希，用于校验定位未漂移"
    )


class FactVersionRef(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: UlidId
    version: conint(ge=1) = Field(
        ...,
        description="领域事实版本号（public_fact_meta.version），固定引用，不随当前版本指针变化",
    )


class Instant(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    value: AwareDatetime = Field(
        ..., description="UTC；精度为日时取原始时区当日零点换算为 UTC"
    )
    precision: Precision
    tz_raw: constr(pattern=r"^[+-][0-9]{2}:[0-9]{2}$") | None = "+08:00"
    prov: Provenance


class Money(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    value: Decimal
    currency: constr(pattern=r"^[A-Z]{3}$")
    raw: str | None = Field(None, description="原文写法，如“120.5 万元”")
    tax_included: bool | None = Field(None, description="是否含税；原文未说明时不出现")
    prov: Provenance


class NormalizedDocument(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    snapshot_ref: constr(pattern=r"^snp_[0-9A-HJKMNP-TV-Z]{26}$")
    pipeline_version: constr(pattern=r"^[a-z0-9_.-]+@[0-9]+\.[0-9]+\.[0-9]+$") = Field(
        ..., description="文档处理管线标识与版本"
    )
    media_type: MediaType
    source_url: str | None = None
    title: str | None = Field(None, description="来源页面标题（未经解析器确认）")
    language: str | None = "zh"
    blocks: list[Block]
    attachments: list[Attachment] | None = Field(
        None, description="附件：各自是独立快照与独立规范化文档"
    )


class OrgRef(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    name_raw: constr(min_length=1)
    org_id: constr(pattern=r"^org_[0-9A-HJKMNP-TV-Z]{26}$") | None = None
    uscc: (
        constr(pattern=r"^[0-9A-HJ-NPQRTUWXY]{2}[0-9]{6}[0-9A-HJ-NPQRTUWXY]{10}$")
        | None
    ) = Field(None, description="统一社会信用代码（原文出现时）")
    resolution_confidence: confloat(ge=0.0, le=1.0) | None = None
    prov: Provenance
    address: Text | None = None
    uscc_prov: Provenance | None = None


class OrgShareholder(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    kind: Kind2
    org: OrgRef | None = None
    person: Person | None = None
    share_ratio: constr(pattern=r"^(0(\.[0-9]+)?|1(\.0+)?)$") | None = Field(
        None, description="0—1持股比例，原文百分数除以100"
    )
    subscribed_capital: Money | None = None
    share_ratio_prov: Provenance | None = None


class ProcurementConditions(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    scope: Text | None = None
    delivery_period: Text | None = None
    delivery_location: Text | None = None
    payment_terms: Text | None = None
    document_access: Text | None = None
    submission_place: Text | None = None
    opening_place: Text | None = None
    consortium_terms: Text | None = None
    sme_policy: Text | None = None
    imported_products: Text | None = None
    agent_participation: Text | None = None
    contract_pricing: Text | None = None
    planned_publication: Text | None = None
    delivery_region: Region | None = None


class PublicFactMeta(BaseModel):
    id: UlidId
    schema_version: constr(pattern=r"^[0-9]+\.[0-9]+\.[0-9]+$")
    version: conint(ge=1) = Field(..., description="领域事实版本号")
    info_domain: constr(pattern=r"^INF-[A-Z]+$")
    snapshot_ref: constr(pattern=r"^snp_[0-9A-HJKMNP-TV-Z]{26}$")
    revision_ref: constr(pattern=r"^exr_[0-9A-HJKMNP-TV-Z]{26}$")
    rights_tags: list[str] = Field(..., min_length=1)
    classification: Classification
    published_at: Instant | None = None
    observed_at: AwareDatetime
    recorded_at: AwareDatetime
    quality: Quality | None = None


class Rate(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    value: constr(pattern=r"^[0-9]+(\.[0-9]+)?$")
    raw: str | None = None
    basis: Text | None = None
    prov: Provenance


class Requirement(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^req_[0-9A-HJKMNP-TV-Z]{26}$")
    kind: Kind4
    code: str | None = Field(
        None,
        description="归一的资质或要求代码；无法归一时省略，不使用 null。体系与版本见 code_scheme；缺 code_scheme 的代码按行业包默认资质体系解释，不能跨体系比较",
    )
    text: Text
    code_scheme: constr(pattern=r"^[a-z][a-z0-9-]*(:[a-z0-9-]+)*$") | None = Field(
        None,
        description="代码所属体系（平台登记，如 qual:mohurd-construction；平台登记体系与映射，不自称拥有外部标准）；只与 code 一起出现",
    )
    code_scheme_version: constr(min_length=1) | None = Field(
        None, description="体系版本；只与 code_scheme 一起出现"
    )
    grade_required: Text | None = None


class SubjectRef(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    type: SubjectType
    id: UlidId
    version: conint(ge=1) | None = None
    part_id: UlidId | None = None


class TenantMeta(BaseModel):
    id: UlidId
    tenant_id: constr(pattern=r"^tnt_[0-9A-HJKMNP-TV-Z]{26}$")
    schema_version: constr(pattern=r"^[0-9]+\.[0-9]+\.[0-9]+$")
    created_at: AwareDatetime
    updated_at: AwareDatetime


class AcceptanceSection(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^acc_[0-9A-HJKMNP-TV-Z]{26}$")
    contract_id: constr(pattern=r"^ctr_[0-9A-HJKMNP-TV-Z]{26}$") | None = None
    accepted_at: Instant | None = None
    result: Result | None = None
    contract_code: Text | None = None
    contract_name: Text | None = None
    supplier: OrgRef | None = None
    opinion: Text | None = None
    result_prov: Provenance | None = None
    scope: Text | None = None
    dispute: Text | None = None
    judicial_outcome: list[Text] | None = Field(
        None,
        description="裁判结论原文段落分别溯源；技术质量、合同解除及付款结论分别保留，不把当事人主张作为法院结论。",
        min_length=1,
    )


class AwardedItem(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    item_id: constr(pattern=r"^itm_[0-9A-HJKMNP-TV-Z]{26}$") | None = Field(
        None, description="能一一对应时引用要求侧标的；无法对应或原文未列要求标的时省略"
    )
    name: Text
    categories: list[Category] | None = None
    spec: Text | None = None
    brand: Text | None = None
    model: Text | None = None
    manufacturer: OrgRef | None = None
    quantity: Quantity | None = None
    unit_price: Money | None = None
    unit_price_unit: Text | None = None


class BidResult(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^bdr_[0-9A-HJKMNP-TV-Z]{26}$")
    stage: Stage
    bidder: OrgRef
    consortium_members: list[OrgRef] | None = Field(None, min_length=1)
    rank: conint(ge=1) | None = None
    is_winner: bool | None = None
    price: Money | None = None
    score: float | None = None
    price_rate: Rate | None = None
    result_kind: ResultKind | None = Field(
        None, description="普通中标或框架第一阶段入围；入围不代表第二阶段成交/签约"
    )
    consortium_lead: OrgRef | None = None
    price_kind: PriceKind | None = Field(
        None, description="报价金额含义；年单价合计、单价和估算额不得当项目总成交额"
    )
    price_kind_prov: Provenance | None = None
    awarded_items: list[AwardedItem] | None = Field(
        None,
        description="该投标人中标或成交的标的明细（原文列出时）；缺失表示原文未列或未标注，不推断为与要求标的相同",
        min_length=1,
    )


class Contact(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    role: Role
    name: str | None = None
    phone: str | None = None
    email: str | None = None
    organization: OrgRef | None = None
    name_prov: Provenance | None = None
    phone_prov: Provenance | None = None
    email_prov: Provenance | None = None


class ContextPolicyView(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    org_ids: list[OrgId] | None = Field(
        None,
        description="我方主体（集团客户的多个法人）。尚未关联本企业时不出现（不用空列表）：依赖它的条件读取不存在的字段为未知，例如“拟定供应商不是我方”不能成立",
        max_length=10,
        min_length=1,
    )
    offering_categories: list[OfferingCategory] = Field(
        ..., description="行业包分类体系的品类代码"
    )
    service_regions: list[ServiceRegion] = Field(
        ..., description="GB/T 2260 行政区划代码；上级包含全部下级"
    )
    brands: list[Brand] = Field(..., description="自有或授权的品牌")
    entities: dict[str, EntityQualifications] | None = Field(
        None,
        description="我方主体 org_id → 资质状态表。键是 org_ids 的子集；GetContextSnapshot 为 org_ids 中每个主体都给出状态表（没有任何资质记录的主体给空表，未声明录全）。资质条件遍历全部 org_ids，缺状态表的主体按未知参与，不按不具备处理；未关联本企业时不出现",
    )


class ContextSnapshot(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    tenant_id: constr(pattern=r"^tnt_[0-9A-HJKMNP-TV-Z]{26}$")
    org_unit_id: constr(min_length=1) = Field(
        ...,
        description="目标所在的组织单元（identity 的 OrgUnit；标识前缀随 identity 契约登记）",
    )
    context_version: conint(ge=1) = Field(
        ...,
        description="租户上下文版本；档案、清单与单元覆盖的每次提交在同一事务中递增",
    )
    unit_chain: list[UnitChainItem] = Field(
        ...,
        description="合成所用的单元链，从目标所在单元到根单元；逐项取值时未覆盖继承上级，显式空集合不继承",
        min_length=1,
    )
    policy_view: ContextPolicyView


class ContractSection(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^ctr_[0-9A-HJKMNP-TV-Z]{26}$")
    lot_id: constr(pattern=r"^lot_[0-9A-HJKMNP-TV-Z]{26}$") | None = None
    supplier: OrgRef
    amount: Money
    signed_at: Instant | None = None
    term_start: Instant | None = None
    term_end: Instant | None = None
    code: Text | None = None
    name: Text | None = None
    amount_kind: AmountKind | None = Field(
        None, description="原文明确金额语义时填写；最高合同金额不得当作实际成交额"
    )
    conditions: ProcurementConditions | None = None
    amount_kind_prov: Provenance | None = None
    items: list[AwardedItem] | None = Field(
        None, description="合同标的明细（原文列出时）", min_length=1
    )


class Enforcement(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    performance: Performance | None = None
    unperformed_amount: Money | None = None


class Judgment(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    role: Role1 = Field(..., description="主体在本案中的地位")
    cause: Text | None = None
    procedure: Procedure | None = None
    counterparties: list[OrgRef] | None = Field(
        None, description="组织当事人；自然人当事人不记录身份"
    )
    outcome: Text | None = None


class DateRange(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    from_: Instant | None = Field(None, alias="from")
    to: Instant | None = None


class Deadline(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^ddl_[0-9A-HJKMNP-TV-Z]{26}$")
    type: Type
    at: Instant


class DerivationInput(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    evidence_ref: constr(pattern=r"^ev_[0-9A-HJKMNP-TV-Z]{26}$") = Field(
        ...,
        description="该输入取值的证据；输入来自领域事实字段时为该版本中该字段的 provenance.evidence_ref。证据链沿派生证据向下必须无环，末端都是文档证据或记录证据",
    )
    fact_ref: FactVersionRef | None = None
    field_path: constr(pattern=r"^(/([^/~]|~[01])*)*$") | None = Field(
        None, description="fact_ref 所指事实内的 JSON Pointer"
    )


class ConstructionPermit(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    contractor: OrgRef | None = None
    designer: OrgRef | None = None
    supervisor: OrgRef | None = None
    surveyor: OrgRef | None = None
    contract_amount: Money | None = None
    area_sqm: Decimal | None = None
    planned_period: DateRange | None = None


class Completion(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    completed_on: Instant | None = None
    actual_cost: Money | None = None


class InvestmentRecord(PublicFactMeta):
    info_domain: Literal["INF-INVP"]
    id: constr(pattern=r"^ivr_[0-9A-HJKMNP-TV-Z]{26}$")
    record_type: RecordType1 = Field(
        ...,
        description="备案、核准、可研批复、环评批复、用地批复、规划许可、施工许可、竣工验收备案",
    )
    project_code: Text | None = None
    project_name: Text
    investment_project_id: constr(pattern=r"^prj_[0-9A-HJKMNP-TV-Z]{26}$") | None = (
        Field(None, description="entity-graph 的投资项目实体")
    )
    procurement_project_id: constr(pattern=r"^pp_[0-9A-HJKMNP-TV-Z]{26}$") | None = (
        Field(
            None,
            description="关联的采购项目聚合（如施工许可对应的施工招标项目），由聚合规则维护",
        )
    )
    owner: OrgRef | None = None
    authority: Text | None = None
    document_no: Text | None = None
    decided_on: Instant
    location: Region | None = None
    total_investment: Money | None = None
    funding_source: Text | None = None
    construction_scale: Text | None = None
    industry: Category | None = None
    construction_permit: ConstructionPermit | None = None
    completion: Completion | None = None


class Item(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^itm_[0-9A-HJKMNP-TV-Z]{26}$")
    name: Text
    categories: list[Category] | None = None
    spec: Text | None = None
    quantity: Quantity | None = None
    brand_required: Text | None = None
    model_required: Text | None = None
    unit_price: Money | None = None
    unit_price_unit: Text | None = None


class LotSection(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    lot_key: str = Field(..., description='原文标包号；未分包为 "1"')
    lot_id: constr(pattern=r"^lot_[0-9A-HJKMNP-TV-Z]{26}$") | None = Field(
        None, description="归并到的项目标包"
    )
    name: Text | None = None
    procurement_category: ProcurementCategory | None = None
    items: list[Item] | None = None
    requirements: list[Requirement] | None = None
    deadlines: list[Deadline] | None = None
    budget: Money | None = None
    max_price: Money | None = None
    consortium_allowed: bool | None = None
    authorization_required: bool | None = Field(None, description="是否要求原厂授权")
    bid_bond: Money | None = None
    document_fee: Money | None = None
    bond_guarantee_accepted: bool | None = Field(None, description="是否接受保函")
    evaluation_method: EvaluationMethod | None = None
    bid_results: list[BidResult] | None = None
    conditions: ProcurementConditions | None = None
    max_price_rate: Rate | None = None
    project_name: Text | None = None
    project_code: Text | None = None
    project_owner: OrgRef | None = None
    estimated_contract_amount: Money | None = None
    ending: EndingSection | None = None


class MembershipSnapshot1(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    as_of: AwareDatetime = Field(
        ...,
        description="成员关系的取值时点：持续型推进取本期结束时间，事件触发的求值取逻辑求值时间；集团成员按该时点有效的控制关系，不用之后才生效或撤销的关系",
    )
    source_version: constr(min_length=1) = Field(
        ...,
        description="成员来源的版本：租户清单为 context_version，集团为实体图谱关系版本，市场为所用统计版本",
    )
    member_count: conint(ge=0)
    members: list[SubjectRef] = Field(..., max_length=200)
    members_object: constr(min_length=1) | None = Field(
        None, description="成员清单的对象存储键（不含签名地址）"
    )
    members_hash: constr(pattern=r"^sha256:[0-9a-f]{64}$") = Field(
        ..., description="按成员标识排序后的清单哈希"
    )


class MembershipSnapshot2(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    as_of: AwareDatetime = Field(
        ...,
        description="成员关系的取值时点：持续型推进取本期结束时间，事件触发的求值取逻辑求值时间；集团成员按该时点有效的控制关系，不用之后才生效或撤销的关系",
    )
    source_version: constr(min_length=1) = Field(
        ...,
        description="成员来源的版本：租户清单为 context_version，集团为实体图谱关系版本，市场为所用统计版本",
    )
    member_count: conint(ge=0)
    members: list[SubjectRef] | None = Field(None, max_length=200)
    members_object: constr(min_length=1) = Field(
        ..., description="成员清单的对象存储键（不含签名地址）"
    )
    members_hash: constr(pattern=r"^sha256:[0-9a-f]{64}$") = Field(
        ..., description="按成员标识排序后的清单哈希"
    )


class MembershipSnapshot(RootModel[MembershipSnapshot1 | MembershipSnapshot2]):
    root: MembershipSnapshot1 | MembershipSnapshot2 = Field(
        ...,
        description="集合在取值时点的实际成员。成员不多于 200 个时内联 members；更多时存对象存储并以 members_object 引用，两者都以 members_hash 校验",
    )


class Notice(PublicFactMeta):
    id: constr(pattern=r"^ntc_[0-9A-HJKMNP-TV-Z]{26}$")
    info_domain: Literal["INF-PROC"]
    notice_type: NoticeType
    trading_system: TradingSystem | None = None
    method: ProcurementMethod | None = None
    procurement_category: ProcurementCategory | None = None
    title: Text
    notice_code: Text | None = None
    project_code: Text | None = None
    approval_code: Text | None = None
    project_id: constr(pattern=r"^pp_[0-9A-HJKMNP-TV-Z]{26}$") | None = Field(
        None,
        description="当前归并到的采购项目。权威记录是 domain-intelligence 的 AggregationLink，本字段为其当前指针",
    )
    related_notice_ids: list[constr(pattern=r"^ntc_[0-9A-HJKMNP-TV-Z]{26}$")] | None = (
        Field(None, description="公告中显式引用的前序公告")
    )
    buyer: OrgRef | None = None
    agency: OrgRef | None = None
    project_owner: OrgRef | None = None
    single_source_supplier: OrgRef | None = None
    location: Region | None = None
    funding_source: Text | None = None
    budget: Money | None = None
    max_price: Money | None = None
    lots: list[LotSection] = Field(..., min_length=1)
    amendment: AmendmentSection | None = None
    contract: ContractSection | None = None
    acceptance: AcceptanceSection | None = None
    ending: EndingSection | None = None
    contacts: list[Contact] | None = None
    award_amount: Money | None = None
    result_details_pending: bool | None = Field(
        None,
        description="结果主体/标包只在当前不可得附件中，已记录quality警告时为true；不伪造中标人",
    )


class OrgAbnormal(PublicFactMeta):
    info_domain: Literal["INF-ORG"]
    id: constr(pattern=r"^oga_[0-9A-HJKMNP-TV-Z]{26}$")
    subject: OrgRef
    list_type: ListType = Field(..., description="经营异常名录、严重违法失信名单")
    reason: Text | None = None
    reason_code: ReasonCode | None = None
    listed_on: Instant
    decision_authority: Text | None = None
    removed_on: Instant | None = None
    removal_reason: Text | None = None


class OrgChange(PublicFactMeta):
    info_domain: Literal["INF-ORG"]
    id: constr(pattern=r"^ogc_[0-9A-HJKMNP-TV-Z]{26}$")
    subject: OrgRef
    change_type: OrgChangeType
    change_item_raw: Text | None = None
    changed_on: Instant | None = None
    before: Text | None = None
    after: Text | None = None
    status_after: OrgStatus | None = None
    capital_before: Money | None = None
    capital_after: Money | None = None
    org_shareholders_added: list[OrgRef] | None = None
    org_shareholders_removed: list[OrgRef] | None = None
    status_after_prov: Provenance | None = None


class OrgRegistration(PublicFactMeta):
    info_domain: Literal["INF-ORG"]
    id: constr(pattern=r"^ogr_[0-9A-HJKMNP-TV-Z]{26}$")
    subject: OrgRef
    uscc: (
        constr(pattern=r"^[0-9A-HJ-NPQRTUWXY]{2}[0-9]{6}[0-9A-HJ-NPQRTUWXY]{10}$")
        | None
    ) = Field(None, description="统一社会信用代码")
    name: Text
    former_names: list[Text] | None = None
    enterprise_type: OrgEnterpriseType | None = None
    enterprise_type_raw: Text | None = None
    status: OrgStatus | None = None
    established_on: Instant | None = None
    business_term: DateRange | None = None
    registered_capital: Money | None = None
    paid_in_capital: Money | None = None
    region: Region | None = None
    address: Text | None = None
    industry: Category | None = None
    business_scope: Text | None = None
    registration_authority: Text | None = None
    approved_on: Instant | None = None
    legal_representative: Person | None = None
    shareholders: list[OrgShareholder] | None = None
    branches: list[OrgRef] | None = None
    parent: OrgRef | None = None
    uscc_prov: Provenance | None = None
    status_prov: Provenance | None = None
    legal_representative_prov: Provenance | None = None


class ProjectLot(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^lot_[0-9A-HJKMNP-TV-Z]{26}$")
    lot_key: str
    name: Text | None = None
    procurement_category: ProcurementCategory | None = None
    items: list[Item] | None = None
    requirements: list[Requirement] | None = None
    deadlines: list[Deadline] | None = Field(
        None, description="应用全部更正后的当前时间节点"
    )
    budget: Money | None = None
    max_price: Money | None = None
    consortium_allowed: bool | None = None
    authorization_required: bool | None = None
    status: Status | None = None
    candidates: list[BidResult] | None = None
    winners: list[BidResult] | None = Field(
        None,
        description="实际中标/成交供应商，可多家；框架第一阶段入围另存shortlisted_suppliers",
    )
    award_amount: Money | None = None
    contract_ids: list[constr(pattern=r"^ctr_[0-9A-HJKMNP-TV-Z]{26}$")] | None = None
    conditions: ProcurementConditions | None = None
    max_price_rate: Rate | None = None
    project_name: Text | None = None
    project_code: Text | None = None
    project_owner: OrgRef | None = None
    estimated_contract_amount: Money | None = None
    ending: EndingSection | None = None
    shortlisted_suppliers: list[BidResult] | None = None


class Signal(TenantMeta):
    id: constr(pattern=r"^sig_[0-9A-HJKMNP-TV-Z]{26}$")
    objective_id: constr(pattern=r"^obj_[0-9A-HJKMNP-TV-Z]{26}$") = Field(
        ..., description="租户启用的商业目标 TenantObjective"
    )
    objective_code: constr(pattern=r"^APP-[A-Z]+\.[A-Z][0-9]{2}$") = Field(
        ..., description="应用领域包中的目标编号，如 APP-TENDER.A01"
    )
    policy: PolicyRef
    subject: SubjectRef
    disposition: Disposition
    needs_review: bool | None = Field(
        None, description="存在按 missing: review 处理的未知硬条件"
    )
    score: int = Field(..., description="同一策略下的排序分，不代表成交概率")
    reason_code: constr(pattern=r"^[A-Z][A-Z0-9_]*$") | None = Field(
        None, description="excluded 时的原因代码（应用领域包 reason_codes 登记）"
    )
    trace: list[RuleTrace]
    features: dict[str, Any] | None = Field(
        None, description="特征快照：特征名 → 值；未知的特征不出现，不含其他租户的信息"
    )
    view_snapshot_hash: constr(pattern=r"^sha256:[0-9a-f]{64}$")
    context_snapshot_version: conint(ge=1)
    evaluated_at: AwareDatetime = Field(
        ..., description="逻辑求值时间，即触发事件的时间（表达式中的 now）"
    )
    trigger_event_id: constr(pattern=r"^evt_[0-9A-HJKMNP-TV-Z]{26}$")
    status: Status2 = Field(
        ...,
        description="open 待处理；triaged 已由用户调整处置；converted 已发起研究或推进；dismissed 用户忽略；closed 求值单位进入终态（如截止、终止）后关闭",
    )
    update_count: conint(ge=0) = Field(..., description="已追加的 SignalUpdate 数")


class SubjectSet(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    kind: Kind5 = Field(
        ...,
        description="tenant_list 为 customer-context 的租户实体清单；group 为以 ref 所指组织为根、按实体图谱控制关系展开的集团成员；market 为租户市场 mkt_ 范围内的对象；explicit 为用户逐个指定",
    )
    list_kind: ListKind | None = Field(
        None, description="kind=tenant_list 时的清单种类"
    )
    ref: SubjectRef | None = None
    resolved: MembershipSnapshot


class Comparison(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    kind: Kind1
    baseline: Period
    baseline_members: MembershipSnapshot | None = None
    baseline_ref: UlidId | None = None


class Rating(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    scheme: Text | None = None
    level: str
    valid: DateRange | None = None


class CreditRecord(PublicFactMeta):
    info_domain: Literal["INF-CRED"]
    id: constr(pattern=r"^crd_[0-9A-HJKMNP-TV-Z]{26}$")
    record_type: RecordType
    subject: OrgRef
    case_or_doc_no: Text
    authority: OrgRef | None = None
    filed_on: Instant | None = None
    published_on: Instant | None = None
    amount: Money | None = None
    resolved: bool = Field(..., description="已履行、已结案、已移出名单或已修复")
    status_raw: Text | None = None
    resolved_on: Instant | None = None
    enforcement: Enforcement | None = None
    penalty: Penalty | None = None
    judgment: Judgment | None = None
    rating: Rating | None = None
    persons: list[Person] | None = None


class DerivationEvidenceRef(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^ev_[0-9A-HJKMNP-TV-Z]{26}$")
    kind: Literal["derivation"]
    transform: constr(pattern=r"^[a-z0-9_.-]+@[0-9]+\.[0-9]+\.[0-9]+$") = Field(
        ...,
        description="转换标识与版本，由信息领域包或平台登记；同一版本对同样输入必须给出同样结果。版本固定全部影响结果的依赖：转换参数、时区、舍入与精度规则等，任一变化都发布新版本",
    )
    inputs: list[DerivationInput] = Field(
        ...,
        description="按转换参数顺序排列。上界是单条派生证据的资源上限，不决定语义：字段派生与统计按计算目的区分，不按输入个数区分。超出上界时登记被拒绝，不截断；字段派生拆成多级，中间结果各自登记派生证据",
        max_length=64,
        min_length=1,
    )
    reference_data: list[ReferenceDataVersion] | None = Field(
        None, description="转换所用参考数据的版本（区划代码表、节假日表、计量单位表等）"
    )
    value_hash: constr(pattern=r"^sha256:[0-9a-f]{64}$") = Field(
        ..., description="派生取值按 RFC 8785 规范化后的哈希"
    )


class ProcurementProject(PublicFactMeta):
    id: constr(pattern=r"^pp_[0-9A-HJKMNP-TV-Z]{26}$")
    info_domain: Literal["INF-PROC"]
    stage: Stage1
    trading_system: TradingSystem | None = None
    method: ProcurementMethod | None = None
    procurement_category: ProcurementCategory | None = None
    title: Text
    project_code: Text | None = None
    approval_code: Text | None = None
    investment_project_id: constr(pattern=r"^prj_[0-9A-HJKMNP-TV-Z]{26}$") | None = (
        Field(
            None,
            description="所属投资项目（entity-graph 的 Project 实体）的便捷字段：仅在整个采购项目归属唯一、已确认的投资项目时出现。归属以 entity-graph 的 part_of 归属边为准（可按标包、可归属多个项目、可修订撤销），时间线与项目链按归属边查询，不以本字段为不变量",
        )
    )
    buyer: OrgRef
    agency: OrgRef | None = None
    project_owner: OrgRef | None = None
    location: Region | None = None
    budget: Money | None = None
    notice_ids: list[NoticeId] = Field(
        ...,
        description="有发布时间时按发布时间排序，否则按稳定公告标识排序。",
        min_length=1,
    )
    first_published_at: Instant | None = None
    lots: list[ProjectLot] = Field(
        ...,
        description="已确认标包；空数组表示尚无已确认标包，不能把公告默认容器1推断成跨公告标包。",
        min_length=0,
    )


RecordValue.model_rebuild()
