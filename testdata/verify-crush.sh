#!/usr/bin/env bash
# Сверка Go-порта CRUSH с crushtool --test на 4096 входах для каждого правила:
#   база        firstn, indep, MSR (msr_indep, msr_firstn), классы устройств, set_choose_tries
#   w           reweight группы OSD (0 и дробный)
#   z           CRUSH-вес 0 для целого хоста с пересчётом предков
#   a           два новых хоста, один в новой стойке (crushtool --add-item, классы, теневые деревья)
#   n           новое правило create-replicated-rule с классом
# Нужен docker с образом Ceph v20 (MSR-правила появились в Squid).
set -euo pipefail
IMAGE=${CEPH_IMAGE:-quay.io/ceph/ceph:v20}
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
ceph_run() { docker run --rm -v "$W":/w -w /w --entrypoint bash "$IMAGE" -c "$1"; }

ceph_run 'crushtool --build --num_osds 144 host straw2 12 rack straw2 4 root straw2 0 -o c.bin >/dev/null && crushtool -d c.bin -o c.txt'
python3 - "$W/c.txt" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = re.sub(r'^device (\d+) osd\.(\d+)$', lambda m: f"device {m[1]} osd.{m[2]} class {'ssd' if int(m[1]) % 12 < 2 else 'hdd'}", s, flags=re.M)
s = s.replace('# end crush map', '''rule ec42_host {
	id 1
	type erasure
	step set_chooseleaf_tries 5
	step set_choose_tries 100
	step take root class hdd
	step chooseleaf indep 0 type host
	step emit
}
rule ec_rack_2perrack {
	id 2
	type erasure
	step set_choose_tries 100
	step take root
	step choose indep 3 type rack
	step chooseleaf indep 2 type host
	step emit
}
rule ssd_rack {
	id 3
	type replicated
	step take root class ssd
	step chooseleaf firstn 0 type rack
	step emit
}
rule ec_msr_host {
	id 4
	type msr_indep
	step take root class hdd
	step choosemsr 3 type host
	step choosemsr 2 type osd
	step emit
}
rule rep_msr_rack {
	id 5
	type msr_firstn
	step take root
	step choosemsr 2 type rack
	step choosemsr 2 type host
	step choosemsr 1 type osd
	step emit
}
# end crush map''')
open(p, 'w').write(s)
PY
RULES="0:3 1:6 2:6 3:3 4:6 5:4"
WEIGHTS="5=0,17=0,29=0,41=0,53=0.5,64=0.3,100=0,101=0,102=0,103=0"
CT_W=$(echo "$WEIGHTS" | tr ',' '\n' | sed 's/=/ /' | sed 's/^/--weight /' | tr '\n' ' ')
ZERO=$(seq 36 47 | paste -sd, -) # весь host-03 и его тень в классах

# host-13 (hdd) в существующей rack2, host-14 (ssd) в новой rack5.
ADDS=()
for i in $(seq 144 155); do ADDS+=(-add "$i:1:hdd:host=host-13,rack=rack2"); done
for i in $(seq 156 167); do ADDS+=(-add "$i:1:ssd:host=host-14,rack=rack5,root=root"); done

cat >"$W/ref.sh" <<EOF
set -e
crushtool -c c.txt -o c.bin && crushtool -i c.bin --dump > c.json
test_rules() { # карта суффикс [доп. аргументы]
  for r in $RULES; do rule=\${r%:*}; n=\${r#*:}
    crushtool -i \$1 --test --rule \$rule --num-rep \$n --min-x 0 --max-x 4095 --show-mappings \${@:3} 2>/dev/null > ref\$2-\$rule.txt
  done
}
test_rules c.bin ""
test_rules c.bin w $CT_W
cp c.bin z.bin; for i in \$(echo $ZERO | tr , ' '); do crushtool -i z.bin --reweight-item osd.\$i 0 -o z.bin >/dev/null 2>&1; done
test_rules z.bin z
cp c.bin a.bin
for i in \$(seq 144 155); do crushtool -i a.bin --add-item \$i 1.0 osd.\$i --loc host host-13 --loc rack rack2 -o a.bin >/dev/null 2>&1; done
for i in \$(seq 156 167); do crushtool -i a.bin --add-item \$i 1.0 osd.\$i --loc host host-14 --loc rack rack5 --loc root root -o a.bin >/dev/null 2>&1; done
# --set-subtree-class не считается действием: идёт вместе с --rebuild-class-roots, а он выполняется раньше.
crushtool -i a.bin --set-subtree-class host-13 hdd --set-subtree-class host-14 ssd --rebuild-class-roots -o a.bin >/dev/null 2>&1
crushtool -i a.bin --rebuild-class-roots -o a.bin >/dev/null 2>&1
test_rules a.bin a
crushtool -i c.bin --create-replicated-rule rep_rack_hdd root rack --device-class hdd -o n.bin >/dev/null 2>&1
crushtool -i n.bin --test --rule 6 --num-rep 3 --min-x 0 --max-x 4095 --show-mappings 2>/dev/null > refn-6.txt
EOF
ceph_run 'bash ref.sh'

fail=0
check() { # имя файла-эталона, файл Go
  local d
  d=$(diff "$1" "$2" | grep -c '^>' || true)
  if [ "$d" = 0 ] && [ -s "$1" ]; then echo "  ✔ $3: 4096 x совпали"; else echo "  ✖ $3: $d расхождений"; { diff "$1" "$2" | head -4; } || true; fail=1; fi
}
go build -o "$W/crushtest" ./cmd/crushtest
for r in $RULES; do
  rule=${r%:*}; n=${r#*:}
  ct() { "$W/crushtest" -crush "$W/c.json" -rule $rule -num-rep $n -max-x 4095 "$@"; }
  ct > "$W/go-$rule.txt";                      check "$W/ref-$rule.txt" "$W/go-$rule.txt" "rule $rule база"
  ct -weight "$WEIGHTS" > "$W/gow-$rule.txt";  check "$W/refw-$rule.txt" "$W/gow-$rule.txt" "rule $rule reweight"
  ct -crush-zero "$ZERO" > "$W/goz-$rule.txt"; check "$W/refz-$rule.txt" "$W/goz-$rule.txt" "rule $rule crush-вес 0"
  ct "${ADDS[@]}" > "$W/goa-$rule.txt";        check "$W/refa-$rule.txt" "$W/goa-$rule.txt" "rule $rule +2 хоста"
done
"$W/crushtest" -crush "$W/c.json" -num-rep 3 -max-x 4095 -new-rule rep_rack_hdd:replicated:root:rack:hdd > "$W/gon-6.txt"
check "$W/refn-6.txt" "$W/gon-6.txt" "новое правило rep_rack_hdd"
exit $fail
