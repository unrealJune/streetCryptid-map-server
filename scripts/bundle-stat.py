#!/usr/bin/env python3
"""Measure an SCB2 or SCB3 bundle stream: stage sizes, per-tile distribution,
and raw MVT bytes by layer. Standard library only.

    python3 -I scripts/bundle-stat.py <file.scb2|file.scb3>
    curl -sS https://HOST/planet/bundle/v2/164/357/14 -o /tmp/s.scb2 && python3 -I scripts/bundle-stat.py /tmp/s.scb2

Use it before and after a format change to prove the byte savings are real.
"""
import collections
import gzip
import statistics
import struct
import sys
import zlib

EMPTY = 0xFFFFFFFF
FLAG_GZIP_ENTRIES = 0x01


def varint(b, i):
    r = s = 0
    while True:
        c = b[i]
        i += 1
        r |= (c & 0x7F) << s
        s += 7
        if c < 0x80:
            return r, i


def pb_fields(b):
    i = 0
    while i < len(b):
        k, i = varint(b, i)
        f, wt = k >> 3, k & 7
        if wt == 0:
            _, i = varint(b, i)
            yield f, None
        elif wt == 2:
            n, i = varint(b, i)
            yield f, b[i : i + n]
            i += n
        elif wt == 5:
            i += 4
            yield f, None
        elif wt == 1:
            i += 8
            yield f, None
        else:
            raise ValueError(f"bad wire type {wt}")


def layer_bytes(tile):
    out = collections.Counter()
    for f, v in pb_fields(tile):
        if f == 3 and v is not None:
            name = next(vv for ff, vv in pb_fields(v) if ff == 1).decode()
            out[name] += len(v)
    return out


def split_scb1(raw):
    assert raw[:4] == b"SCB1", "not SCB1"
    flags = raw[7]
    z = raw[6]
    n = struct.unpack(">I", raw[16:20])[0]
    tiles = []
    p = 20
    for _ in range(n):
        L = struct.unpack(">I", raw[p : p + 4])[0]
        p += 4
        if L == EMPTY:
            tiles.append(None)
            continue
        tiles.append(raw[p : p + L])
        p += L
    assert p == len(raw), "trailing bytes"
    return z, flags, tiles


def report_stage(label, wire_len, raw, flags, tiles):
    nonempty = [t for t in tiles if t]
    if flags & FLAG_GZIP_ENTRIES:
        inflated = [gzip.decompress(t) for t in nonempty]
    else:
        inflated = nonempty
    raw_total = sum(len(t) for t in inflated)
    sizes = sorted(len(t) for t in inflated) or [0]
    print(f"\n=== {label}: {len(tiles)} tiles, {len(nonempty)} non-empty, "
          f"wire {wire_len/1e6:.2f} MB, inflated MVT {raw_total/1e6:.2f} MB")
    print(f"    inflated tile sizes: median {statistics.median(sizes)/1e3:.0f} KB, "
          f"p90 {sizes[int(len(sizes)*.9)]/1e3:.0f} KB, max {sizes[-1]/1e3:.0f} KB")
    lb = collections.Counter()
    for t in inflated:
        lb.update(layer_bytes(t))
    tot = sum(lb.values()) or 1
    print("    bytes by layer:", ", ".join(f"{k} {v/tot:.0%}" for k, v in lb.most_common()))
    if not (flags & FLAG_GZIP_ENTRIES):
        per_tile = sum(len(zlib.compress(t, 6)) for t in inflated)
        print(f"    (reference) per-tile gzip -6 would be {per_tile/1e6:.2f} MB")


def main(path):
    data = open(path, "rb").read()
    magic = data[:4]
    if magic == b"SCB2":
        nstages = struct.unpack(">I", data[16:20])[0]
        off = 20
        for _ in range(nstages):
            clen, rawlen = struct.unpack(">II", data[off : off + 8])
            gz = data[off + 40 : off + 40 + clen]
            off += 40 + clen
            raw = gzip.decompress(gz)
            assert len(raw) == rawlen
            z, flags, tiles = split_scb1(raw)
            report_stage(f"SCB2 stage z{z}", clen, raw, flags, tiles)
    elif magic == b"SCB3":
        nstages = struct.unpack(">I", data[16:20])[0]
        off = 20
        for _ in range(nstages):
            plen = struct.unpack(">I", data[off : off + 4])[0]
            zoom, part = data[off + 4], data[off + 5]
            payload = data[off + 40 : off + 40 + plen]
            off += 40 + plen
            z, flags, tiles = split_scb1(payload)
            name = {0: "full", 1: "structure", 2: "labels"}.get(part, str(part))
            report_stage(f"SCB3 stage z{zoom} {name}", plen, payload, flags, tiles)
    else:
        sys.exit(f"unknown magic {magic!r}")
    assert off == len(data), "trailing stream bytes"
    print(f"\ntotal stream: {len(data)/1e6:.2f} MB")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(sys.argv[1])
