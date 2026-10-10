# syntax=docker/dockerfile:1
# The terrain bake: GDAL (for gdalbuildvrt) + rasterio/Pillow/pmtiles.
# Built and published by CI as ghcr.io/<owner>/streetcryptid-terrain-bake.
FROM python:3.12-slim-bookworm

RUN apt-get update \
 && apt-get install -y --no-install-recommends gdal-bin ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY requirements.txt /opt/terrain/requirements.txt
RUN pip install --no-cache-dir -r /opt/terrain/requirements.txt

COPY bake_terrain.py /opt/terrain/bake_terrain.py

# Runs as an arbitrary non-root uid in the cluster; nothing here needs root.
USER 65532:65532
ENTRYPOINT ["python", "/opt/terrain/bake_terrain.py"]
