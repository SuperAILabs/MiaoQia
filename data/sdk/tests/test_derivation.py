"""固定字段/DAG/参考内容与RFC8785重算，不依赖数据库或网络。"""
import copy

import pytest
from miaoqia.sdk import Document, ExtractionContext
from miaoqia.sdk.derivation import canonical, evaluate, fingerprint, pointer
from miaoqia.sdk.testing import html_to_document


@pytest.fixture
def graph():
    document = html_to_document("<p>名称</p>", "snp_" + "0" * 26)
    doc = Document(document)
    ctx = ExtractionContext(doc, "test@1.0.0")
    field = ctx.text(doc.block_span(0))
    leaf = field["prov"]["evidence_ref"]
    fact = {"id": "ntc_" + "0" * 26, "version": 1, "title": field}
    root = "ev_" + "1" * 26
    refs = {**ctx.evidence, root: {"id": root, "kind": "derivation", "transform": "test.identity@1.0.0",
        "inputs": [{"evidence_ref": leaf, "fact_ref": {"id": fact["id"], "version": 1}, "field_path": "/title/value"}],
        "reference_data": [{"name": "test-policy", "version": "1"}], "value_hash": fingerprint("名称")}}
    reference = {"content": {"n": 1.0, "non_production": True}}
    reference["hash"] = fingerprint(reference["content"])
    kwargs = dict(get_ref=refs.get, get_document=lambda _: document,
                  get_fact=lambda i, v: fact if (i, v) == (fact["id"], 1) else None,
                  get_reference=lambda n, v: reference,
                  transforms={"test.identity@1.0.0": lambda values, references: values[0]})
    return root, refs, fact, document, reference, kwargs


def test_rfc8785_and_shared_dag(graph):
    assert canonical({"b": 1.0, "a": -0.0}) == b'{"a":0,"b":1}'
    root, refs, fact, _, _, kwargs = graph
    assert evaluate(root, **kwargs) == "名称"
    second = "ev_" + "2" * 26
    refs[second] = {**copy.deepcopy(refs[root]), "id": second,
                    "inputs": [{"evidence_ref": root}, {"evidence_ref": root}]}
    assert evaluate(second, **kwargs) == "名称"
    assert pointer({"a/b": {"~c": [4]}}, "/a~1b/~0c/0") == 4


@pytest.mark.parametrize("fault", ["cycle", "missing", "wrong_version", "field", "proof", "hash", "reference", "unknown_transform", "record", "unknown_kind", "doc_hash", "too_many", "unfixed_path", "derived_value"])
def test_invalid_chains_reject(graph, fault):
    root, refs, fact, document, reference, kwargs = graph
    edge = refs[root]["inputs"][0]
    leaf = edge["evidence_ref"]
    if fault == "cycle":
        edge["evidence_ref"] = root
    elif fault == "missing":
        refs.pop(leaf)
    elif fault == "wrong_version":
        edge["fact_ref"]["version"] = 2
    elif fault == "field":
        edge["field_path"] = "/missing/value"
    elif fault == "proof":
        fact["title"]["prov"]["evidence_ref"] = "ev_" + "9" * 26
    elif fault == "hash":
        refs[root]["value_hash"] = fingerprint("wrong")
    elif fault == "reference":
        reference["content"]["n"] = 2
    elif fault == "unknown_transform":
        refs[root]["transform"] = "unknown@1.0.0"
    elif fault in ("record", "unknown_kind"):
        refs[leaf]["kind"] = fault
    elif fault == "doc_hash":
        refs[leaf]["quote_hash"] = fingerprint("tampered")
    elif fault == "too_many":
        refs[root]["inputs"] *= 65
    elif fault == "unfixed_path":
        edge.pop("fact_ref")
    else:
        child = "ev_" + "2" * 26
        refs[child] = {**copy.deepcopy(refs[root]), "id": child}
        edge["evidence_ref"] = child
        fact["title"]["prov"]["evidence_ref"] = child
        # Child uses the document directly so it computes 名称; parent points at a different fixed value.
        refs[child]["inputs"] = [{"evidence_ref": leaf}]
        fact["title"]["value"] = "different"
    with pytest.raises((ValueError, KeyError, IndexError)):
        evaluate(root, **kwargs)


def test_depth_bound_and_noncanonical_values_reject(graph):
    root, refs, _, _, _, kwargs = graph
    previous = root
    for i in range(2, 67):
        next_id = "ev_" + str(i).zfill(26)
        refs[next_id] = {**copy.deepcopy(refs[root]), "id": next_id, "inputs": [{"evidence_ref": previous}]}
        previous = next_id
    with pytest.raises(ValueError, match="超界"):
        evaluate(previous, **kwargs)
    with pytest.raises(ValueError):
        fingerprint(float("nan"))


@pytest.mark.parametrize("node_count", [1024, 1025])
def test_total_unique_node_bound(graph, node_count):
    root, refs, _, _, _, kwargs = graph
    template = copy.deepcopy(refs[root])
    leaf = copy.deepcopy(refs[template["inputs"][0]["evidence_ref"]])
    refs.clear()

    def add_node(value):
        ref_id = "ev_" + str(len(refs) + 1).zfill(26)
        refs[ref_id] = {**copy.deepcopy(value), "id": ref_id}
        return {"evidence_ref": ref_id}

    # Sixteen branches plus one root; all nodes have at most 64 inputs.
    remaining = node_count - 17
    branches = []
    for _ in range(16):
        edges = [add_node(leaf) for _ in range(min(64, remaining))]
        remaining -= len(edges)
        branches.append(add_node({**template, "inputs": edges}))
    root = add_node({**template, "inputs": branches})["evidence_ref"]
    assert len(refs) == node_count
    if node_count > 1024:
        with pytest.raises(ValueError, match="超界"):
            evaluate(root, **kwargs)
    else:
        assert evaluate(root, **kwargs) == "名称"
        # Reusing completed nodes must not consume the unique-node budget.
        refs[root]["inputs"] *= 2
        assert evaluate(root, **kwargs) == "名称"


@pytest.mark.parametrize("depth", [64, 65, 66])
@pytest.mark.parametrize("leaf_first", [True, False])
def test_cached_subgraph_depth_bound(graph, depth, leaf_first):
    root, refs, _, _, _, kwargs = graph
    template = copy.deepcopy(refs.pop(root))
    previous = template["inputs"][0]["evidence_ref"]
    edges = []
    for i in range(1, depth - 1):
        ref_id = "ev_" + str(i).zfill(26)
        refs[ref_id] = {**copy.deepcopy(template), "id": ref_id,
                        "inputs": [{"evidence_ref": previous}]}
        previous = ref_id
        edges.append({"evidence_ref": ref_id})
    root = "ev_" + str(depth).zfill(26)
    refs[root] = {**template, "id": root,
                  "inputs": edges if leaf_first else list(reversed(edges))}
    if depth > 64:
        with pytest.raises(ValueError, match="超界"):
            evaluate(root, **kwargs)
    else:
        assert evaluate(root, **kwargs) == "名称"
