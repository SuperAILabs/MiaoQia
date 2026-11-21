# Build from the root of the allowlisted release snapshot.
FROM ghcr.io/astral-sh/uv:0.11.32@sha256:df4cae8f3a96d175e2e5f992e597550000edbe78fdc2594d5cd8de1a217f504c AS uv
FROM docker.io/library/python:3.12.12-slim-bookworm@sha256:593bd06efe90efa80dc4eee3948be7c0fde4134606dd40d8dd8dbcade98e669c AS python
FROM python AS build

COPY --from=uv /uv /usr/local/bin/uv
ENV UV_PROJECT_ENVIRONMENT=/opt/miaoqia/venv \
    UV_PYTHON_DOWNLOADS=never \
    UV_LINK_MODE=copy \
    UV_NO_CACHE=1
WORKDIR /build/data
COPY data/ ./

# Hatchling's build dependencies are outside the runtime lock. Omit local
# wheel installer metadata, which records build-time file timestamps.
RUN printf '%s\n' \
      'hatchling==1.27.0 --hash=sha256:d3a2f3567c4f926ea39849cdf924c7e99e6686c9c8e288ae1037c8fa2a5d937b' \
      'packaging==26.3 --hash=sha256:d7193f7c8e4e93f444fde0262bf90af30e16fa0ad0ad44cb553c87339b23cd1c' \
      'pathspec==0.12.1 --hash=sha256:a0d503e138a4c123b27490a4f7beda6a01c6f288df0e4a8b79c7eb0dc7b4cc08' \
      'pluggy==1.6.0 --hash=sha256:e920276dd6813095e9377c0bc5566d94c932c33b27a3e3945d8389c374dd4746' \
      'trove-classifiers==2025.11.14.15 --hash=sha256:d1dac259c1e908939862e3331177931c6df0a37af2c1a8debcc603d9115fcdd9' \
      > /tmp/build-constraints.txt \
    && uv build --all-packages --wheel --build-constraints /tmp/build-constraints.txt --require-hashes --out-dir /tmp/wheels \
    && uv sync --locked --all-packages --no-dev --no-install-workspace --no-build \
    && UV_NO_INSTALLER_METADATA=1 uv pip install --no-index --no-deps --python /opt/miaoqia/venv/bin/python /tmp/wheels/*.whl \
    && uv pip check --python /opt/miaoqia/venv/bin/python

FROM python
ENV PATH="/opt/miaoqia/venv/bin:${PATH}" \
    HOME=/home/miaoqia \
    MIAOQIA_DEPLOY_DIR=/opt/miaoqia/deploy \
    MIAOQIA_PACKS_DIR=/opt/miaoqia/packs \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
RUN groupadd --gid 10001 miaoqia \
    && useradd --uid 10001 --gid 10001 --create-home --no-log-init --shell /usr/sbin/nologin miaoqia \
    && chage --lastday 0 miaoqia
WORKDIR /opt/miaoqia
COPY --from=build /opt/miaoqia/venv/ ./venv/
COPY deploy/subsystems.yaml ./deploy/
COPY deploy/acquisition/roles.yaml ./deploy/acquisition/
COPY deploy/data/roles.yaml ./deploy/data/
COPY deploy/platform/roles.yaml ./deploy/platform/
COPY deploy/vps/data-plane/verify_archive.py deploy/vps/data-plane/backup_manifest.py \
     deploy/vps/data-plane/verify_list_retention.py ./deploy/vps/data-plane/

# Pack entry points resolve code relative to their manifests. The frozen recipe
# and the organization fixture adapter also verify these specific metadata files.
COPY packs/application_domains/tendering/*.yaml ./packs/application_domains/tendering/
COPY packs/application_domains/tendering/cards/ ./packs/application_domains/tendering/cards/
COPY packs/application_domains/tendering/feedback/ ./packs/application_domains/tendering/feedback/
COPY packs/application_domains/tendering/frameworks/ ./packs/application_domains/tendering/frameworks/
COPY packs/application_domains/tendering/objectives/ ./packs/application_domains/tendering/objectives/
COPY packs/application_domains/tendering/outcomes/ ./packs/application_domains/tendering/outcomes/
COPY packs/application_domains/tendering/policy/ ./packs/application_domains/tendering/policy/
COPY packs/application_domains/tendering/roles/ ./packs/application_domains/tendering/roles/
COPY packs/information_domains/credit/pack.yaml ./packs/information_domains/credit/
COPY packs/information_domains/investment_projects/*.yaml ./packs/information_domains/investment_projects/
COPY packs/information_domains/organizations/pack.yaml ./packs/information_domains/organizations/
COPY packs/information_domains/organizations/src/ ./packs/information_domains/organizations/src/
COPY packs/information_domains/organizations/corpus/r2-fixture-registry-v1.json ./packs/information_domains/organizations/corpus/
COPY packs/information_domains/procurement/*.yaml ./packs/information_domains/procurement/
COPY packs/information_domains/procurement/src/ ./packs/information_domains/procurement/src/
COPY packs/information_domains/procurement/corpus/building-topic-scope.json packs/information_domains/procurement/corpus/lifecycle-manifest.json ./packs/information_domains/procurement/corpus/
COPY packs/information_domains/procurement/corpus/official-building-award-20261026/reviewed.json ./packs/information_domains/procurement/corpus/official-building-award-20261026/
COPY packs/information_domains/procurement/corpus/official-building-tender-20261026/reviewed.json ./packs/information_domains/procurement/corpus/official-building-tender-20261026/
COPY packs/industries/building-materials/*.yaml packs/industries/building-materials/topic-evaluation.json ./packs/industries/building-materials/
COPY packs/industries/building-materials/src/ ./packs/industries/building-materials/src/

RUN chmod -R a+rX,go-w deploy packs
USER 10001:10001
CMD ["mq-acquisition", "--help"]
