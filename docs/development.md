# Разработка

Go 1.22, один модуль `cephsim`, без внешних зависимостей. Около 5 500 строк.

## Устройство кода

| Файл | Что |
|------|-----|
| `main.go` | CLI: `collect`, `verify`, `calibrate`, `simulate`, разбор флагов, сбор `meta.json` и mClock |
| `snapshot.go` | загрузка снимка: пулы, дерево, `osd df`, `pg ls`, профили EC, `meta.json` |
| `placement.go` | логика OSDMap поверх CRUSH: pps, reweight, up/exists, `pg_upmap`, `pg_upmap_items`, `MapAll` |
| `change.go` | подготовка состояния «после» по сценарию: разбор `-add` и `-new-rule`, `expand`, смена правила и `pg_num`, доли раскола PG (`splitShares`) |
| `sim.go` | `Analyze` (доступность, переезд, заполненность, PG на OSD), `recoveryTime`, `Finalize`, вердикт (`assess`), план, `Variants` |
| `rolling.go` | расписание обслуживания: партии, блокирующие хосты, отчёт `PrintRolling`, общий вывод вердикта |
| `mclock.go` | профили mClock, доля клиентов и recovery |
| `calibrate.go` | запросы к Prometheus, калибровка слота по OSD и по пулам, предел объектов, клиентская нагрузка |
| `report.go` | текстовый отчёт |
| `html.go` | HTML-отчёты: шаблоны сценария и карты рисков, дерево кластера, SVG-графики заполненности, хода переезда и расписания |
| `cluster.go` | карта рисков: отказ, обслуживание и «сейчас» для каждого домена, запас по месту, пулы, главные риски |
| `osdmaptool.go` | обёртка над `osdmaptool` и `crushtool` (локально или в docker), разбор вывода; нужна только `verify` |
| `crush/crush.go` | порт `mapper.c`: straw2, `crush_ln`, firstn, indep, `do_rule`, choose_args, `SetItemWeight` |
| `crush/msr.go` | порт `crush_msr_do_rule` (правила `msr_firstn`, `msr_indep`) |
| `crush/edit.go` | правка карты как в CrushWrapper: `InsertDevice`, `RebuildShadows`, `AddRule`, описание правил |
| `crush/hash.go`, `crush/ln_table.go` | rjenkins1 и таблицы `crush_ln` (сгенерированы из `crush_ln_table.h`) |
| `cmd/crushtest` | печать раскладки в формате `crushtool --test --show-mappings`, с reweight, CRUSH-весом 0, `-add`, `-new-rule` |
| `cmd/genfixture` | синтетический снимок из osdmap, собранного `crushtool --build` и `osdmaptool --createsimple` |

Поток данных в `simulate`:

```
LoadSnapshot → prepare (копия Placement/CRUSH/пулов + изменение сценария)
             → Analyze: доступность по pg ls, MapAll до и после, куски данных → OSDProj
             → Finalize: время, заполненность, клиенты, вердикт, план
             → PrintReport | JSON | WriteHTML(reportData)
rolling: Rolling → Analyze(maintenance, noout) по хостам и партиям → PrintRolling | WriteHTML(rollingData)
report:  BuildCluster → для каждого домена Analyze(maintenance) ×2 и Analyze+Finalize(failure), Rolling → WriteClusterHTML
         раскладка снимка считается один раз (Snapshot.baseMapping)
```

Источник истины для портов - исходники Ceph, github.com/ceph/ceph (`src/crush/mapper.c`, `src/crush/CrushWrapper.cc`, `src/osd/OSDMap.cc`, `src/common/mclock_common.h`). Правки в портах сверяйте с ними и проверяйте `verify-crush.sh`.

## Тесты

```bash
go test ./...                                                  # юнит-тесты, секунды
CEPH_IMAGE=quay.io/ceph/ceph:v20 ./testdata/verify-crush.sh    # порт CRUSH против crushtool, ~2 мин, нужен docker
./cephsim verify -snap snap-prod                               # раскладка реального снимка против osdmaptool и pg ls
```

| Тест | Что проверяет |
|------|---------------|
| `TestAvailabilityHostDown` | degraded, inactive, incomplete, без запаса на EC 2+2 и rep 3 |
| `TestDrainKeepsAvailability` | drain не теряет копий |
| `TestParseMapping` | разбор вывода `osdmaptool`, в том числе up из `-dump-all` |
| `TestSplitShares` | доли раскола и слияния PG, в том числе не степени двойки |
| `TestExpandFillsNewHosts` | expand на синтетике: id новых OSD, расположение, заполненность новых ≈ старых |
| `TestRuleChangeToRack` | смена правила на rack: без undersized; на root при size 3 - все undersized |
| `TestPGNumKeepsBytes` | pgnum: PG на OSD почти удваиваются, переезд не больше трёх копий |
| `TestRollingBatchesByRack` | rolling: при failure domain rack - партии по стойкам, `-parallel` |
| `TestMSRRuleSpec` | разбор `-new-rule msr`, запрет MSR для replicated |
| `TestCalibrate*`, `TestPoolClasses` | калибровка по фейковому Prometheus, по пулам, классы пулов по CRUSH-правилам |
| `TestObjectLimitDominates` | предел по объектам перекрывает байтовый |
| `TestClientCapProfiles`, `TestMClockLimitsSmallObjectsOnHDD` | доли mClock по профилям, цена мелких объектов на HDD |
| `TestRebuildShadowsIdentity` | перестройка теневых деревьев на неизменённой карте даёт ту же карту |
| `TestHTMLReports` | HTML-отчёт собирается для каждого сценария и для rolling, со всеми разделами |
| `TestClusterReport` | карта рисков на синтетике: потерю хоста кластер переживает, стойки - нет; 12 партий; все разделы HTML |

Тесты, которым нужен `testdata/snap-synthetic`, пропускаются, если снимка нет. `testdata/crush-classes.json` - синтетическая карта с классами hdd и ssd (собрана crushtool v20) для проверки теневых деревьев.

## Фикстуры

```bash
CEPH_IMAGE=quay.io/ceph/ceph:v20 ./testdata/make-fixture.sh   # testdata/snap-synthetic
```

Синтетический кластер: 3 стойки × 4 хоста × 12 OSD (имена `host-01`…`host-12`, `rack1`…`rack3`), один пул `rbd` rep 3, 4 608 PG. CRUSH и osdmap собраны настоящими `crushtool` и `osdmaptool`, объёмы PG случайные. CRUSH-вес OSD - 1,0 при размере 16 TB, поэтому в тестах expand задаётся `weight=1`.

## Как добавить сценарий

1. Константа `Act…` в `sim.go`; если сценарий теряет копии - в `degrading`, если нужен `-target` - в `withTargets`.
2. Изменение состояния - ветка в `prepare` (`change.go`): правка `ch.after` (раскладка, CRUSH) и/или `ch.pools`.
3. Если нужны особые предупреждения - в `assess`, план - в `plan`.
4. Заголовок сценария - в `actionTitle` (`report.go`), флаги - в `cmdSimulate`, справка - в `usage`.
5. Тест в `scenarios_test.go` на `testdata/snap-synthetic`; описание - в `docs/scenarios.md`.

Если сценарий правит CRUSH-карту, добавьте сверку в `testdata/verify-crush.sh`: сделайте ту же правку через `crushtool` и сравните раскладку через `cmd/crushtest`.
