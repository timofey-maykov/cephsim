#!/usr/bin/env bash
# Синтетический кластер: 3 стойки × 4 хоста × 12 OSD, пул rep 3 по host.
# CRUSH и osdmap собираются настоящими crushtool/osdmaptool из образа Ceph.
set -euo pipefail
IMAGE=${CEPH_IMAGE:-quay.io/ceph/ceph:v20}
OUT=${1:-testdata/snap-synthetic}
RAW=$(mktemp -d)
trap 'rm -rf "$RAW"' EXIT

ceph_run() { docker run --rm -v "$RAW":/w -w /w --entrypoint bash "$IMAGE" -c "$1"; }

ceph_run 'crushtool --build --num_osds 144 host straw2 12 rack straw2 4 root straw2 0 -o crush.bin && crushtool -d crush.bin -o crush.txt'
# host0 → host-01, rack0 → rack1
python3 - "$RAW/crush.txt" <<'EOF'
import re, sys
p = sys.argv[1]
s = open(p).read()
s = re.sub(r'\b(host|rack)(\d+)\b', lambda m: f"{m[1]}-{int(m[2])+1:02d}" if m[1] == 'host' else f"rack{int(m[2])+1}", s)
open(p, 'w').write(s)
EOF
ceph_run '
crushtool -c crush.txt -o crush.bin
crushtool -i crush.bin --dump > crush.json
osdmaptool --createsimple 144 --pg-bits 5 --with-default-pool --clobber osdmap.bin 2>/dev/null
osdmaptool osdmap.bin --import-crush crush.bin 2>/dev/null
osdmaptool osdmap.bin --dump json 2>/dev/null > dump.json
osdmaptool osdmap.bin --tree json 2>/dev/null > tree.json
osdmaptool osdmap.bin --mark-up-in --test-map-pgs-dump 2>/dev/null > mapping.txt'

mkdir -p "$OUT"
cp "$RAW/osdmap.bin" "$RAW/crush.json" "$OUT/"
go run ./cmd/genfixture -raw "$RAW" -out "$OUT"
