#!/usr/bin/env python3
"""Bake the Copernicus GLO-30 DEM into a Terrarium-encoded WebP PMTiles archive.

The app shades parkland from elevation (hillshade, contours, treeline). This is
where that elevation comes from:

    Copernicus DEM GLO-30 (public, 1 arcsec)  ┐
    Copernicus DEM GLO-90 (where 30 m is withheld)  ┴→ one GDAL VRT, read in place
    → z12 Web Mercator tiles, bilinear, 256 px
    → z11..z0 by averaging 2x2 children
    → Terrarium RGB (metres = R*256 + G + B/256 - 32768), integer metres, WebP lossless
    → terrain.pmtiles  (then `streetcryptid-map-server tiles import-terrain`)

Integer metres, not Mapbox terrain-RGB's 0.1 m: the app quantizes elevation to
8-bit contour bands anyway, and the tenths are noise that defeats WebP — the
blue channel of a 0.1 m encoding is random, so tiles come out several times
larger. With B always 0 a coastal-hills z12 tile is ~10-20 KB.

Tiles whose every pixel is at or below sea level are not written: the app reads
a missing terrain tile as 0 m, which is what the sea is.

The bake is resumable. Progress lives in a SQLite staging database in --workdir;
a restarted bake skips every 1-degree cell it already finished.

Copernicus DEM attribution (required by the licence):
  "© DLR e.V. 2010-2014 and © Airbus Defence and Space GmbH 2014-2018 provided
   under COPERNICUS by the European Union and ESA; all rights reserved"
"""

from __future__ import annotations

import argparse
import io
import logging
import math
import multiprocessing as mp
import os
import sqlite3
import sys
import time
import urllib.request
from dataclasses import dataclass

import mercantile
import numpy as np
import rasterio
from PIL import Image
from pmtiles.tile import Compression, TileType, zxy_to_tileid
from pmtiles.writer import Writer
from rasterio.enums import Resampling
from rasterio.transform import from_bounds
from rasterio.warp import reproject
from rasterio.windows import Window, from_bounds as window_from_bounds

LOG = logging.getLogger("bake-terrain")

GLO30 = "https://copernicus-dem-30m.s3.amazonaws.com"
GLO90 = "https://copernicus-dem-90m.s3.amazonaws.com"
TILE_SIZE = 256
NODATA = -32767.0
# Mercator stops at ±85.0511°; the DEM's polar cells have nothing to give it.
MAX_LAT = 85.0511287798066
ATTRIBUTION = (
    "© DLR e.V. 2010-2014 and © Airbus Defence and Space GmbH 2014-2018 provided under "
    "COPERNICUS by the European Union and ESA; all rights reserved"
)


# --- encoding ------------------------------------------------------------------


def encode_terrarium(metres: np.ndarray) -> bytes:
    """Integer-metre Terrarium, WebP lossless."""
    v = np.clip(np.rint(metres), -32768, 32767).astype(np.int32) + 32768
    rgb = np.zeros(metres.shape + (3,), dtype=np.uint8)
    rgb[..., 0] = (v >> 8) & 0xFF
    rgb[..., 1] = v & 0xFF
    buf = io.BytesIO()
    Image.fromarray(rgb).save(buf, "WEBP", lossless=True, quality=100, method=4)
    return buf.getvalue()


def decode_terrarium(data: bytes) -> np.ndarray:
    rgb = np.asarray(Image.open(io.BytesIO(data)).convert("RGB"), dtype=np.float32)
    return rgb[..., 0] * 256.0 + rgb[..., 1] + rgb[..., 2] / 256.0 - 32768.0


# --- sources ---------------------------------------------------------------------


@dataclass(frozen=True)
class Cell:
    """One 1x1 degree DEM cell, named by its south-west corner."""

    lat: int
    lon: int

    @property
    def key(self) -> str:
        return f"{self.lat},{self.lon}"


def cell_of_name(name: str) -> Cell:
    # Copernicus_DSM_COG_10_N35_00_E135_00_DEM
    parts = name.split("_")
    ns, ew = parts[4], parts[6]
    lat = int(ns[1:]) * (1 if ns[0] == "N" else -1)
    lon = int(ew[1:]) * (1 if ew[0] == "E" else -1)
    return Cell(lat, lon)


def tile_list(base: str) -> list[str]:
    with urllib.request.urlopen(f"{base}/tileList.txt", timeout=120) as resp:
        names = [line.strip() for line in resp.read().decode().splitlines() if line.strip()]
    return names


def build_vrt(workdir: str, bbox: tuple[float, float, float, float] | None) -> tuple[str, list[Cell]]:
    """A VRT over GLO-90 then GLO-30 (later sources win), read in place over HTTP."""
    names30 = tile_list(GLO30)
    names90 = tile_list(GLO90)
    cells30 = {cell_of_name(n): n for n in names30}
    cells90 = {cell_of_name(n): n for n in names90}

    def wanted(c: Cell) -> bool:
        if abs(c.lat) >= 86 or (abs(c.lat + 1) >= 86 and c.lat < 0):
            return False
        if bbox is None:
            return True
        w, s, e, n = bbox
        return c.lon < e and c.lon + 1 > w and c.lat < n and c.lat + 1 > s

    sources: list[str] = []
    for cell, name in cells90.items():
        if cell not in cells30 and wanted(cell):
            sources.append(f"/vsicurl/{GLO90}/{name}/{name}.tif")
    for cell, name in cells30.items():
        if wanted(cell):
            sources.append(f"/vsicurl/{GLO30}/{name}/{name}.tif")
    cells = sorted({c for c in list(cells30) + list(cells90) if wanted(c)}, key=lambda c: (c.lat, c.lon))
    LOG.info("DEM cells: %d (%d at 30 m, %d at 90 m only)", len(cells), len(cells30), len(sources) - len([s for s in sources if GLO30 in s]))

    list_path = os.path.join(workdir, "sources.txt")
    with open(list_path, "w") as f:
        f.write("\n".join(sources) + "\n")
    vrt = os.path.join(workdir, "dem.vrt")
    if not os.path.exists(vrt):
        # -resolution highest: GLO-30's longitude spacing widens with latitude,
        # so the VRT takes the finest and resamples the rest on read.
        cmd = f"gdalbuildvrt -resolution highest -r bilinear -input_file_list {list_path} {vrt}.tmp"
        LOG.info("building VRT over %d sources (this opens every header once)", len(sources))
        if os.system(cmd) != 0:
            raise RuntimeError("gdalbuildvrt failed")
        os.replace(f"{vrt}.tmp", vrt)
    return vrt, cells


# --- tiling ----------------------------------------------------------------------


def cell_tiles(cell: Cell, zoom: int) -> list[mercantile.Tile]:
    """z tiles whose centre falls in `cell`, so neighbouring cells never repeat one."""
    south = max(cell.lat, -MAX_LAT)
    north = min(cell.lat + 1, MAX_LAT)
    if south >= north:
        return []
    out = []
    for t in mercantile.tiles(cell.lon, south, cell.lon + 1, north, zoom):
        b = mercantile.bounds(t)
        clon, clat = (b.west + b.east) / 2, (b.south + b.north) / 2
        if cell.lon <= clon < cell.lon + 1 and cell.lat <= clat < cell.lat + 1:
            out.append(t)
    return out


_src = None


def _worker_init(vrt: str) -> None:
    global _src
    _src = rasterio.open(vrt)


def render_tile(t: mercantile.Tile) -> np.ndarray | None:
    """Elevation (m) for one Web Mercator tile, or None when it is all sea."""
    src = _src
    b = mercantile.bounds(t)
    # Two source pixels of margin so bilinear has neighbours at the tile edge.
    mx, my = 2 * abs(src.transform.a), 2 * abs(src.transform.e)
    win = window_from_bounds(b.west - mx, b.south - my, b.east + mx, b.north + my, src.transform)
    full = Window(0, 0, src.width, src.height)
    try:
        win = win.round_offsets().round_lengths().intersection(full)
    except rasterio.errors.WindowError:
        return None
    data = src.read(1, window=win, out_dtype="float32")
    if data.size == 0:
        return None
    nodata = src.nodata
    if nodata is not None:
        data[data == nodata] = NODATA
    dst = np.full((TILE_SIZE, TILE_SIZE), NODATA, dtype=np.float32)
    xy = mercantile.xy_bounds(t)
    reproject(
        data,
        dst,
        src_transform=src.window_transform(win),
        src_crs="EPSG:4326",
        src_nodata=NODATA,
        dst_transform=from_bounds(xy.left, xy.bottom, xy.right, xy.top, TILE_SIZE, TILE_SIZE),
        dst_crs="EPSG:3857",
        dst_nodata=NODATA,
        resampling=Resampling.bilinear,
    )
    dst[dst == NODATA] = 0.0
    if not np.any(dst > 0.5):
        return None
    return dst


def bake_cell(args: tuple[Cell, int]) -> tuple[str, list[tuple[int, int, int, bytes]]]:
    cell, zoom = args
    out = []
    for t in cell_tiles(cell, zoom):
        for attempt in range(4):
            try:
                metres = render_tile(t)
                break
            except rasterio.errors.RasterioIOError:
                # A transient HTTP failure inside /vsicurl/; back off and retry.
                if attempt == 3:
                    raise
                time.sleep(2 ** attempt)
        if metres is not None:
            out.append((t.z, t.x, t.y, encode_terrarium(metres)))
    return cell.key, out


# --- pyramid ---------------------------------------------------------------------

_stage = None


def _pyramid_init(stage_path: str) -> None:
    global _stage
    _stage = sqlite3.connect(f"file:{stage_path}?mode=ro", uri=True)


def bake_parent(parent: tuple[int, int, int]) -> tuple[int, int, int, bytes] | None:
    z, x, y = parent
    canvas = np.zeros((2 * TILE_SIZE, 2 * TILE_SIZE), dtype=np.float32)
    found = False
    for dy in (0, 1):
        for dx in (0, 1):
            row = _stage.execute(
                "SELECT data FROM tiles WHERE z=? AND x=? AND y=?", (z + 1, 2 * x + dx, 2 * y + dy)
            ).fetchone()
            if row is None:
                continue
            found = True
            canvas[dy * TILE_SIZE : (dy + 1) * TILE_SIZE, dx * TILE_SIZE : (dx + 1) * TILE_SIZE] = decode_terrarium(row[0])
    if not found:
        return None
    metres = canvas.reshape(TILE_SIZE, 2, TILE_SIZE, 2).mean(axis=(1, 3))
    if not np.any(metres > 0.5):
        return None
    return z, x, y, encode_terrarium(metres)


# --- staging + output -------------------------------------------------------------


def open_stage(path: str) -> sqlite3.Connection:
    db = sqlite3.connect(path)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA synchronous=NORMAL")
    db.execute(
        "CREATE TABLE IF NOT EXISTS tiles (tileid INTEGER PRIMARY KEY, z INT, x INT, y INT, data BLOB)"
    )
    db.execute("CREATE INDEX IF NOT EXISTS tiles_zxy ON tiles (z, x, y)")
    db.execute("CREATE TABLE IF NOT EXISTS done (unit TEXT PRIMARY KEY)")
    return db


def insert(db: sqlite3.Connection, rows: list[tuple[int, int, int, bytes]]) -> None:
    db.executemany(
        "INSERT OR REPLACE INTO tiles (tileid, z, x, y, data) VALUES (?, ?, ?, ?, ?)",
        [(zxy_to_tileid(z, x, y), z, x, y, data) for z, x, y, data in rows],
    )


def write_pmtiles(db: sqlite3.Connection, out: str, max_zoom: int, bbox) -> None:
    w, s, e, n = bbox or (-180.0, -MAX_LAT, 180.0, MAX_LAT)
    tmp = out + ".tmp"
    count = 0
    with open(tmp, "wb") as f:
        writer = Writer(f)
        for tileid, data in db.execute("SELECT tileid, data FROM tiles ORDER BY tileid"):
            writer.write_tile(tileid, data)
            count += 1
        writer.finalize(
            {
                "tile_type": TileType.WEBP,
                "tile_compression": Compression.NONE,
                "min_zoom": 0,
                "max_zoom": max_zoom,
                "min_lon_e7": int(w * 1e7),
                "min_lat_e7": int(s * 1e7),
                "max_lon_e7": int(e * 1e7),
                "max_lat_e7": int(n * 1e7),
                "center_zoom": 0,
                "center_lon_e7": int((w + e) / 2 * 1e7),
                "center_lat_e7": int((s + n) / 2 * 1e7),
            },
            {
                "name": "streetcryptid-terrain",
                "encoding": "terrarium",
                "attribution": ATTRIBUTION,
            },
        )
    os.replace(tmp, out)
    LOG.info("wrote %s (%d tiles)", out, count)


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--output", required=True, help="terrain.pmtiles to write")
    p.add_argument("--workdir", required=True, help="scratch for the VRT and the resumable staging DB")
    p.add_argument("--max-zoom", type=int, default=12)
    p.add_argument("--workers", type=int, default=max(1, (os.cpu_count() or 2) - 1))
    p.add_argument("--bbox", help="west,south,east,north (a regional or test bake)")
    args = p.parse_args()

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    if not 0 < args.max_zoom <= 12:
        p.error("--max-zoom must be 1..12 (the server refuses deeper terrain)")
    bbox = tuple(float(v) for v in args.bbox.split(",")) if args.bbox else None
    os.makedirs(args.workdir, exist_ok=True)

    # Remote COGs: never list directories, cache what we read, retry quietly.
    os.environ.setdefault("GDAL_DISABLE_READDIR_ON_OPEN", "EMPTY_DIR")
    os.environ.setdefault("CPL_VSIL_CURL_ALLOWED_EXTENSIONS", ".tif")
    os.environ.setdefault("GDAL_HTTP_MAX_RETRY", "5")
    os.environ.setdefault("GDAL_HTTP_RETRY_DELAY", "2")
    os.environ.setdefault("VSI_CACHE", "TRUE")
    os.environ.setdefault("VSI_CACHE_SIZE", str(256 << 20))
    os.environ.setdefault("GDAL_CACHEMAX", "512")

    vrt, cells = build_vrt(args.workdir, bbox)
    db = open_stage(os.path.join(args.workdir, "stage.db"))
    done = {row[0] for row in db.execute("SELECT unit FROM done")}

    # 1. The finest zoom, one 1-degree cell per task.
    todo = [c for c in cells if f"z{args.max_zoom}:{c.key}" not in done]
    LOG.info("z%d: %d cells to bake (%d already done)", args.max_zoom, len(todo), len(cells) - len(todo))
    started = time.time()
    with mp.Pool(args.workers, initializer=_worker_init, initargs=(vrt,)) as pool:
        for i, (key, rows) in enumerate(pool.imap_unordered(bake_cell, [(c, args.max_zoom) for c in todo]), 1):
            insert(db, rows)
            db.execute("INSERT OR REPLACE INTO done (unit) VALUES (?)", (f"z{args.max_zoom}:{key}",))
            db.commit()
            if i % 50 == 0 or i == len(todo):
                rate = i / max(1e-9, time.time() - started)
                LOG.info("z%d: %d/%d cells (%.2f/s, ~%.1f h left)", args.max_zoom, i, len(todo), rate, (len(todo) - i) / max(rate, 1e-9) / 3600)

    # 2. Every coarser zoom from the one below it.
    for z in range(args.max_zoom - 1, -1, -1):
        unit = f"pyramid:z{z}"
        if unit in done:
            continue
        parents = sorted({(z, x // 2, y // 2) for x, y in db.execute("SELECT x, y FROM tiles WHERE z=?", (z + 1,))})
        LOG.info("z%d: %d tiles from z%d", z, len(parents), z + 1)
        db.execute("DELETE FROM tiles WHERE z=?", (z,))
        with mp.Pool(args.workers, initializer=_pyramid_init, initargs=(os.path.join(args.workdir, "stage.db"),)) as pool:
            batch = []
            for row in pool.imap_unordered(bake_parent, parents, chunksize=64):
                if row is not None:
                    batch.append(row)
                if len(batch) >= 2048:
                    insert(db, batch)
                    db.commit()
                    batch = []
            insert(db, batch)
        db.execute("INSERT OR REPLACE INTO done (unit) VALUES (?)", (unit,))
        db.commit()

    write_pmtiles(db, args.output, args.max_zoom, bbox)
    return 0


if __name__ == "__main__":
    sys.exit(main())
