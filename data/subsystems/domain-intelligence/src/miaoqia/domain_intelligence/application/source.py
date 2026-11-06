"""来源公告与真实解析修订一致性校验，补建和增强共同使用。"""
import copy
from datetime import datetime
from ..domain.versions import META, stabilize_result_ids, value_hash


def validate_source(notice, row, revision):
    if (revision is None or revision.pack_id != row.info_domain or revision.source_id != row.source_id
            or revision.source_key != row.source_key or revision.snapshot_ref != notice["snapshot_ref"]
            or revision.id != notice["revision_ref"] or revision.quality_status == "suspect"
            or revision.observed_at != datetime.fromisoformat(notice["observed_at"].replace("Z", "+00:00"))
            or list(revision.rights_tags) != notice["rights_tags"] or row.ordinal >= len(revision.facts)):
        raise ValueError(f"公告与真实解析修订不一致：{notice["id"]}")
    extracted = stabilize_result_ids(notice, copy.deepcopy(revision.facts[row.ordinal]))
    body = {k: v for k, v in notice.items() if k not in META}
    if value_hash(extracted) != value_hash(notice) or extracted != body:
        raise ValueError(f"公告字段/证据与解析修订不一致：{notice["id"]}")
