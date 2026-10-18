"""Assert a bake carries OSM land cover below z7.

Reads the tile covering --lat/--lon at each zoom from a PMTiles archive and
checks its `landcover` layer: OSM classes (wood, grass, farmland, ...) must be
absent at z4, where only Natural Earth glaciers belong, and present at z6 and z7.
The stock OpenMapTiles profile fails the z6 check. z5 is reported but not
required: it starts the OSM range too, but at 2 px a z5 feature must be several
kilometres across, and a small extract's forests may all be finer than that.

    python check_landcover.py out.pmtiles --lat 49.8 --lon 6.1 --class wood
"""

import argparse
import gzip
import math
import sys

import mapbox_vector_tile
from pmtiles.reader import MmapSource, Reader


def tile_of(lat: float, lon: float, z: int) -> tuple[int, int]:
    n = 2**z
    x = int((lon + 180.0) / 360.0 * n)
    r = math.radians(lat)
    y = int((1.0 - math.log(math.tan(r) + 1.0 / math.cos(r)) / math.pi) / 2.0 * n)
    return x, y


def landcover_classes(reader: Reader, z: int, x: int, y: int) -> dict[str, int]:
    data = reader.get(z, x, y)
    if not data:
        return {}
    if data[:2] == b"\x1f\x8b":
        data = gzip.decompress(data)
    layer = mapbox_vector_tile.decode(data).get("landcover")
    counts: dict[str, int] = {}
    for feature in (layer or {}).get("features", []):
        cls = str(feature["properties"].get("class", ""))
        counts[cls] = counts.get(cls, 0) + 1
    return counts


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("archive")
    ap.add_argument("--lat", type=float, required=True)
    ap.add_argument("--lon", type=float, required=True)
    ap.add_argument("--class", dest="cls", default="wood")
    args = ap.parse_args()

    failures = []
    with open(args.archive, "rb") as f:
        reader = Reader(MmapSource(f))
        for z in (4, 5, 6, 7):
            x, y = tile_of(args.lat, args.lon, z)
            counts = landcover_classes(reader, z, x, y)
            print(f"z{z}/{x}/{y} landcover {counts}")
            has = counts.get(args.cls, 0) > 0
            if z == 4 and has:
                failures.append(f"z4 carries OSM {args.cls}; land cover should start at z5")
            if z >= 6 and not has:
                failures.append(f"z{z} has no {args.cls}; land cover should start at z5")
    for failure in failures:
        print("FAIL:", failure)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
