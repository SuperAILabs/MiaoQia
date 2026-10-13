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


class AmendsNoticeId(RootModel[constr(pattern=r"^ntc_[0-9A-HJKMNP-TV-Z]{26}$")]):
    root: constr(pattern=r"^ntc_[0-9A-HJKMNP-TV-Z]{26}$")


class Stage(Enum):
    opening = "opening"
    candidate = "candidate"
    award = "award"


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


class Role(Enum):
    buyer = "buyer"
    agency = "agency"
    other = "other"


class Contact(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    role: Role
    name: str | None = None
    phone: str | None = None
    email: str | None = None


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


class Decimal(RootModel[constr(pattern=r"^-?[0-9]+(\.[0-9]+)?$")]):
    root: constr(pattern=r"^-?[0-9]+(\.[0-9]+)?$") = Field(
        ..., description="十进制字符串，避免浮点误差"
    )


class EndingType(Enum):
    terminated = "terminated"
    failed_bid = "failed_bid"
    voided = "voided"


class EvaluationMethod(Enum):
    lowest_price = "lowest_price"
    comprehensive_scoring = "comprehensive_scoring"
    other = "other"


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


class Kind(Enum):
    organization = "organization"
    natural_person = "natural_person"


class OrgStatus(Enum):
    active = "active"
    suspended = "suspended"
    revoked = "revoked"
    cancelled = "cancelled"
    moved_out = "moved_out"


class Person(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    name: constr(min_length=1)
    role: str | None = Field(None, description="原文职务，如 董事长、执行董事、经理")


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


class Method(Enum):
    rule = "rule"
    model = "model"
    manual = "manual"


class Classification(Enum):
    public = "public"
    internal = "internal"
    sensitive = "sensitive"
    personal = "personal"


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


class Kind1(Enum):
    qualification = "qualification"
    performance = "performance"
    personnel = "personnel"
    credit = "credit"
    authorization = "authorization"
    consortium = "consortium"
    technical = "technical"
    other = "other"


class TradingSystem(Enum):
    government_procurement = "government_procurement"
    construction_bidding = "construction_bidding"
    soe_procurement = "soe_procurement"


class UlidId(RootModel[constr(pattern=r"^[a-z]{2,4}_[0-9A-HJKMNP-TV-Z]{26}$")]):
    root: constr(pattern=r"^[a-z]{2,4}_[0-9A-HJKMNP-TV-Z]{26}$") = Field(
        ..., description="类型化 ULID，前缀登记在 contracts/identifiers.yaml"
    )


class Provenance(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    evidence_ref: UlidId
    method: Method
    confidence: confloat(ge=0.0, le=1.0)
    extractor: constr(pattern=r"^[a-z0-9_.-]+@[0-9]+\.[0-9]+\.[0-9]+$") | None = Field(
        None, description="解析器标识与版本"
    )


class Quantity(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    value: Decimal
    unit: str | None = Field(None, description="归一单位")
    unit_raw: str | None = None
    prov: Provenance


class Region(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    code: constr(pattern=r"^[0-9]{6}$") = Field(
        ..., description="GB/T 2260 行政区划代码"
    )
    name_raw: str | None = None
    prov: Provenance


class Text(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    value: constr(min_length=1)
    prov: Provenance


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
    amends_notice_ids: list[AmendsNoticeId] = Field(..., min_length=1)
    changes: list[Change]


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


class OrgShareholder(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    kind: Kind
    org: OrgRef | None = None
    person: Person | None = None
    share_ratio: Decimal | None = None
    subscribed_capital: Money | None = None


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


class Requirement(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^req_[0-9A-HJKMNP-TV-Z]{26}$")
    kind: Kind1
    code: str | None = Field(
        None, description="归一的资质或要求代码；无法归一时为 null"
    )
    text: Text


class AcceptanceSection(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^acc_[0-9A-HJKMNP-TV-Z]{26}$")
    contract_id: constr(pattern=r"^ctr_[0-9A-HJKMNP-TV-Z]{26}$") | None = None
    accepted_at: Instant | None = None
    result: Result | None = None


class BidResult(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
    )
    id: constr(pattern=r"^bdr_[0-9A-HJKMNP-TV-Z]{26}$")
    stage: Stage
    bidder: OrgRef
    consortium_members: list[OrgRef] | None = None
    rank: conint(ge=1) | None = None
    is_winner: bool | None = None
    price: Money | None = None
    score: float | None = None


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
    bond_guarantee_accepted: bool | None = Field(None, description="是否接受保函")
    evaluation_method: EvaluationMethod | None = None
    bid_results: list[BidResult] | None = None


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
    buyer: OrgRef
    agency: OrgRef | None = None
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
    changed_on: Instant
    before: Text | None = None
    after: Text | None = None
    status_after: OrgStatus | None = None
    capital_before: Money | None = None
    capital_after: Money | None = None
    org_shareholders_added: list[OrgRef] | None = None
    org_shareholders_removed: list[OrgRef] | None = None
    natural_person_changes: conint(ge=0) | None = Field(
        None, description="涉及自然人的变更条数"
    )
    personal_detail: list[Person] | None = None


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
    status: OrgStatus
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
        None, description="中标或成交人；框架协议可有多个"
    )
    award_amount: Money | None = None
    contract_ids: list[constr(pattern=r"^ctr_[0-9A-HJKMNP-TV-Z]{26}$")] | None = None


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
        Field(None, description="所属投资项目（entity-graph 的 Project 实体）")
    )
    buyer: OrgRef
    agency: OrgRef | None = None
    location: Region | None = None
    budget: Money | None = None
    notice_ids: list[NoticeId] = Field(..., description="按发布时间排序", min_length=1)
    first_published_at: Instant | None = None
    lots: list[ProjectLot] = Field(..., min_length=1)
